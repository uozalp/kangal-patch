/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/drain"
	"github.com/uozalp/kangal-patch/internal/patchutil"
	"github.com/uozalp/kangal-patch/internal/talos"
)

// PatchJobReconciler reconciles a PatchJob object
type PatchJobReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Namespace string
}

// Requeue intervals for different phases
const (
	requeueForDrainCheck   = 10 * time.Second // checking if drain is complete
	requeueForUpgradeStart = 5 * time.Second  // starting upgrade operation
	requeueForRebootCheck  = 60 * time.Second // checking if node is back online
)

// +kubebuilder:rbac:groups=kangalpatch.ozalp.dk,resources=patchjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kangalpatch.ozalp.dk,resources=patchjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *PatchJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the PatchJob
	var patchJob patchv1alpha1.PatchJob
	if err := r.Get(ctx, req.NamespacedName, &patchJob); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch PatchJob")
		return ctrl.Result{}, err
	}

	// Terminal phases - nothing more to do
	if patchJob.Status.Phase.IsTerminal() {
		return ctrl.Result{}, nil
	}

	switch patchJob.Status.Phase {
	case "":
		return r.initJob(ctx, &patchJob)

	case patchv1alpha1.PatchJobPhasePending:
		return r.startDrain(ctx, &patchJob)

	case patchv1alpha1.PatchJobPhaseDraining:
		return r.waitForDrain(ctx, &patchJob)

	case patchv1alpha1.PatchJobPhaseUpgrading:
		return r.startUpgrade(ctx, &patchJob)

	case patchv1alpha1.PatchJobPhaseRebooting:
		return r.waitForReboot(ctx, &patchJob)

	}

	return ctrl.Result{}, nil
}

func (r *PatchJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&patchv1alpha1.PatchJob{}).
		Complete(r)
}

// initJob initializes a new PatchJob with pending status
func (r *PatchJobReconciler) initJob(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	original := patchJob.DeepCopy()

	// Get Talos config from parent PatchPlan
	talosConfig, err := r.getTalosConfig(ctx, patchJob)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to get Talos config", err)
	}

	// Create Talos client
	talosClient, err := talos.NewClient(talosConfig)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to create Talos client", err)
	}
	defer talosClient.Close()

	// Get current version from node
	currentVersion, err := talosClient.GetVersion(ctx, patchJob.Spec.NodeName)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to get current version", err)
	}

	targetVersion := patchJob.Spec.Target.Version

	// Check if already at target version
	if currentVersion == targetVersion {
		// Ensure node is uncordoned in case it was cordoned from a previous run
		drainer := drain.NewDrainer(r.Client)
		if err := drainer.UncordonNode(ctx, patchJob.Spec.NodeName); err != nil {
			logger.Error(err, "failed to uncordon node", "node", patchJob.Spec.NodeName)
			// Continue anyway - node is at target version
		}

		// Delete the lease since no patching is needed
		if err := r.deletePatchJobLease(ctx, patchJob); err != nil {
			logger.Error(err, "failed to delete lease", "node", patchJob.Spec.NodeName)
			// Continue anyway - node is at target version
		}

		patchJob.Status.Phase = patchv1alpha1.PatchJobPhaseCompleted
		patchJob.Status.CurrentVersion = currentVersion
		patchJob.Status.TargetVersion = targetVersion
		patchJob.Status.Message = "already at target version"

		if err := r.patchStatus(ctx, original, patchJob); err != nil {
			logger.Error(err, "failed to update status")
			return ctrl.Result{}, err
		}

		logger.Info("node already at target version",
			"node", patchJob.Spec.NodeName,
			"version", currentVersion)

		return ctrl.Result{}, nil
	}

	// Update status with initialization complete
	patchJob.Status.Phase = patchv1alpha1.PatchJobPhasePending
	patchJob.Status.CurrentVersion = currentVersion
	patchJob.Status.TargetVersion = targetVersion
	patchJob.Status.Message = "initialized, ready to drain"

	if err := r.patchStatus(ctx, original, patchJob); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("job initialized",
		"node", patchJob.Spec.NodeName,
		"currentVersion", currentVersion,
		"targetVersion", targetVersion)

	return ctrl.Result{Requeue: true}, nil
}

// failJob transitions a PatchJob to failed state and updates status
func (r *PatchJobReconciler) failJob(ctx context.Context, original, modified *patchv1alpha1.PatchJob, message string, err error) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Error(err, message)

	modified.Status.Phase = patchv1alpha1.PatchJobPhaseFailed
	modified.Status.Message = fmt.Sprintf("%s: %v", message, err)

	if statusErr := r.patchStatus(ctx, original, modified); statusErr != nil {
		logger.Error(statusErr, "failed to update status after failure")
	}

	return ctrl.Result{}, err
}

