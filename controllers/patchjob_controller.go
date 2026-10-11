package controllers

import (
	"context"
	stderrors "errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/drain"
	"github.com/uozalp/kangal-patch/internal/nodeutil"
	"github.com/uozalp/kangal-patch/internal/patchutil"
	"github.com/uozalp/kangal-patch/internal/talos"
)

// talosAPI is the subset of *talos.Client the PatchJob state machine uses.
type talosAPI interface {
	GetVersion(ctx context.Context, nodeName string) (string, error)
	GetSchematicID(ctx context.Context, nodeName string) (string, error)
	Upgrade(ctx context.Context, nodeName, image string) error
	PatchKubeletVersion(ctx context.Context, nodeName, kubeletImage string) error
	PatchControlPlaneVersion(ctx context.Context, nodeName, apiServerImage, controllerManagerImage, schedulerImage string) error
	Close() error
}

// PatchJobReconciler reconciles a PatchJob object
type PatchJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Namespace is the operator namespace; the only one Talos Secrets may be read from.
	Namespace string

	// NewTalosClient overrides talos.NewClient; tests inject a fake.
	NewTalosClient func(*talos.Config) (talosAPI, error)

	// transientSince holds the time of the first unbroken run of transient errors per PatchJob.
	// In-memory only: a controller restart simply restarts the retry window.
	transientSince sync.Map
}

// Requeue intervals for different phases
const (
	requeueImmediate            = 1 * time.Millisecond // Requeue is deprecated in favor of a minimal RequeueAfter
	requeueForDrainCheck        = 10 * time.Second     // checking if drain is complete
	requeueForUpgradeStart      = 5 * time.Second      // starting upgrade operation
	requeueForRebootCheck       = 60 * time.Second     // checking if node is back online
	requeueForKubernetesUpgrade = 5 * time.Second      // starting the kubernetes upgrade patch
	requeueForKubernetesCheck   = 15 * time.Second     // checking if kubelet/static pods report the new version

	defaultRebootTimeout            = 10 * time.Minute // used when the plan has no rebootTimeout
	defaultKubernetesUpgradeTimeout = 10 * time.Minute // used when the plan has no kubernetesUpgradeTimeout

	// transientErrorDeadline is how long API/Talos errors are retried with backoff before the job fails
	transientErrorDeadline = 5 * time.Minute

	// maxConcurrentReconciles keeps one slow node from blocking reconciles of the others
	maxConcurrentReconciles = 4
)

// +kubebuilder:rbac:groups=kangalpatch.ozalp.dk,resources=patchjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kangalpatch.ozalp.dk,resources=patchjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

const kubeSystemNamespace = "kube-system"

