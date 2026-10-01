package controllers

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/maintenance"
	"github.com/uozalp/kangal-patch/internal/nodeutil"
	"github.com/uozalp/kangal-patch/internal/patchutil"
	"github.com/uozalp/kangal-patch/internal/scheduling"
)

// PatchPlanReconciler reconciles a PatchPlan object
type PatchPlanReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Namespace string
	// Discovery is used for the Kubernetes API health check during preflight.
	Discovery discovery.DiscoveryInterface
	// Releases lists upstream Talos releases for auto-update templates.
	Releases releaseLister
}

// JobCounts are the PatchJob outcomes of the unique nodes covered by the rollout.
type JobCounts struct {
	Completed int
	Failed    int
	Running   int
	Pending   int
}

// Requeue intervals for different reconciliation scenarios
const (
	requeueWhenAtMaxConcurrency = 5 * time.Second  // waiting for job completion to free capacity
	requeueForNextNode          = 10 * time.Second // interval between scheduling nodes
	requeueWhenJobsInProgress   = 30 * time.Second // waiting for in-progress jobs to complete
	requeueWhenPaused           = time.Minute      // checking if plan is still paused
)

// +kubebuilder:rbac:groups=kangalpatch.ozalp.dk,resources=patchplans,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kangalpatch.ozalp.dk,resources=patchplans/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kangalpatch.ozalp.dk,resources=patchplans/finalizers,verbs=update
// +kubebuilder:rbac:groups=kangalpatch.ozalp.dk,resources=patchjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;update;patch

// kubeProxyDaemonSetName/kubeProxyNamespace identify the cluster-wide kube-proxy DaemonSet that
// is upgraded once, after all control plane nodes have completed their Kubernetes upgrade.
const (
	kubeProxyDaemonSetName = "kube-proxy"
	kubeProxyNamespace     = "kube-system"
)

