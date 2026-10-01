package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/drain"
	"github.com/uozalp/kangal-patch/internal/nodeutil"
	"github.com/uozalp/kangal-patch/internal/patchutil"
	"github.com/uozalp/kangal-patch/internal/talos"
)

// PatchJobReconciler reconciles a PatchJob object
type PatchJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Requeue intervals for different phases
const (
	requeueImmediate            = 1 * time.Millisecond // Requeue is deprecated in favor of a minimal RequeueAfter
	requeueForDrainCheck        = 10 * time.Second     // checking if drain is complete
	requeueForUpgradeStart      = 5 * time.Second      // starting upgrade operation
	requeueForRebootCheck       = 60 * time.Second     // checking if node is back online
	requeueForKubernetesUpgrade = 5 * time.Second      // starting the kubernetes upgrade patch
	requeueForKubernetesCheck   = 15 * time.Second     // checking if kubelet/static pods report the new version
)

// +kubebuilder:rbac:groups=kangalpatch.ozalp.dk,resources=patchjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kangalpatch.ozalp.dk,resources=patchjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Well-known Talos static pod labels/namespace for control plane component readiness checks.
const (
	kubeSystemNamespace   = "kube-system"
	controlPlaneComponent = "tier=control-plane"
)

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

	case patchv1alpha1.PatchJobPhaseUpgradingKubernetes:
		return r.startKubernetesUpgrade(ctx, &patchJob)

	case patchv1alpha1.PatchJobPhaseValidatingKubernetes:
		return r.waitForKubernetesUpgrade(ctx, &patchJob)

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
	defer func() {
		if cerr := talosClient.Close(); cerr != nil {
			logger.Error(cerr, "failed to close Talos client")
		}
	}()

	// Get current Talos and Kubernetes versions from the node
	nodeAddr, err := r.getNodeAddress(ctx, patchJob.Spec.NodeName)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to resolve node address", err)
	}

	currentTalosVersion, err := talosClient.GetVersion(ctx, nodeAddr)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to get current Talos version", err)
	}

	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: patchJob.Spec.NodeName}, &node); err != nil {
		return r.failJob(ctx, original, patchJob, "failed to get node", err)
	}
	currentKubernetesVersion := node.Status.NodeInfo.KubeletVersion

	targetTalosVersion := patchJob.Spec.Target.TalosVersion
	targetKubernetesVersion := patchJob.Spec.Target.KubernetesVersion

	talosNeedsUpgrade := targetTalosVersion != "" && currentTalosVersion != targetTalosVersion
	kubernetesNeedsUpgrade := targetKubernetesVersion != "" && currentKubernetesVersion != targetKubernetesVersion

	patchJob.Status.CurrentTalosVersion = currentTalosVersion
	patchJob.Status.CurrentKubernetesVersion = currentKubernetesVersion
	// Display the requested target, or the current value if that dimension isn't being changed
	patchJob.Status.TargetTalosVersion = firstNonEmpty(targetTalosVersion, currentTalosVersion)
	patchJob.Status.TargetKubernetesVersion = firstNonEmpty(targetKubernetesVersion, currentKubernetesVersion)

	// Check if already at target version(s)
	if !talosNeedsUpgrade && !kubernetesNeedsUpgrade {
		// Ensure node is uncordoned in case it was cordoned from a previous run
		drainer := drain.NewDrainer(r.Client)
		if err := drainer.UncordonNode(ctx, patchJob.Spec.NodeName); err != nil {
			logger.Error(err, "failed to uncordon node", "node", patchJob.Spec.NodeName)
			// Continue anyway - node is at target version
		}

		patchJob.Status.Phase = patchv1alpha1.PatchJobPhaseCompleted
		patchJob.Status.Message = "already at target version"

		if err := r.patchStatus(ctx, original, patchJob); err != nil {
			logger.Error(err, "failed to update status")
			return ctrl.Result{}, err
		}

		logger.Info("node already at target version",
			"node", patchJob.Spec.NodeName,
			"talosVersion", currentTalosVersion,
			"kubernetesVersion", currentKubernetesVersion)

		return ctrl.Result{}, nil
	}

	// A Talos OS upgrade requires cordon/drain/reboot; a Kubernetes-only upgrade patches the
	// kubelet (and control plane static pods) in place without disrupting the node.
	if talosNeedsUpgrade {
		patchJob.Status.Phase = patchv1alpha1.PatchJobPhasePending
		patchJob.Status.Message = "initialized, ready to drain"
	} else {
		patchJob.Status.Phase = patchv1alpha1.PatchJobPhaseUpgradingKubernetes
		patchJob.Status.Message = "initialized, ready to upgrade kubernetes"
	}

	if err := r.patchStatus(ctx, original, patchJob); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("job initialized",
		"node", patchJob.Spec.NodeName,
		"currentTalosVersion", currentTalosVersion,
		"targetTalosVersion", targetTalosVersion,
		"currentKubernetesVersion", currentKubernetesVersion,
		"targetKubernetesVersion", targetKubernetesVersion)

	return ctrl.Result{RequeueAfter: requeueImmediate}, nil
}