func (r *PatchJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the PatchJob
	var patchJob patchv1alpha1.PatchJob
	if err := r.Get(ctx, req.NamespacedName, &patchJob); err != nil {
		if errors.IsNotFound(err) {
			r.transientSince.Delete(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch PatchJob")
		return ctrl.Result{}, err
	}

	// Terminal phases - nothing more to do
	if patchJob.Status.Phase.IsTerminal() {
		r.transientSince.Delete(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	res, err := r.reconcilePhase(ctx, &patchJob)
	if err == nil {
		r.transientSince.Delete(req.NamespacedName)
	}
	return res, err
}

func (r *PatchJobReconciler) reconcilePhase(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (ctrl.Result, error) {
	switch patchJob.Status.Phase {
	case "":
		return r.initJob(ctx, patchJob)

	case patchv1alpha1.PatchJobPhasePending:
		return r.startDrain(ctx, patchJob)

	case patchv1alpha1.PatchJobPhaseDraining:
		return r.waitForDrain(ctx, patchJob)

	case patchv1alpha1.PatchJobPhaseUpgrading:
		return r.startUpgrade(ctx, patchJob)

	case patchv1alpha1.PatchJobPhaseRebooting:
		return r.waitForReboot(ctx, patchJob)

	case patchv1alpha1.PatchJobPhaseUpgradingKubernetes:
		return r.startKubernetesUpgrade(ctx, patchJob)

	case patchv1alpha1.PatchJobPhaseValidatingKubernetes:
		return r.waitForKubernetesUpgrade(ctx, patchJob)

	}

	return ctrl.Result{}, nil
}

func (r *PatchJobReconciler) newTalosClient(cfg *talos.Config) (talosAPI, error) {
	if r.NewTalosClient != nil {
		return r.NewTalosClient(cfg)
	}
	return talos.NewClient(cfg)
}

func (r *PatchJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&patchv1alpha1.PatchJob{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Complete(r)
}

// initJob initializes a new PatchJob with pending status
func (r *PatchJobReconciler) initJob(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	original := patchJob.DeepCopy()
	patchJob.Status.StartTime = &metav1.Time{Time: time.Now()}

	return r.withTalos(ctx, original, patchJob, func(talosClient talosAPI, node *corev1.Node, nodeAddr string) (ctrl.Result, error) {
		// Get current Talos and Kubernetes versions from the node
		currentTalosVersion, err := talosClient.GetVersion(ctx, nodeAddr)
		if err != nil {
			return r.retryOrFail(ctx, original, patchJob, "failed to get current Talos version", err)
		}

		currentKubernetesVersion := node.Status.NodeInfo.KubeletVersion

		targetTalosVersion := patchJob.Spec.Target.TalosVersion
		targetKubernetesVersion := patchJob.Spec.Target.KubernetesVersion

		nodeSchematicID := ""
		if checksSchematic(patchJob.Spec.Target) {
			if nodeSchematicID, err = talosClient.GetSchematicID(ctx, nodeAddr); err != nil {
				return r.retryOrFail(ctx, original, patchJob, "failed to detect node schematic ID", err)
			}
		}

		talosNeedsUpgrade := needsTalosUpgrade(patchJob.Spec.Target, currentTalosVersion, nodeSchematicID)
		kubernetesNeedsUpgrade := targetKubernetesVersion != "" && currentKubernetesVersion != targetKubernetesVersion
		// A kubelet at target does not prove the control plane static pods rolled out (e.g. a retried job)
		if targetKubernetesVersion != "" && !kubernetesNeedsUpgrade && nodeutil.IsControlPlane(node) {
			ready, err := r.controlPlaneStaticPodsReady(ctx, node.Name, targetKubernetesVersion)
			if err != nil {
				return r.retryOrFail(ctx, original, patchJob, "failed to check control plane static pods", err)
			}
			kubernetesNeedsUpgrade = !ready
		}
		patchJob.Status.CurrentTalosVersion = currentTalosVersion
		patchJob.Status.CurrentKubernetesVersion = currentKubernetesVersion

		if talosNeedsUpgrade && !patchJob.Spec.Target.AllowTalosDowngrade && isDowngrade(currentTalosVersion, targetTalosVersion) {
			return r.failJob(ctx, original, patchJob, "refusing to downgrade Talos",
				fmt.Errorf("node runs %s, target is older (%s); set target.allowTalosDowngrade to roll back", currentTalosVersion, targetTalosVersion))
		}
		if targetKubernetesVersion != "" && isDowngrade(currentKubernetesVersion, targetKubernetesVersion) {
			return r.failJob(ctx, original, patchJob, "refusing to downgrade Kubernetes",
				fmt.Errorf("kubelet runs %s, target is older (%s)", currentKubernetesVersion, targetKubernetesVersion))
		}
		// Display the requested target, or the current value if that dimension isn't being changed
		patchJob.Status.TargetTalosVersion = firstNonEmpty(targetTalosVersion, currentTalosVersion)
		patchJob.Status.TargetKubernetesVersion = firstNonEmpty(targetKubernetesVersion, currentKubernetesVersion)

		// Check if already at target version(s)
		if !talosNeedsUpgrade && !kubernetesNeedsUpgrade {
			// Release a cordon left by a previous run of this operator (admin cordons are kept)
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
	})
}

// checksSchematic reports whether the target pins a factory schematic the node must run.
func checksSchematic(t patchv1alpha1.TargetSpec) bool {
	return t.Source == "factory" && t.SchematicID != ""
}

// needsTalosUpgrade reports whether a node must be upgraded: it runs another Talos version, or
// another schematic than the one the target pins.
func needsTalosUpgrade(t patchv1alpha1.TargetSpec, version, schematicID string) bool {
	if t.TalosVersion == "" {
		return false
	}
	return version != t.TalosVersion || (checksSchematic(t) && schematicID != t.SchematicID)
}

// firstNonEmpty returns a if it is non-empty, otherwise b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// retryOrFail returns err so controller-runtime retries with backoff, and fails the job only once
// errors have persisted for transientErrorDeadline. Use it for API and Talos call errors; use failJob
// directly for permanent causes.
func (r *PatchJobReconciler) retryOrFail(ctx context.Context, original, modified *patchv1alpha1.PatchJob, message string, err error) (ctrl.Result, error) {
	since, _ := r.transientSince.LoadOrStore(client.ObjectKeyFromObject(modified), time.Now())
	if time.Since(since.(time.Time)) > transientErrorDeadline {
		return r.failJob(ctx, original, modified, message, err)
	}
	return ctrl.Result{}, fmt.Errorf("%s (will retry): %w", message, err)
}

// failJob transitions a PatchJob to failed state and updates status. Before the reboot the node still
// runs the old version, so the cordon is released; after it the node is left cordoned for inspection.
func (r *PatchJobReconciler) failJob(ctx context.Context, original, modified *patchv1alpha1.PatchJob, message string, err error) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Error(err, message)

	switch original.Status.Phase {
	case patchv1alpha1.PatchJobPhasePending, patchv1alpha1.PatchJobPhaseDraining, patchv1alpha1.PatchJobPhaseUpgrading:
		if uerr := drain.NewDrainer(r.Client).UncordonNode(ctx, modified.Spec.NodeName); uerr != nil {
			logger.Error(uerr, "failed to uncordon node after failure", "node", modified.Spec.NodeName)
		}
	}

	modified.Status.Phase = patchv1alpha1.PatchJobPhaseFailed
	modified.Status.Message = fmt.Sprintf("%s: %v", message, err)

	if statusErr := r.patchStatus(ctx, original, modified); statusErr != nil {
		logger.Error(statusErr, "failed to update status after failure")
		return ctrl.Result{}, statusErr // retry so the failure is recorded
	}

	return ctrl.Result{}, nil
}

// patchStatus applies a status patch to the PatchJob, only updating changed fields.
func (r *PatchJobReconciler) patchStatus(ctx context.Context, original, modified *patchv1alpha1.PatchJob) error {
	if modified.Status.Phase.IsTerminal() && modified.Status.CompletionTime == nil {
		modified.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	}
	return patchutil.PatchStatus(ctx, r.Status(), original, modified)
}

// withTalos opens a Talos client and looks up the job's Node and its InternalIP, then runs fn.
// Failures in this setup go through retryOrFail/failJob; the client is closed when fn returns.
func (r *PatchJobReconciler) withTalos(ctx context.Context, original, patchJob *patchv1alpha1.PatchJob, fn func(c talosAPI, node *corev1.Node, addr string) (ctrl.Result, error)) (ctrl.Result, error) {
	talosConfig, err := r.getTalosConfig(ctx, patchJob)
	if err != nil {
		return r.retryOrFail(ctx, original, patchJob, "failed to get Talos config", err)
	}

	talosClient, err := r.newTalosClient(talosConfig)
	if err != nil {
		return r.failJob(ctx, original, patchJob, "failed to create Talos client", err)
	}
	defer func() {
		if cerr := talosClient.Close(); cerr != nil {
			log.FromContext(ctx).Error(cerr, "failed to close Talos client")
		}
	}()

	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: patchJob.Spec.NodeName}, &node); err != nil {
		return r.retryOrFail(ctx, original, patchJob, "failed to get node", err)
	}

	nodeAddr, err := nodeutil.GetNodeInternalIP(&node)
	if err != nil {
		return r.retryOrFail(ctx, original, patchJob, "failed to resolve node address", err)
	}

	return fn(talosClient, &node, nodeAddr)
}

// getPatchPlan retrieves the parent PatchPlan.
func (r *PatchJobReconciler) getPatchPlan(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (*patchv1alpha1.PatchPlan, error) {
	if patchJob.Spec.PatchPlanRef == "" {
		return nil, fmt.Errorf("patchPlanRef not set")
	}

	var patchPlan patchv1alpha1.PatchPlan
	if err := r.Get(ctx, types.NamespacedName{Name: patchJob.Spec.PatchPlanRef}, &patchPlan); err != nil {
		return nil, fmt.Errorf("failed to get PatchPlan %s: %w", patchJob.Spec.PatchPlanRef, err)
	}

	return &patchPlan, nil
}

// getTalosConfig retrieves Talos configuration from the parent PatchPlan.
func (r *PatchJobReconciler) getTalosConfig(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (*talos.Config, error) {
	patchPlan, err := r.getPatchPlan(ctx, patchJob)
	if err != nil {
		return nil, err
	}

	return resolveTalosConfig(ctx, r.Client, r.Namespace, patchPlan)
}

// drainTimeout returns the plan's drain timeout, or the default when unset.
func drainTimeout(plan *patchv1alpha1.PatchPlan) time.Duration {
	if d := plan.Spec.DrainTimeout.Duration; d > 0 {
		return d
	}
	return drain.DefaultTimeout
}

// startDrain cordons the node and initiates the drain process
func (r *PatchJobReconciler) startDrain(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	original := patchJob.DeepCopy()

	// Get PatchPlan to retrieve drain settings
	patchPlan, err := r.getPatchPlan(ctx, patchJob)
	if err != nil {
		return r.retryOrFail(ctx, original, patchJob, "failed to get PatchPlan", err)
	}

	// Create drainer
	drainer := drain.NewDrainer(r.Client)

	// Cordon the node first
	if err := drainer.CordonNode(ctx, patchJob.Spec.NodeName); err != nil {
		return r.retryOrFail(ctx, original, patchJob, "failed to cordon node", err)
	}

	logger.Info("node cordoned", "node", patchJob.Spec.NodeName)

	// Start drain operation
	drainOpts := drain.DrainOptions{
		RespectPDBs:    patchPlan.Spec.RespectPDBs,
		EvictUnmanaged: patchPlan.Spec.EvictUnmanagedPods,
		Timeout:        drainTimeout(patchPlan),
	}

	if patchJob.Status.DrainStartTime == nil {
		patchJob.Status.DrainStartTime = &metav1.Time{Time: time.Now()}
	}

	blocked, err := drainer.DrainNode(ctx, patchJob.Spec.NodeName, drainOpts)
	if stderrors.Is(err, drain.ErrUnmanagedPods) {
		return r.failJob(ctx, original, patchJob, "drain refused", err)
	}
	if err != nil {
		return r.retryOrFail(ctx, original, patchJob, "failed to drain node", err)
	}

	if blocked {
		// Stay Pending and retry; only fail once drainTimeout has elapsed since the first attempt.
		if time.Since(patchJob.Status.DrainStartTime.Time) > drainOpts.Timeout {
			return r.failJob(ctx, original, patchJob, "drain timed out",
				fmt.Errorf("pods still blocked by PodDisruptionBudgets after %s", drainOpts.Timeout))
		}
		patchJob.Status.Message = "drain blocked by PodDisruptionBudget, retrying"
		if err := r.patchStatus(ctx, original, patchJob); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueForDrainCheck}, nil
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
		return r.retryOrFail(ctx, original, patchJob, "failed to check drain status", err)
	}

	if !drained {
		patchPlan, err := r.getPatchPlan(ctx, patchJob)
		if err != nil {
			return r.retryOrFail(ctx, original, patchJob, "failed to get PatchPlan", err)
		}
		timeout := drainTimeout(patchPlan)
		if start := patchJob.Status.DrainStartTime; start != nil && time.Since(start.Time) > timeout {
			return r.failJob(ctx, original, patchJob, "drain timed out",
				fmt.Errorf("pods still on node after %s", timeout))
		}
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

	return r.withTalos(ctx, original, patchJob, func(talosClient talosAPI, node *corev1.Node, nodeAddr string) (ctrl.Result, error) {
		var err error
		target := patchJob.Spec.Target
		nodeSchematicID := ""
		if target.Source == "factory" && target.SchematicID == "" {
			nodeSchematicID, err = talosClient.GetSchematicID(ctx, nodeAddr)
			if err != nil {
				return r.retryOrFail(ctx, original, patchJob, "failed to detect node schematic ID", err)
			}
		}

		// Build the installer image URL
		installerImage, err := patchutil.BuildInstallerImage(target, nodeSchematicID)
		if err != nil {
			return r.failJob(ctx, original, patchJob, "failed to build installer image", err)
		}

		// Initiate upgrade
		if err := talosClient.Upgrade(ctx, nodeAddr, installerImage); err != nil {
			return r.retryOrFail(ctx, original, patchJob, "failed to start upgrade", err)
		}

		// Update status to rebooting phase
		patchJob.Status.Phase = patchv1alpha1.PatchJobPhaseRebooting
		patchJob.Status.Message = "upgrade started, waiting for reboot"
		patchJob.Status.RebootStartTime = &metav1.Time{Time: time.Now()}
		patchJob.Status.PreUpgradeBootID = node.Status.NodeInfo.BootID

		// Upgrade was accepted: retry the write, a failed one would trigger a second Upgrade call.
		if err := retry.OnError(retry.DefaultBackoff, func(error) bool { return true }, func() error {
			return r.patchStatus(ctx, original, patchJob)
		}); err != nil {
			logger.Error(err, "failed to update status")
			return ctrl.Result{}, err
		}

		logger.Info("upgrade initiated",
			"node", patchJob.Spec.NodeName,
			"targetTalosVersion", patchJob.Status.TargetTalosVersion)

		return ctrl.Result{RequeueAfter: requeueForRebootCheck}, nil
	})
}

// waitForReboot checks if the node has rebooted and become responsive, then validates the upgrade
func (r *PatchJobReconciler) waitForReboot(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	original := patchJob.DeepCopy()

	return r.withTalos(ctx, original, patchJob, func(talosClient talosAPI, node *corev1.Node, nodeAddr string) (ctrl.Result, error) {
		patchPlan, err := r.getPatchPlan(ctx, patchJob)
		if err != nil {
			return r.retryOrFail(ctx, original, patchJob, "failed to get PatchPlan", err)
		}

		// Jobs that entered Rebooting before the start time was recorded get a fresh timeout window
		if patchJob.Status.RebootStartTime == nil {
			patchJob.Status.RebootStartTime = &metav1.Time{Time: time.Now()}
			if err := r.patchStatus(ctx, original, patchJob); err != nil {
				logger.Error(err, "failed to update status")
				return ctrl.Result{}, err
			}
			original = patchJob.DeepCopy()
		}

		rebootTimeout := patchPlan.Spec.RebootTimeout.Duration
		if rebootTimeout <= 0 {
			rebootTimeout = defaultRebootTimeout
		}
		remaining := rebootTimeout - time.Since(patchJob.Status.RebootStartTime.Time)
		timedOut := remaining <= 0
		requeueAfter := min(requeueForRebootCheck, max(remaining, requeueImmediate))

		// Try to get version - this checks both responsiveness and upgrade success
		currentVersion, err := talosClient.GetVersion(ctx, nodeAddr)
		if err != nil {
			if timedOut {
				return r.failJob(ctx, original, patchJob, "node did not come back after reboot",
					fmt.Errorf("no response within %s (last error: %v); node left cordoned, verify it and uncordon manually", rebootTimeout, err))
			}
			logger.Info("waiting for node to become responsive", "node", patchJob.Spec.NodeName)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}

		// Check if upgrade succeeded
		if currentVersion != patchJob.Status.TargetTalosVersion {
			// A new boot ID with the old version means the node rebooted into the previous partition
			bootID := patchJob.Status.PreUpgradeBootID
			if bootID != "" && node.Status.NodeInfo.BootID != bootID {
				return r.failJob(ctx, original, patchJob, "upgrade rolled back",
					fmt.Errorf("node rebooted but is running %s, want %s; node left cordoned, verify it and uncordon manually",
						currentVersion, patchJob.Status.TargetTalosVersion))
			}
			if timedOut {
				return r.failJob(ctx, original, patchJob, "node did not reach target version",
					fmt.Errorf("running %s, want %s after %s; node left cordoned, verify it and uncordon manually",
						currentVersion, patchJob.Status.TargetTalosVersion, rebootTimeout))
			}
			logger.Info("node responsive but upgrade not complete yet",
				"node", patchJob.Spec.NodeName,
				"currentVersion", currentVersion,
				"targetTalosVersion", patchJob.Status.TargetTalosVersion)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}

		// The Talos API answers early in boot; the node must also be back in Kubernetes (and etcd, via the
		// control plane static pods) before the next node may be taken down.
		if reason, err := r.nodeUnhealthy(ctx, node, patchJob.Status.PreUpgradeBootID); err != nil {
			return r.retryOrFail(ctx, original, patchJob, "failed to check node health", err)
		} else if reason != "" {
			if timedOut {
				return r.failJob(ctx, original, patchJob, "node did not become healthy after upgrade",
					fmt.Errorf("%s after %s; node left cordoned, verify it and uncordon manually", reason, rebootTimeout))
			}
			logger.Info("waiting for node to become healthy", "node", patchJob.Spec.NodeName, "reason", reason)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}

		patchJob.Status.CurrentTalosVersion = currentVersion

		// Uncordon the node
		drainer := drain.NewDrainer(r.Client)
		if err := drainer.UncordonNode(ctx, patchJob.Spec.NodeName); err != nil {
			return r.retryOrFail(ctx, original, patchJob, "failed to uncordon node", err)
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
	})
}

// nodeUnhealthy returns why the node is not yet healthy after a reboot, or "" when it is: kubelet has
// reported from the new boot, the Node is Ready and, on control plane nodes, the static pods are ready.
func (r *PatchJobReconciler) nodeUnhealthy(ctx context.Context, node *corev1.Node, preUpgradeBootID string) (string, error) {
	// Until kubelet reports the new boot ID, the Ready condition may still be the stale pre-reboot one
	if preUpgradeBootID != "" && node.Status.NodeInfo.BootID == preUpgradeBootID {
		return "kubelet has not reported since the reboot", nil
	}
	ready := false
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	if !ready {
		return "node is not Ready", nil
	}
	if nodeutil.IsControlPlane(node) {
		podsReady, err := r.controlPlaneStaticPodsReady(ctx, node.Name, "")
		if err != nil {
			return "", err
		}
		if !podsReady {
			return "control plane static pods are not all ready", nil
		}
	}
	return "", nil
}

// startKubernetesUpgrade patches the kubelet (and, for control plane nodes, the control plane
// static pod images) to the target Kubernetes version. Unlike the Talos OS upgrade, this does not
// require a drain/reboot cycle.
func (r *PatchJobReconciler) startKubernetesUpgrade(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	original := patchJob.DeepCopy()

	return r.withTalos(ctx, original, patchJob, func(talosClient talosAPI, node *corev1.Node, nodeAddr string) (ctrl.Result, error) {
		kubernetesVersion := patchJob.Spec.Target.KubernetesVersion

		if nodeutil.IsControlPlane(node) {
			if err := talosClient.PatchControlPlaneVersion(ctx, nodeAddr,
				patchutil.BuildAPIServerImage(kubernetesVersion),
				patchutil.BuildControllerManagerImage(kubernetesVersion),
				patchutil.BuildSchedulerImage(kubernetesVersion),
			); err != nil {
				return r.retryOrFail(ctx, original, patchJob, "failed to patch control plane components", err)
			}
		}

		if err := talosClient.PatchKubeletVersion(ctx, nodeAddr, patchutil.BuildKubeletImage(kubernetesVersion)); err != nil {
			return r.retryOrFail(ctx, original, patchJob, "failed to patch kubelet version", err)
		}

		patchJob.Status.Phase = patchv1alpha1.PatchJobPhaseValidatingKubernetes
		patchJob.Status.Message = "kubernetes upgrade patch applied, waiting for rollout"
		patchJob.Status.KubernetesUpgradeStartTime = &metav1.Time{Time: time.Now()}

		if err := r.patchStatus(ctx, original, patchJob); err != nil {
			logger.Error(err, "failed to update status")
			return ctrl.Result{}, err
		}

		logger.Info("kubernetes upgrade initiated",
			"node", patchJob.Spec.NodeName,
			"targetKubernetesVersion", kubernetesVersion)

		return ctrl.Result{RequeueAfter: requeueForKubernetesCheck}, nil
	})
}

// waitForKubernetesUpgrade polls the node until the kubelet reports the target version and, for
// control plane nodes, the control plane static pods are running with the target images.
func (r *PatchJobReconciler) waitForKubernetesUpgrade(ctx context.Context, patchJob *patchv1alpha1.PatchJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	original := patchJob.DeepCopy()

	patchPlan, err := r.getPatchPlan(ctx, patchJob)
	if err != nil {
		return r.retryOrFail(ctx, original, patchJob, "failed to get PatchPlan", err)
	}

	// Jobs that entered ValidatingKubernetes before the start time was recorded get a fresh window
	if patchJob.Status.KubernetesUpgradeStartTime == nil {
		patchJob.Status.KubernetesUpgradeStartTime = &metav1.Time{Time: time.Now()}
		if err := r.patchStatus(ctx, original, patchJob); err != nil {
			logger.Error(err, "failed to update status")
			return ctrl.Result{}, err
		}
		original = patchJob.DeepCopy()
	}

	timeout := patchPlan.Spec.KubernetesUpgradeTimeout.Duration
	if timeout <= 0 {
		timeout = defaultKubernetesUpgradeTimeout
	}
	remaining := timeout - time.Since(patchJob.Status.KubernetesUpgradeStartTime.Time)
	timedOut := remaining <= 0
	requeueAfter := min(requeueForKubernetesCheck, max(remaining, requeueImmediate))

	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: patchJob.Spec.NodeName}, &node); err != nil {
		return r.retryOrFail(ctx, original, patchJob, "failed to get node", err)
	}

	kubernetesVersion := patchJob.Spec.Target.KubernetesVersion

	if node.Status.NodeInfo.KubeletVersion != kubernetesVersion {
		if timedOut {
			return r.failJob(ctx, original, patchJob, "kubelet did not reach target version",
				fmt.Errorf("running %s, want %s after %s", node.Status.NodeInfo.KubeletVersion, kubernetesVersion, timeout))
		}
		logger.Info("waiting for kubelet to report target version",
			"node", patchJob.Spec.NodeName,
			"currentKubernetesVersion", node.Status.NodeInfo.KubeletVersion,
			"targetKubernetesVersion", kubernetesVersion)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	if nodeutil.IsControlPlane(&node) {
		ready, err := r.controlPlaneStaticPodsReady(ctx, patchJob.Spec.NodeName, kubernetesVersion)
		if err != nil {
			return r.retryOrFail(ctx, original, patchJob, "failed to check control plane static pods", err)
		}
		if !ready {
			if timedOut {
				return r.failJob(ctx, original, patchJob, "control plane static pods did not roll out",
					fmt.Errorf("not running and ready with %s images after %s", kubernetesVersion, timeout))
			}
			logger.Info("waiting for control plane static pods to roll out", "node", patchJob.Spec.NodeName)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
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
// An empty kubernetesVersion accepts any image.
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
			if (kubernetesVersion == "" || cs.Image == wantImage) && cs.Ready {
				found[component] = true
				break
			}
		}
	}

	return len(found) == len(wantImages), nil
}