// Reconcile handles PatchPlan reconciliation
func (r *PatchPlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the PatchPlan
	var patchPlan patchv1alpha1.PatchPlan
	if err := r.Get(ctx, req.NamespacedName, &patchPlan); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch PatchPlan")
		return ctrl.Result{}, err
	}

	if patchPlan.IsAutoUpdateTemplate() {
		return r.reconcileAutoUpdate(ctx, &patchPlan)
	}

	// Once finished history is purged the plan is frozen and must not schedule anything again
	purged, err := r.applyRetention(ctx, &patchPlan)
	if err != nil {
		return ctrl.Result{}, err
	}
	if purged {
		return ctrl.Result{}, nil
	}

	// Get the nodes selected by the plan and resolve which group schedules each of them
	nodes, err := nodeutil.ListMatchingNodes(ctx, r.Client, patchPlan.Spec.NodeSelector)
	if err != nil {
		return ctrl.Result{}, err
	}
	rollout, err := scheduling.Resolve(nodes, patchPlan.Spec)
	if err != nil {
		return r.failInvalidStrategy(ctx, &patchPlan, err)
	}

	// Get list of existing PatchJobs
	jobs, err := r.listPatchJobs(ctx, patchPlan.Name)
	if err != nil {
		logger.Error(err, "unable to list PatchJobs")
		return ctrl.Result{}, err
	}
	jobsByNode := make(map[string]*patchv1alpha1.PatchJob, len(jobs))
	for i := range jobs {
		jobsByNode[jobs[i].Spec.NodeName] = &jobs[i]
	}

	// Leases are the source of truth for concurrency: release finished jobs, heal missing ones
	activeLeases, err := r.syncLeases(ctx, &patchPlan, rollout, jobs)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Update status counts and total nodes
	original := patchPlan.DeepCopy()
	jobSummary := summarize(&patchPlan, rollout, jobsByNode, activeLeases)
	// Deleting accepted failed jobs (to retry them) must not leave a credit for future failures
	patchPlan.Status.FailuresAcknowledged = min(patchPlan.Status.FailuresAcknowledged, jobSummary.Failed)
	patchPlan.Status.TargetTalosVersion = patchPlan.Spec.Target.TalosVersion
	patchPlan.Status.TargetKubernetesVersion = patchPlan.Spec.Target.KubernetesVersion

	if !equality.Semantic.DeepEqual(original.Status, patchPlan.Status) {
		if err := r.patchStatus(ctx, original, &patchPlan); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Handle cancellation - takes priority over pause and stops the reconciler for good
	cancelled, err := r.ensureCancelledState(ctx, &patchPlan)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cancelled {
		return terminalResult(&patchPlan), nil
	}

	// Handle pause/resume state
	paused, err := r.ensurePauseState(ctx, &patchPlan, jobSummary.Failed)
	if err != nil {
		return ctrl.Result{}, err
	}
	if paused {
		return ctrl.Result{RequeueAfter: requeueWhenPaused}, nil
	}

	// Validate the plan once before any PatchJob exists; nothing is scheduled until it passes
	preflightPassed, err := r.ensurePreflight(ctx, &patchPlan, rollout, jobsByNode)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !preflightPassed {
		return ctrl.Result{RequeueAfter: requeueWhenPreflightFailed}, nil
	}

	// Check if all nodes are processed
	allProcessed, err := r.allNodesProcessed(ctx, &patchPlan, jobSummary, rollout.Total())
	if err != nil {
		return ctrl.Result{}, err
	}
	if allProcessed {
		return terminalResult(&patchPlan), nil
	}

	// Check the failure policy
	failureBudgetExceeded, err := r.failureBudgetExceeded(ctx, &patchPlan, jobSummary.Failed)
	if err != nil {
		return ctrl.Result{}, err
	}
	if failureBudgetExceeded {
		return terminalResult(&patchPlan), nil
	}

	// Check maintenance window
	inWindow, message := maintenance.IsInMaintenanceWindow(patchPlan.Spec.Maintenance, time.Now())
	if !inWindow {
		logger.Info("outside maintenance window, waiting", "reason", message)
		// Try to calculate next window
		nextWindow, err := maintenance.NextMaintenanceWindow(patchPlan.Spec.Maintenance, time.Now())
		if err != nil {
			logger.Info("unable to determine next maintenance window", "error", err.Error())
			return ctrl.Result{RequeueAfter: 15 * time.Minute}, nil
		}
		requeueDuration := time.Until(nextWindow)
		logger.Info("scheduling for next maintenance window", "nextWindow", nextWindow, "requeue", requeueDuration)
		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	return r.scheduleNodes(ctx, &patchPlan, rollout, jobsByNode, activeLeases)
}

// SetupWithManager sets up the controller with the Manager
func (r *PatchPlanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index PatchJobs by their PatchPlan reference for efficient lookup
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &patchv1alpha1.PatchJob{}, "spec.patchPlanRef", func(rawObj client.Object) []string {
		patchJob := rawObj.(*patchv1alpha1.PatchJob)
		if patchJob.Spec.PatchPlanRef == "" {
			return nil
		}
		return []string{patchJob.Spec.PatchPlanRef}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&patchv1alpha1.PatchPlan{}).
		Owns(&patchv1alpha1.PatchPlan{}). // child plans wake their auto-update template
		Owns(&patchv1alpha1.PatchJob{}).
		Complete(r)
}

// newFailures is the number of failed nodes the user has not yet accepted by resuming the plan.
func newFailures(patchPlan *patchv1alpha1.PatchPlan, failed int) int {
	return max(failed-patchPlan.Status.FailuresAcknowledged, 0)
}

// failureBudgetExceeded applies the failurePolicy once enough nodes failed: Halt marks the plan
// Failed, Pause pauses it for inspection. Returns true if scheduling must stop.
func (r *PatchPlanReconciler) failureBudgetExceeded(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, failedCount int) (bool, error) {
	policy := patchPlan.Spec.FailurePolicy
	if !policy.Exceeded(newFailures(patchPlan, failedCount)) {
		return false, nil
	}

	logger := log.FromContext(ctx)

	if policy.Type == patchv1alpha1.FailurePolicyPause {
		if err := r.pauseOnFailures(ctx, patchPlan, failedCount); err != nil {
			return true, err
		}
		logger.Info("PatchPlan paused due to failure policy", "failed", failedCount, "maxFailures", policy.MaxFailures)
		return true, nil
	}

	if patchPlan.Status.Phase != patchv1alpha1.PatchPhaseFailed {
		original := patchPlan.DeepCopy()
		patchPlan.Status.Phase = patchv1alpha1.PatchPhaseFailed
		patchPlan.Status.Message = fmt.Sprintf("failure policy Halt: %d failed node(s), maxFailures %d", failedCount, policy.MaxFailures)
		patchPlan.Status.CompletionTime = &metav1.Time{Time: time.Now()}

		if err := r.patchStatus(ctx, original, patchPlan); err != nil {
			return true, err
		}
	}

	logger.Info("PatchPlan failed due to failure policy", "failed", failedCount, "maxFailures", policy.MaxFailures)
	return true, nil
}

// pauseOnFailures sets spec.paused so the regular pause handling takes over; the user resumes by
// setting it back to false.
func (r *PatchPlanReconciler) pauseOnFailures(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, failedCount int) error {
	if !patchPlan.Spec.Paused {
		originalSpec := patchPlan.DeepCopy()
		patchPlan.Spec.Paused = true
		if err := r.Patch(ctx, patchPlan, client.MergeFrom(originalSpec)); err != nil {
			return fmt.Errorf("failed to pause PatchPlan: %w", err)
		}
	}

	original := patchPlan.DeepCopy()
	patchPlan.Status.Phase = patchv1alpha1.PatchPhasePaused
	patchPlan.Status.Message = fmt.Sprintf("Paused by failure policy: %d failed node(s). Inspect or delete the failed PatchJobs, then set spec.paused=false to resume", failedCount)
	return r.patchStatus(ctx, original, patchPlan)
}

// allNodesCompleted returns true if every node has a Completed PatchJob in jobsByNode.
func allNodesCompleted(nodes []corev1.Node, jobsByNode map[string]*patchv1alpha1.PatchJob) bool {
	for i := range nodes {
		job, ok := jobsByNode[nodes[i].Name]
		if !ok || job.Status.Phase != patchv1alpha1.PatchJobPhaseCompleted {
			return false
		}
	}
	return true
}

// ensureKubeProxyUpgraded patches the cluster-wide kube-proxy DaemonSet image to match
// kubernetesVersion and reports whether the rollout has finished. Clusters without a kube-proxy
// DaemonSet (e.g. kube-proxy-less CNI setups) are treated as already done.
func (r *PatchPlanReconciler) ensureKubeProxyUpgraded(ctx context.Context, kubernetesVersion string) (bool, error) {
	logger := log.FromContext(ctx)

	var ds appsv1.DaemonSet
	if err := r.Get(ctx, types.NamespacedName{Name: kubeProxyDaemonSetName, Namespace: kubeProxyNamespace}, &ds); err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("failed to get kube-proxy DaemonSet: %w", err)
	}

	if len(ds.Spec.Template.Spec.Containers) == 0 {
		return false, fmt.Errorf("kube-proxy DaemonSet has no containers")
	}

	targetImage := patchutil.BuildKubeProxyImage(kubernetesVersion)
	if ds.Spec.Template.Spec.Containers[0].Image != targetImage {
		original := ds.DeepCopy()
		ds.Spec.Template.Spec.Containers[0].Image = targetImage
		if err := r.Patch(ctx, &ds, client.MergeFrom(original)); err != nil {
			return false, fmt.Errorf("failed to update kube-proxy image: %w", err)
		}
		logger.Info("upgraded kube-proxy DaemonSet image", "image", targetImage)
		return false, nil
	}

	rolledOut := ds.Status.UpdatedNumberScheduled == ds.Status.DesiredNumberScheduled &&
		ds.Status.NumberReady == ds.Status.DesiredNumberScheduled
	return rolledOut, nil
}