// firstNonEmpty returns a if it is non-empty, otherwise b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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

// getNodeAddress resolves a Kubernetes node name to its InternalIP for reaching the Talos API.
func (r *PatchJobReconciler) getNodeAddress(ctx context.Context, nodeName string) (string, error) {
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	return nodeutil.GetNodeInternalIP(&node)
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

	return resolveTalosConfig(ctx, r.Client, &patchPlan)
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
	defer func() {
		if cerr := talosClient.Close(); cerr != nil {
			logger.Error(cerr, "failed to close Talos client")
		}
	}()

	nodeAddr, err := r.getNodeAddress(ctx, patchJob.Spec.NodeName)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to resolve node address", err)
	}

	target := patchJob.Spec.Target
	nodeSchematicID := ""
	if target.Source == "factory" && target.SchematicID == "" {
		nodeSchematicID, err = talosClient.GetSchematicID(ctx, nodeAddr)
		if err != nil {
			return r.failJob(ctx, original, patchJob, "failed to detect node schematic ID", err)
		}
	}

	// Build the installer image URL
	installerImage, err := patchutil.BuildInstallerImage(target, nodeSchematicID)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to build installer image", err)
	}

	// Initiate upgrade
	if err := talosClient.Upgrade(ctx, nodeAddr, installerImage); err != nil {
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
		"targetTalosVersion", patchJob.Status.TargetTalosVersion)

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
	defer func() {
		if cerr := talosClient.Close(); cerr != nil {
			logger.Error(cerr, "failed to close Talos client")
		}
	}()

	// Try to get version - this checks both responsiveness and upgrade success
	nodeAddr, err := r.getNodeAddress(ctx, patchJob.Spec.NodeName)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to resolve node address", err)
	}

	currentVersion, err := talosClient.GetVersion(ctx, nodeAddr)
	if err != nil {
		// Node not responsive yet, keep waiting
		logger.Info("waiting for node to become responsive", "node", patchJob.Spec.NodeName)
		return ctrl.Result{RequeueAfter: requeueForRebootCheck}, nil
	}

	// Check if upgrade succeeded
	if currentVersion != patchJob.Status.TargetTalosVersion {
		logger.Info("node responsive but upgrade not complete yet",
			"node", patchJob.Spec.NodeName,
			"currentVersion", currentVersion,
			"targetTalosVersion", patchJob.Status.TargetTalosVersion)
		return ctrl.Result{RequeueAfter: requeueForRebootCheck}, nil
	}

	patchJob.Status.CurrentTalosVersion = currentVersion

	// Uncordon the node
	drainer := drain.NewDrainer(r.Client)
	if err := drainer.UncordonNode(ctx, patchJob.Spec.NodeName); err != nil {
		return r.failJob(ctx, original, patchJob, "failed to uncordon node", err)
	}

	// A Kubernetes upgrade may also have been requested alongside the Talos OS upgrade
	if patchJob.Spec.Target.KubernetesVersion != "" && patchJob.Status.CurrentKubernetesVersion != patchJob.Spec.Target.KubernetesVersion {
		patchJob.Status.Phase = patchv1alpha1.PatchJobPhaseUpgradingKubernetes
		patchJob.Status.Message = "talos upgrade completed, ready to upgrade kubernetes"
	} else {
		patchJob.Status.Phase = patchv1alpha1.PatchJobPhaseCompleted
		patchJob.Status.Message = "upgrade completed successfully"
	}

	if err := r.patchStatus(ctx, original, patchJob); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("talos upgrade completed successfully",
		"node", patchJob.Spec.NodeName,
		"version", currentVersion)

	return ctrl.Result{RequeueAfter: requeueImmediate}, nil
}

