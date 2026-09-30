package controllers

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
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

type JobCounts struct {
	Completed  int
	Failed     int
	InProgress int
}

// Requeue intervals for different reconciliation scenarios
const (
	requeueWhenLeasesFull       = 2 * time.Second  // waiting for lease slots to free up
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

	// Clean up expired leases first
	if err := r.cleanupExpiredLeases(ctx, patchPlan.Name); err != nil {
		return ctrl.Result{}, err
	}

	// Get list of all nodes matching selector
	nodes, err := nodeutil.ListMatchingNodes(ctx, r.Client, patchPlan.Spec.NodeSelector)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Split nodes by role
	controlPlaneNodes, workerNodes := nodeutil.SplitByRole(nodes)

	// Order target nodes based on spec
	targetNodes := nodeutil.OrderTargetNodes(controlPlaneNodes, workerNodes, patchPlan.Spec)

	// Get list of existing PatchJobs and compute counts
	jobsByNode, jobSummary, err := r.fetchPatchJobSummary(ctx, patchPlan.Name)
	if err != nil {
		logger.Error(err, "unable to list PatchJobs")
		return ctrl.Result{}, err
	}

	// Update status counts and total nodes
	original := patchPlan.DeepCopy()
	patchPlan.Status.TotalNodes = len(targetNodes)
	patchPlan.Status.TargetTalosVersion = patchPlan.Spec.Target.TalosVersion
	patchPlan.Status.TargetKubernetesVersion = patchPlan.Spec.Target.KubernetesVersion
	patchPlan.Status.CompletedNodes = jobSummary.Completed
	patchPlan.Status.FailedNodes = jobSummary.Failed

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
		return ctrl.Result{}, nil
	}

	// Handle pause/resume state
	paused, err := r.ensurePauseState(ctx, &patchPlan)
	if err != nil {
		return ctrl.Result{}, err
	}
	if paused {
		return ctrl.Result{RequeueAfter: requeueWhenPaused}, nil
	}

	// Validate the plan once before any PatchJob exists; nothing is scheduled until it passes
	preflightPassed, err := r.ensurePreflight(ctx, &patchPlan, targetNodes, jobsByNode)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !preflightPassed {
		return ctrl.Result{RequeueAfter: requeueWhenPreflightFailed}, nil
	}

	// Check if all nodes are processed
	allProcessed, err := r.allNodesProcessed(ctx, &patchPlan, jobSummary, len(targetNodes))
	if err != nil {
		return ctrl.Result{}, err
	}
	if allProcessed {
		return ctrl.Result{}, nil
	}

	// Check MaxFailures
	failureBudgetExceeded, err := r.failureBudgetExceeded(ctx, &patchPlan, jobSummary.Failed)
	if err != nil {
		return ctrl.Result{}, err
	}
	if failureBudgetExceeded {
		return ctrl.Result{}, nil
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

	// Check if we should wait for leases to expire
	leaseCapacityReached, err := r.leaseCapacityReached(ctx, patchPlan.Name, patchPlan.Spec.MaxConcurrency)
	if err != nil {
		return ctrl.Result{}, err
	}
	if leaseCapacityReached {
		return ctrl.Result{RequeueAfter: requeueWhenLeasesFull}, nil
	}

	// Check concurrency limit
	if r.maxConcurrencyReached(ctx, &patchPlan, jobSummary.InProgress) {
		return ctrl.Result{RequeueAfter: requeueWhenAtMaxConcurrency}, nil
	}

	// Find next node to patch
	nextNode := nodeutil.FindNextUnpatchedNode(targetNodes, jobsByNode)
	if nextNode == nil {
		// No more nodes to patch, wait for in-progress jobs
		logger.Info("no more nodes to schedule, waiting for in-progress jobs", "inProgress", jobSummary.InProgress)

		return ctrl.Result{RequeueAfter: requeueWhenJobsInProgress}, nil
	}

	// Kubernetes upgrades must fully land on every control plane node (apiserver, kubelet) before
	// any worker's kubelet is bumped or the cluster-wide kube-proxy DaemonSet is upgraded - a
	// kubelet must never run newer than the apiserver it connects to.
	if patchPlan.Spec.Target.KubernetesVersion != "" && len(controlPlaneNodes) > 0 && !nodeutil.IsControlPlane(nextNode) {
		if !allNodesCompleted(controlPlaneNodes, jobsByNode) {
			logger.Info("waiting for control plane nodes to finish kubernetes upgrade before patching workers")
			return ctrl.Result{RequeueAfter: requeueWhenJobsInProgress}, nil
		}

		if !patchPlan.Status.KubeProxyUpgraded {
			done, err := r.ensureKubeProxyUpgraded(ctx, patchPlan.Spec.Target.KubernetesVersion)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !done {
				logger.Info("waiting for kube-proxy rollout before patching workers")
				return ctrl.Result{RequeueAfter: requeueForNextNode}, nil
			}

			kubeProxyOriginal := patchPlan.DeepCopy()
			patchPlan.Status.KubeProxyUpgraded = true
			if err := r.patchStatus(ctx, kubeProxyOriginal, &patchPlan); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Set phase to InProgress before creating job (handles retry scenarios)
	if patchPlan.Status.Phase == patchv1alpha1.PatchPhasePending ||
		patchPlan.Status.Phase == patchv1alpha1.PatchPhaseFailed ||
		patchPlan.Status.Phase == patchv1alpha1.PatchPhaseCompleted ||
		patchPlan.Status.Phase == patchv1alpha1.PatchPhaseCancelled ||
		patchPlan.Status.Phase == "" {
		original := patchPlan.DeepCopy()
		patchPlan.Status.Phase = patchv1alpha1.PatchPhaseInProgress
		if patchPlan.Status.StartTime == nil {
			patchPlan.Status.StartTime = &metav1.Time{Time: time.Now()}
		}
		// Clear completion time if resuming after completion/failure
		patchPlan.Status.CompletionTime = nil

		if err := r.patchStatus(ctx, original, &patchPlan); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Create PatchJob for the next node
	if err := r.createPatchJob(ctx, &patchPlan, nextNode); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue to schedule next node
	return ctrl.Result{RequeueAfter: requeueForNextNode}, nil
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

// failureBudgetExceeded checks if the PatchPlan has reached the maximum allowed failures.
// Returns true if max failures reached and the plan should stop.
func (r *PatchPlanReconciler) failureBudgetExceeded(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, failedCount int) (bool, error) {
	if failedCount < patchPlan.Spec.MaxFailures {
		return false, nil
	}

	logger := log.FromContext(ctx)

	if patchPlan.Status.Phase != patchv1alpha1.PatchPhaseFailed {
		original := patchPlan.DeepCopy()
		patchPlan.Status.Phase = patchv1alpha1.PatchPhaseFailed
		patchPlan.Status.Message = fmt.Sprintf("maximum failures reached: %d/%d", failedCount, patchPlan.Spec.MaxFailures)
		patchPlan.Status.CompletionTime = &metav1.Time{Time: time.Now()}

		if err := r.patchStatus(ctx, original, patchPlan); err != nil {
			return true, err
		}
	}

	logger.Info("PatchPlan failed due to max failures", "failed", failedCount, "max", patchPlan.Spec.MaxFailures)
	return true, nil
}

// maxConcurrencyReached verifies if the current number of in-progress jobs
// has reached the maximum concurrency limit.
// Returns true if at capacity and scheduling should wait.
func (r *PatchPlanReconciler) maxConcurrencyReached(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, inProgressCount int) bool {
	if inProgressCount < patchPlan.Spec.MaxConcurrency {
		return false
	}

	logger := log.FromContext(ctx)
	logger.Info("at max concurrency, waiting for job completion",
		"inProgress", inProgressCount,
		"max", patchPlan.Spec.MaxConcurrency)

	return true
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

// cleanupExpiredLeases removes all expired leases for the given PatchPlan.
// This ensures expired leases don't clutter the system.
func (r *PatchPlanReconciler) cleanupExpiredLeases(ctx context.Context, planName string) error {
	logger := log.FromContext(ctx)

	leaseList := &coordinationv1.LeaseList{}
	leaseLabels := client.MatchingLabels{"kangalpatch.ozalp.dk/patchplan": planName}
	if err := r.List(ctx, leaseList, leaseLabels, client.InNamespace(r.Namespace)); err != nil {
		logger.Error(err, "unable to list leases")
		return err
	}

	now := time.Now()
	deletedCount := 0

	for i := range leaseList.Items {
		lease := &leaseList.Items[i]
		if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
			continue
		}

		expiryTime := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
		if now.After(expiryTime) {
			if err := r.Delete(ctx, lease); err != nil {
				logger.Error(err, "unable to delete expired lease", "lease", lease.Name)
				return err
			}
			deletedCount++
		}
	}

	if deletedCount > 0 {
		logger.Info("deleted expired leases", "count", deletedCount)
	}

	return nil
}

// leaseCapacityReached checks if scheduling should wait for leases to expire.
// Returns true if all lease slots are occupied and scheduling must wait.
func (r *PatchPlanReconciler) leaseCapacityReached(ctx context.Context, planName string, maxConcurrency int) (bool, error) {
	logger := log.FromContext(ctx)

	// Get leases for this plan
	leaseList := &coordinationv1.LeaseList{}
	leaseLabels := client.MatchingLabels{"kangalpatch.ozalp.dk/patchplan": planName}
	if err := r.List(ctx, leaseList, leaseLabels, client.InNamespace(r.Namespace)); err != nil {
		logger.Error(err, "unable to list leases")
		return false, err
	}

	// Count active (non-expired) leases
	now := time.Now()
	activeLeases := 0
	for i := range leaseList.Items {
		lease := &leaseList.Items[i]
		if lease.Spec.RenewTime != nil && lease.Spec.LeaseDurationSeconds != nil {
			expiryTime := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
			if now.Before(expiryTime) {
				activeLeases++
			}
		}
	}

	// Check if we need to wait
	availableSlots := maxConcurrency - activeLeases
	shouldWait := availableSlots <= 0

	if shouldWait {
		logger.Info("waiting for lease to expire", "activeLeases", activeLeases, "max", maxConcurrency)
	}

	return shouldWait, nil
}

// createSchedulingLease creates a rate-limiting lease for node scheduling.
// The lease duration equals delayBetweenNodes and prevents scheduling the next node too quickly.
func (r *PatchPlanReconciler) createSchedulingLease(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, patchJob *patchv1alpha1.PatchJob, nodeName string) error {
	logger := log.FromContext(ctx)

	delayBetweenNodes := patchPlan.Spec.DelayBetweenNodes.Duration

	leaseDurationSeconds := int32(delayBetweenNodes.Seconds())
	renewTime := metav1.NewMicroTime(time.Now())

	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-scheduling", patchPlan.Name, nodeName),
			Namespace: r.Namespace,
			Labels: map[string]string{
				"kangalpatch.ozalp.dk/patchplan": patchPlan.Name,
				"kangalpatch.ozalp.dk/node":      nodeName,
				"kangalpatch.ozalp.dk/patchjob":  patchJob.Name,
				"kangalpatch.ozalp.dk/group":     "xxx", // TODO: hardcoded until group support is added
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &patchJob.Name,
			LeaseDurationSeconds: &leaseDurationSeconds,
			RenewTime:            &renewTime,
		},
	}

	if err := ctrl.SetControllerReference(patchJob, lease, r.Scheme); err != nil {
		logger.Error(err, "unable to set owner reference on Lease")
		return err
	}

	if err := r.Create(ctx, lease); err != nil {
		logger.Error(err, "unable to create Lease", "lease", lease.Name)
		return err
	}

	logger.Info("created rate-limiting Lease", "lease", lease.Name, "duration", delayBetweenNodes)
	return nil
}

// createPatchJob creates a new PatchJob for the given node and updates the PatchPlan status.
// Returns the created PatchJob or an error.
func (r *PatchPlanReconciler) createPatchJob(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, node *corev1.Node) error {
	logger := log.FromContext(ctx)

	// Create PatchJob
	patchJob := &patchv1alpha1.PatchJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-%s", patchPlan.Name, node.Name),
			Labels: map[string]string{
				"kangalpatch.ozalp.dk/patchplan": patchPlan.Name,
				"kangalpatch.ozalp.dk/node":      node.Name,
			},
		},
		Spec: patchv1alpha1.PatchJobSpec{
			NodeName:     node.Name,
			Target:       patchPlan.Spec.Target,
			PatchPlanRef: patchPlan.Name,
		},
	}

	// Set owner reference so PatchJob is deleted when PatchPlan is deleted
	if err := ctrl.SetControllerReference(patchPlan, patchJob, r.Scheme); err != nil {
		logger.Error(err, "unable to set owner reference on PatchJob")
		return err
	}

	if err := r.Create(ctx, patchJob); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "unable to create PatchJob", "node", node.Name)
			return err
		}
	}

	// Update PatchPlan status before creating the job
	original := patchPlan.DeepCopy()
	patchPlan.Status.LastNodeScheduledAt = &metav1.Time{Time: metav1.Now().Time}
	patchPlan.Status.Message = fmt.Sprintf("Patching node %s", node.Name)

	if err := r.patchStatus(ctx, original, patchPlan); err != nil {
		logger.Error(err, "unable to update PatchPlan status")
		return err
	}

	logger.Info("Created PatchJob", "node", node.Name, "job", patchJob.Name)

	// Create rate-limiting lease to enforce delay between node scheduling
	if err := r.createSchedulingLease(ctx, patchPlan, patchJob, node.Name); err != nil {
		logger.Error(err, "failed to create scheduling lease", "node", node.Name)
		// Note: We don't return error here since the PatchJob was created successfully
		// The lease creation failure will just mean less rate limiting
	}

	return nil
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
func (r *PatchPlanReconciler) ensurePauseState(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan) (shouldPause bool, err error) {
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

// fetchPatchJobSummary retrieves all PatchJobs for a PatchPlan and counts them by status phase.
// It returns a map of node names to their corresponding PatchJobs and aggregated counts.
func (r *PatchPlanReconciler) fetchPatchJobSummary(ctx context.Context, planName string) (map[string]*patchv1alpha1.PatchJob, JobCounts, error) {
	patchJobList := &patchv1alpha1.PatchJobList{}
	if err := r.List(ctx, patchJobList, client.MatchingFields{"spec.patchPlanRef": planName}); err != nil {
		return nil, JobCounts{}, err
	}

	jobsByNode := make(map[string]*patchv1alpha1.PatchJob)
	var counts JobCounts

	for i := range patchJobList.Items {
		job := &patchJobList.Items[i]
		jobsByNode[job.Spec.NodeName] = job

		switch job.Status.Phase {
		case patchv1alpha1.PatchJobPhaseCompleted:
			counts.Completed++
		case patchv1alpha1.PatchJobPhaseFailed:
			counts.Failed++
		case patchv1alpha1.PatchJobPhasePending,
			patchv1alpha1.PatchJobPhaseDraining,
			patchv1alpha1.PatchJobPhaseUpgrading,
			patchv1alpha1.PatchJobPhaseRebooting:
			counts.InProgress++
		}
	}

	return jobsByNode, counts, nil
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