// patchStatus applies a status patch to the PatchJob, only updating changed fields.
func (r *PatchJobReconciler) patchStatus(ctx context.Context, original, modified *patchv1alpha1.PatchJob) error {
	return patchutil.PatchStatus(ctx, r.Status(), original, modified)
}

// getTalosConfig retrieves Talos configuration from the parent PatchPlan.
func (r *PatchJobReconciler) getTalosConfig(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (*patchv1alpha1.TalosConfig, error) {
	if patchJob.Spec.PatchPlanRef == "" {
		return nil, fmt.Errorf("patchPlanRef not set")
	}

	var patchPlan patchv1alpha1.PatchPlan
	if err := r.Get(ctx, types.NamespacedName{Name: patchJob.Spec.PatchPlanRef}, &patchPlan); err != nil {
		return nil, fmt.Errorf("failed to get PatchPlan %s: %w", patchJob.Spec.PatchPlanRef, err)
	}

	talosConfig := &patchPlan.Spec.TalosConfig

	// Resolve secret reference if present
	if talosConfig.SecretRef != nil {
		return r.resolveSecretRef(ctx, talosConfig)
	}

	return talosConfig, nil
}

// resolveSecretRef retrieves certificate data from a Kubernetes secret.
func (r *PatchJobReconciler) resolveSecretRef(ctx context.Context, talosConfig *patchv1alpha1.TalosConfig) (*patchv1alpha1.TalosConfig, error) {
	secretRef := talosConfig.SecretRef
	if secretRef.Namespace == "" {
		return nil, fmt.Errorf("secretRef.namespace must be specified")
	}

	var secret corev1.Secret
	secretKey := types.NamespacedName{
		Name:      secretRef.Name,
		Namespace: secretRef.Namespace,
	}
	if err := r.Get(ctx, secretKey, &secret); err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", secretRef.Namespace, secretRef.Name, err)
	}

	// Validate required keys
	requiredKeys := []string{"ca.crt", "tls.crt", "tls.key"}
	for _, key := range requiredKeys {
		if _, ok := secret.Data[key]; !ok {
			return nil, fmt.Errorf("secret %s/%s missing required key: %s", secretRef.Namespace, secretRef.Name, key)
		}
	}

	return &patchv1alpha1.TalosConfig{
		Endpoints:  talosConfig.Endpoints,
		CACert:     string(secret.Data["ca.crt"]),
		ClientCert: string(secret.Data["tls.crt"]),
		ClientKey:  string(secret.Data["tls.key"]),
	}, nil
}

// startDrain cordons the node and initiates the drain process
func (r *PatchJobReconciler) startDrain(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	original := patchJob.DeepCopy()

	// Get PatchPlan to retrieve drain settings
	var patchPlan patchv1alpha1.PatchPlan
	if err := r.Get(ctx, types.NamespacedName{Name: patchJob.Spec.PatchPlanRef}, &patchPlan); err != nil {
		return r.failJob(ctx, original, patchJob, "failed to get PatchPlan", err)
	}

	// Create drainer
	drainer := drain.NewDrainer(r.Client)

	// Cordon the node first
	if err := drainer.CordonNode(ctx, patchJob.Spec.NodeName); err != nil {
		return r.failJob(ctx, original, patchJob, "failed to cordon node", err)
	}

	logger.Info("node cordoned", "node", patchJob.Spec.NodeName)

	// Start drain operation
	drainOpts := drain.DrainOptions{
		RespectPDBs: patchPlan.Spec.RespectPDBs,
		Timeout:     patchPlan.Spec.DrainTimeout.Duration,
	}

	if err := drainer.DrainNode(ctx, patchJob.Spec.NodeName, drainOpts); err != nil {
		return r.failJob(ctx, original, patchJob, "failed to drain node", err)
	}

	// Update status to draining phase
	patchJob.Status.Phase = patchv1alpha1.PatchJobPhaseDraining
	patchJob.Status.Message = "draining node"

	if err := r.patchStatus(ctx, original, patchJob); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("drain initiated", "node", patchJob.Spec.NodeName)

	return ctrl.Result{RequeueAfter: requeueForDrainCheck}, nil
}

// waitForDrain checks if the node drain is complete and transitions to upgrade phase
func (r *PatchJobReconciler) waitForDrain(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	original := patchJob.DeepCopy()

	drainer := drain.NewDrainer(r.Client)

	// Check if drain is complete
	drained, err := drainer.IsDrained(ctx, patchJob.Spec.NodeName)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to check drain status", err)
	}

	if !drained {
		logger.Info("node still draining", "node", patchJob.Spec.NodeName)
		return ctrl.Result{RequeueAfter: requeueForDrainCheck}, nil
	}

	// Drain complete, move to upgrade phase
	patchJob.Status.Phase = patchv1alpha1.PatchJobPhaseUpgrading
	patchJob.Status.Message = "drain complete, ready to upgrade"

	if err := r.patchStatus(ctx, original, patchJob); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("node drained successfully", "node", patchJob.Spec.NodeName)

	return ctrl.Result{RequeueAfter: requeueForUpgradeStart}, nil
}