// startKubernetesUpgrade patches the kubelet (and, for control plane nodes, the control plane
// static pod images) to the target Kubernetes version. Unlike the Talos OS upgrade, this does not
// require a drain/reboot cycle.
func (r *PatchJobReconciler) startKubernetesUpgrade(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	original := patchJob.DeepCopy()

	talosConfig, err := r.getTalosConfig(ctx, patchJob)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to get Talos config", err)
	}

	talosClient, err := talos.NewClient(talosConfig)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to create Talos client", err)
	}
	defer func() {
		if cerr := talosClient.Close(); cerr != nil {
			logger.Error(cerr, "failed to close Talos client")
		}
	}()

	nodeAddr, err := r.getNodeAddress(ctx, patchJob.Spec.NodeName)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to resolve node address", err)
	}

	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: patchJob.Spec.NodeName}, &node); err != nil {
		return r.failJob(ctx, original, patchJob, "failed to get node", err)
	}

	kubernetesVersion := patchJob.Spec.Target.KubernetesVersion

	if nodeutil.IsControlPlane(&node) {
		if err := talosClient.PatchControlPlaneVersion(ctx, nodeAddr,
			patchutil.BuildAPIServerImage(kubernetesVersion),
			patchutil.BuildControllerManagerImage(kubernetesVersion),
			patchutil.BuildSchedulerImage(kubernetesVersion),
		); err != nil {
			return r.failJob(ctx, original, patchJob, "failed to patch control plane components", err)
		}
	}

	if err := talosClient.PatchKubeletVersion(ctx, nodeAddr, patchutil.BuildKubeletImage(kubernetesVersion)); err != nil {
		return r.failJob(ctx, original, patchJob, "failed to patch kubelet version", err)
	}

	patchJob.Status.Phase = patchv1alpha1.PatchJobPhaseValidatingKubernetes
	patchJob.Status.Message = "kubernetes upgrade patch applied, waiting for rollout"

	if err := r.patchStatus(ctx, original, patchJob); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("kubernetes upgrade initiated",
		"node", patchJob.Spec.NodeName,
		"targetKubernetesVersion", kubernetesVersion)

	return ctrl.Result{RequeueAfter: requeueForKubernetesCheck}, nil
}

// waitForKubernetesUpgrade polls the node until the kubelet reports the target version and, for
// control plane nodes, the control plane static pods are running with the target images.
func (r *PatchJobReconciler) waitForKubernetesUpgrade(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	original := patchJob.DeepCopy()

	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: patchJob.Spec.NodeName}, &node); err != nil {
		return r.failJob(ctx, original, patchJob, "failed to get node", err)
	}

	kubernetesVersion := patchJob.Spec.Target.KubernetesVersion

	if node.Status.NodeInfo.KubeletVersion != kubernetesVersion {
		logger.Info("waiting for kubelet to report target version",
			"node", patchJob.Spec.NodeName,
			"currentKubernetesVersion", node.Status.NodeInfo.KubeletVersion,
			"targetKubernetesVersion", kubernetesVersion)
		return ctrl.Result{RequeueAfter: requeueForKubernetesCheck}, nil
	}

	if nodeutil.IsControlPlane(&node) {
		ready, err := r.controlPlaneStaticPodsReady(ctx, patchJob.Spec.NodeName, kubernetesVersion)
		if err != nil {
			return r.failJob(ctx, original, patchJob, "failed to check control plane static pods", err)
		}
		if !ready {
			logger.Info("waiting for control plane static pods to roll out", "node", patchJob.Spec.NodeName)
			return ctrl.Result{RequeueAfter: requeueForKubernetesCheck}, nil
		}
	}

	patchJob.Status.CurrentKubernetesVersion = kubernetesVersion
	patchJob.Status.Phase = patchv1alpha1.PatchJobPhaseCompleted
	patchJob.Status.Message = "kubernetes upgrade completed successfully"

	if err := r.patchStatus(ctx, original, patchJob); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("kubernetes upgrade completed successfully",
		"node", patchJob.Spec.NodeName,
		"version", kubernetesVersion)

	return ctrl.Result{}, nil
}

// controlPlaneStaticPodsReady checks whether the kube-apiserver, kube-controller-manager and
// kube-scheduler static pods on the given node are running the target image and are ready.
func (r *PatchJobReconciler) controlPlaneStaticPodsReady(ctx context.Context, nodeName, kubernetesVersion string) (bool, error) {
	wantImages := map[string]string{
		"kube-apiserver":          patchutil.BuildAPIServerImage(kubernetesVersion),
		"kube-controller-manager": patchutil.BuildControllerManagerImage(kubernetesVersion),
		"kube-scheduler":          patchutil.BuildSchedulerImage(kubernetesVersion),
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(kubeSystemNamespace),
		client.MatchingFields{"spec.nodeName": nodeName},
	); err != nil {
		return false, fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
	}

	found := make(map[string]bool, len(wantImages))

	for i := range podList.Items {
		pod := &podList.Items[i]
		component, ok := pod.Labels["component"]
		if !ok {
			continue
		}
		wantImage, ok := wantImages[component]
		if !ok {
			continue
		}

		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Image == wantImage && cs.Ready {
				found[component] = true
				break
			}
		}
	}

	return len(found) == len(wantImages), nil
}