// ensureCancelledState checks if the PatchPlan has been cancelled and updates the status
// accordingly. Returns true if cancelled, in which case Reconcile must stop scheduling new
// nodes; unlike pause this is a terminal phase and the reconciler does not requeue.
func (r *PatchPlanReconciler) ensureCancelledState(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan) (bool, error) {
	if !patchPlan.Spec.Cancelled {
		return false, nil
	}

	logger := log.FromContext(ctx)

	if patchPlan.Status.Phase != patchv1alpha1.PatchPhaseCancelled {
		original := patchPlan.DeepCopy()
		patchPlan.Status.Phase = patchv1alpha1.PatchPhaseCancelled
		patchPlan.Status.Message = "Patching cancelled by user"
		patchPlan.Status.CompletionTime = &metav1.Time{Time: time.Now()}

		if err := r.patchStatus(ctx, original, patchPlan); err != nil {
			logger.Error(err, "unable to update PatchPlan status to cancelled")
			return true, err
		}
	}

	logger.Info("PatchPlan is cancelled")
	return true, nil
}

// ensurePauseState checks if the PatchPlan is paused or resuming from pause
// and updates the status accordingly.
func (r *PatchPlanReconciler) ensurePauseState(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, failedCount int) (shouldPause bool, err error) {
	logger := log.FromContext(ctx)

	// Check if paused
	if patchPlan.Spec.Paused {
		if patchPlan.Status.Phase != patchv1alpha1.PatchPhasePaused {
			original := patchPlan.DeepCopy()
			patchPlan.Status.Phase = patchv1alpha1.PatchPhasePaused
			patchPlan.Status.Message = "Patching paused by user"

			if err := r.patchStatus(ctx, original, patchPlan); err != nil {
				logger.Error(err, "unable to update PatchPlan status to paused")
				return true, err
			}
		}
		logger.Info("PatchPlan is paused")
		return true, nil
	}

	// Resume from paused state
	if patchPlan.Status.Phase == patchv1alpha1.PatchPhasePaused {
		original := patchPlan.DeepCopy()
		patchPlan.Status.Phase = patchv1alpha1.PatchPhaseInProgress
		patchPlan.Status.Message = "Resuming patching operation"
		// Resuming a plan the failure policy paused accepts the failures seen so far
		if patchPlan.Spec.FailurePolicy.Type == patchv1alpha1.FailurePolicyPause &&
			patchPlan.Spec.FailurePolicy.Exceeded(newFailures(patchPlan, failedCount)) {
			patchPlan.Status.FailuresAcknowledged = failedCount
		}

		if err := r.patchStatus(ctx, original, patchPlan); err != nil {
			logger.Error(err, "unable to update PatchPlan status")
			return false, err
		}
	}

	return false, nil
}