// startUpgrade initiates the Talos upgrade operation
func (r *PatchJobReconciler) startUpgrade(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	original := patchJob.DeepCopy()

	// Get Talos config
	talosConfig, err := r.getTalosConfig(ctx, patchJob)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to get Talos config", err)
	}

	// Create Talos client
	talosClient, err := talos.NewClient(talosConfig)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to create Talos client", err)
	}
	defer talosClient.Close()

	// Build the installer image URL
	installerImage, err := patchutil.BuildInstallerImage(patchJob.Spec.Target)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to build installer image", err)
	}

	// Initiate upgrade
	if err := talosClient.Upgrade(ctx, patchJob.Spec.NodeName, installerImage); err != nil {
		return r.failJob(ctx, original, patchJob, "failed to start upgrade", err)
	}

	// Update status to rebooting phase
	patchJob.Status.Phase = patchv1alpha1.PatchJobPhaseRebooting
	patchJob.Status.Message = "upgrade started, waiting for reboot"

	if err := r.patchStatus(ctx, original, patchJob); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("upgrade initiated",
		"node", patchJob.Spec.NodeName,
		"targetVersion", patchJob.Status.TargetVersion)

	return ctrl.Result{RequeueAfter: requeueForRebootCheck}, nil
}

// waitForReboot checks if the node has rebooted and become responsive, then validates the upgrade
func (r *PatchJobReconciler) waitForReboot(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	original := patchJob.DeepCopy()

	// Get Talos config
	talosConfig, err := r.getTalosConfig(ctx, patchJob)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to get Talos config", err)
	}

	// Create Talos client
	talosClient, err := talos.NewClient(talosConfig)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to create Talos client", err)
	}
	defer talosClient.Close()

	// Try to get version - this checks both responsiveness and upgrade success
	currentVersion, err := talosClient.GetVersion(ctx, patchJob.Spec.NodeName)
	if err != nil {
		// Node not responsive yet, keep waiting
		logger.Info("waiting for node to become responsive", "node", patchJob.Spec.NodeName)
		return ctrl.Result{RequeueAfter: requeueForRebootCheck}, nil
	}

	// Check if upgrade succeeded
	if currentVersion != patchJob.Status.TargetVersion {
		logger.Info("node responsive but upgrade not complete yet",
			"node", patchJob.Spec.NodeName,
			"currentVersion", currentVersion,
			"targetVersion", patchJob.Status.TargetVersion)
		return ctrl.Result{RequeueAfter: requeueForRebootCheck}, nil
	}

	// Uncordon the node
	drainer := drain.NewDrainer(r.Client)
	if err := drainer.UncordonNode(ctx, patchJob.Spec.NodeName); err != nil {
		return r.failJob(ctx, original, patchJob, "failed to uncordon node", err)
	}

	// Move directly to completed phase
	patchJob.Status.Phase = patchv1alpha1.PatchJobPhaseCompleted
	patchJob.Status.Message = "upgrade completed successfully"

	if err := r.patchStatus(ctx, original, patchJob); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("upgrade completed successfully",
		"node", patchJob.Spec.NodeName,
		"version", currentVersion)

	return ctrl.Result{}, nil
}

// deletePatchJobLease deletes the scheduling lease associated with this PatchJob.
func (r *PatchJobReconciler) deletePatchJobLease(ctx context.Context, patchJob *patchv1alpha1.PatchJob) error {
	logger := log.FromContext(ctx)

	planName := patchJob.Spec.PatchPlanRef
	if planName == "" {
		return fmt.Errorf("patchPlanRef not set")
	}

	var patchPlan patchv1alpha1.PatchPlan
	if err := r.Get(ctx, types.NamespacedName{Name: planName}, &patchPlan); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get PatchPlan %s: %w", planName, err)
	}

	leaseName := fmt.Sprintf("%s-%s-scheduling", planName, patchJob.Spec.NodeName)

	lease := &coordinationv1.Lease{}
	leaseKey := types.NamespacedName{
		Name:      leaseName,
		Namespace: r.Namespace,
	}

	if err := r.Get(ctx, leaseKey, lease); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get lease %s: %w", leaseName, err)
	}

	if err := r.Delete(ctx, lease); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete lease %s: %w", leaseName, err)
	}

	logger.Info("deleted lease for patch job", "lease", leaseName, "node", patchJob.Spec.NodeName)
	return nil
}