// patchStatus applies a status patch to the PatchPlan, only updating changed fields.
func (r *PatchPlanReconciler) patchStatus(ctx context.Context, original, modified *patchv1alpha1.PatchPlan) error {
	return patchutil.PatchStatus(ctx, r.Status(), original, modified)
}

// listPatchJobs returns all PatchJobs belonging to a PatchPlan.
func (r *PatchPlanReconciler) listPatchJobs(ctx context.Context, planName string) ([]patchv1alpha1.PatchJob, error) {
	patchJobList := &patchv1alpha1.PatchJobList{}
	if err := r.List(ctx, patchJobList, client.MatchingFields{"spec.patchPlanRef": planName}); err != nil {
		return nil, err
	}
	return patchJobList.Items, nil
}

// summarize counts each unique node of the rollout once by the outcome of its PatchJob and fills
// the totals and per-group breakdown of the plan status.
func summarize(patchPlan *patchv1alpha1.PatchPlan, rollout *scheduling.Rollout, jobsByNode map[string]*patchv1alpha1.PatchJob, activeLeases map[string]int) JobCounts {
	var total JobCounts
	groups := make([]patchv1alpha1.GroupStatus, 0, len(rollout.Groups))

	for i := range rollout.Groups {
		g := &rollout.Groups[i]
		gs := patchv1alpha1.GroupStatus{
			Name:        g.Name,
			Concurrency: g.Concurrency,
			Active:      activeLeases[g.Name],
			Total:       len(g.Nodes),
		}
		for j := range g.Nodes {
			job, ok := jobsByNode[g.Nodes[j].Name]
			switch {
			case !ok:
				gs.Pending++
			case job.Status.Phase == patchv1alpha1.PatchJobPhaseCompleted:
				gs.Completed++
			case job.Status.Phase == patchv1alpha1.PatchJobPhaseFailed:
				gs.Failed++
			default:
				gs.Running++
			}
		}
		total.Completed += gs.Completed
		total.Failed += gs.Failed
		total.Running += gs.Running
		total.Pending += gs.Pending
		groups = append(groups, gs)
	}

	patchPlan.Status.TotalNodes = rollout.Total()
	patchPlan.Status.CompletedNodes = total.Completed
	patchPlan.Status.FailedNodes = total.Failed
	patchPlan.Status.RunningNodes = total.Running
	patchPlan.Status.PendingNodes = total.Pending
	patchPlan.Status.Groups = groups
	return total
}

// allNodesProcessed checks if all target nodes have been processed (completed or failed).
// Returns true if all nodes are done and the plan should complete successfully.
func (r *PatchPlanReconciler) allNodesProcessed(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, jobSummary JobCounts, totalNodes int) (bool, error) {
	processedCount := jobSummary.Completed + jobSummary.Failed
	if processedCount < totalNodes {
		return false, nil
	}

	logger := log.FromContext(ctx)

	if patchPlan.Status.Phase != patchv1alpha1.PatchPhaseCompleted {
		original := patchPlan.DeepCopy()
		patchPlan.Status.Phase = patchv1alpha1.PatchPhaseCompleted
		patchPlan.Status.Message = "all nodes processed"
		patchPlan.Status.CompletionTime = &metav1.Time{Time: time.Now()}

		if err := r.patchStatus(ctx, original, patchPlan); err != nil {
			logger.Error(err, "unable to update PatchPlan status to completed")
			return true, err
		}
	}

	logger.Info("PatchPlan completed", "total", totalNodes, "completed", jobSummary.Completed, "failed", jobSummary.Failed)
	return true, nil
}
