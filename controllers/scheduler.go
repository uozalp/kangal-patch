package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/nodeutil"
	"github.com/uozalp/kangal-patch/internal/scheduling"
)

// reasonInvalidStrategy is set on the PreflightPassed condition when strategy or groups can't be resolved.
const reasonInvalidStrategy = "InvalidStrategy"

// scheduleNodes creates PatchJobs for the first group in strategy.order that still has work. A group
// only starts once every earlier group has finished, and never holds more Leases than its concurrency.
func (r *PatchPlanReconciler) scheduleNodes(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, rollout *scheduling.Rollout, jobsByNode map[string]*patchv1alpha1.PatchJob, activeLeases map[string]int) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	for i := range rollout.Groups {
		group := &rollout.Groups[i]

		var pending []*corev1.Node
		finished := true
		for j := range group.Nodes {
			job, ok := jobsByNode[group.Nodes[j].Name]
			switch {
			case !ok:
				pending = append(pending, &group.Nodes[j])
				finished = false
			case !job.Status.Phase.IsTerminal():
				finished = false
			}
		}
		if finished {
			continue
		}

		if len(pending) == 0 {
			logger.Info("all nodes of the group are scheduled, waiting for them to finish", "group", group.Name)
			return ctrl.Result{RequeueAfter: requeueWhenJobsInProgress}, nil
		}

		free := group.Concurrency - activeLeases[group.Name]
		if free <= 0 {
			logger.Info("group is at max concurrency, waiting for a lease to be released",
				"group", group.Name, "active", activeLeases[group.Name], "concurrency", group.Concurrency)
			return ctrl.Result{RequeueAfter: requeueWhenAtMaxConcurrency}, nil
		}

		if wait := delayRemaining(patchPlan, jobsByNode); wait > 0 {
			logger.Info("waiting for delayBetweenNodes", "remaining", wait)
			return ctrl.Result{RequeueAfter: wait}, nil
		}

		if err := r.markInProgress(ctx, patchPlan); err != nil {
			return ctrl.Result{}, err
		}

		created := 0
		for _, node := range pending[:min(free, len(pending))] {
			wait, err := r.waitForControlPlane(ctx, patchPlan, rollout, jobsByNode, node)
			if err != nil {
				return ctrl.Result{}, err
			}
			if wait > 0 {
				if created == 0 {
					return ctrl.Result{RequeueAfter: wait}, nil
				}
				break
			}

			if err := r.createPatchJob(ctx, patchPlan, node, group.Name); err != nil {
				return ctrl.Result{}, err
			}
			activeLeases[group.Name]++
			created++
		}

		return ctrl.Result{RequeueAfter: requeueForNextNode}, nil
	}

	// Every group has finished; allNodesProcessed completes the plan once the counts catch up
	return ctrl.Result{RequeueAfter: requeueWhenJobsInProgress}, nil
}

// delayRemaining is how much of delayBetweenNodes is left, counted from the later of the last node
// being scheduled and the last job finishing.
func delayRemaining(patchPlan *patchv1alpha1.PatchPlan, jobsByNode map[string]*patchv1alpha1.PatchJob) time.Duration {
	var last time.Time
	if t := patchPlan.Status.LastNodeScheduledAt; t != nil {
		last = t.Time
	}
	for _, job := range jobsByNode {
		if t := job.Status.CompletionTime; t != nil && t.After(last) {
			last = t.Time
		}
	}
	if last.IsZero() {
		return 0
	}
	return time.Until(last.Add(patchPlan.Spec.DelayBetweenNodes.Duration))
}

// markInProgress moves the plan to InProgress before a job is created (also resumes after a failure or completion).
func (r *PatchPlanReconciler) markInProgress(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan) error {
	switch patchPlan.Status.Phase {
	case patchv1alpha1.PatchPhasePending, patchv1alpha1.PatchPhaseFailed, patchv1alpha1.PatchPhaseCompleted,
		patchv1alpha1.PatchPhaseCancelled, "":
	default:
		return nil
	}

	return r.updateStatus(ctx, patchPlan, func(s *patchv1alpha1.PatchPlanStatus) {
		s.Phase = patchv1alpha1.PatchPhaseInProgress
		if s.StartTime == nil {
			s.StartTime = &metav1.Time{Time: time.Now()}
		}
		// Clear completion time if resuming after completion/failure
		s.CompletionTime = nil
	})
}

// waitForControlPlane gates worker nodes on a Kubernetes upgrade: every control plane node
// (apiserver, kubelet) must have completed before any worker's kubelet is bumped - a kubelet must
// never run newer than the apiserver it connects to. It returns how long to wait, or zero when the
// node may be scheduled. kube-proxy is rolled out earlier by ensureKubeProxy.
func (r *PatchPlanReconciler) waitForControlPlane(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, rollout *scheduling.Rollout, jobsByNode map[string]*patchv1alpha1.PatchJob, node *corev1.Node) (time.Duration, error) {
	if patchPlan.Spec.Target.KubernetesVersion == "" || nodeutil.IsControlPlane(node) {
		return 0, nil
	}

	logger := log.FromContext(ctx)

	controlPlane := rollout.ControlPlaneNodes()
	if len(controlPlane) == 0 {
		return 0, nil
	}
	if !allNodesCompleted(controlPlane, jobsByNode) {
		for i := range controlPlane {
			job, ok := jobsByNode[controlPlane[i].Name]
			if !ok || job.Status.Phase != patchv1alpha1.PatchJobPhaseFailed {
				continue
			}
			// A Failed job never becomes Completed, so say why the workers are not moving.
			msg := fmt.Sprintf("worker rollout blocked: control plane node %s failed", controlPlane[i].Name)
			if err := r.setMessage(ctx, patchPlan, msg); err != nil {
				return 0, err
			}
			return requeueWhenJobsInProgress, nil
		}
		logger.Info("waiting for control plane nodes to finish kubernetes upgrade before patching workers")
		return requeueWhenJobsInProgress, nil
	}

	return 0, nil
}

// ensureKubeProxy upgrades kube-proxy once every control plane job has completed, whether or not the
// rollout contains workers. It returns how long to wait for the rollout, or zero when nothing is left to do.
func (r *PatchPlanReconciler) ensureKubeProxy(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, rollout *scheduling.Rollout, jobsByNode map[string]*patchv1alpha1.PatchJob) (time.Duration, error) {
	if patchPlan.Spec.Target.KubernetesVersion == "" || patchPlan.Status.KubeProxyUpgraded {
		return 0, nil
	}

	controlPlane := rollout.ControlPlaneNodes()
	if len(controlPlane) == 0 || !allNodesCompleted(controlPlane, jobsByNode) {
		return 0, nil
	}

	done, err := r.ensureKubeProxyUpgraded(ctx, patchPlan.Spec.Target.KubernetesVersion)
	if err != nil {
		return 0, err
	}
	if !done {
		message := "Waiting for the kube-proxy rollout"
		timeout := patchPlan.Spec.KubernetesUpgradeTimeout.Duration
		if timeout <= 0 {
			timeout = defaultKubernetesUpgradeTimeout
		}
		// shortcut: only reports a stuck rollout, never fails the plan; add a failure path if this proves common.
		if time.Since(lastCompletion(controlPlane, jobsByNode)) > timeout {
			message = fmt.Sprintf("kube-proxy rollout not finished %s after the control plane completed; check the kube-proxy DaemonSet in %s", timeout, kubeProxyNamespace)
		}
		if err := r.setMessage(ctx, patchPlan, message); err != nil {
			return 0, err
		}
		log.FromContext(ctx).Info("waiting for kube-proxy rollout")
		return requeueForNextNode, nil
	}

	return 0, r.updateStatus(ctx, patchPlan, func(s *patchv1alpha1.PatchPlanStatus) { s.KubeProxyUpgraded = true })
}

// lastCompletion is the latest completion time of the nodes' jobs.
func lastCompletion(nodes []corev1.Node, jobsByNode map[string]*patchv1alpha1.PatchJob) time.Time {
	var last time.Time
	for i := range nodes {
		if job, ok := jobsByNode[nodes[i].Name]; ok && job.Status.CompletionTime != nil && job.Status.CompletionTime.After(last) {
			last = job.Status.CompletionTime.Time
		}
	}
	return last
}

// createPatchJob creates the PatchJob of a node together with the Lease holding its concurrency slot.
func (r *PatchPlanReconciler) createPatchJob(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, node *corev1.Node, group string) error {
	logger := log.FromContext(ctx)

	patchJob := &patchv1alpha1.PatchJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-%s", patchPlan.Name, node.Name),
			Labels: map[string]string{
				patchv1alpha1.LabelPatchPlan: patchPlan.Name,
				patchv1alpha1.LabelGroup:     group,
				patchv1alpha1.LabelNode:      node.Name,
			},
		},
		Spec: patchv1alpha1.PatchJobSpec{
			NodeName:     node.Name,
			Target:       patchPlan.Spec.Target,
			PatchPlanRef: patchPlan.Name,
			Group:        group,
		},
	}

	// Set owner reference so PatchJob is deleted when PatchPlan is deleted
	if err := ctrl.SetControllerReference(patchPlan, patchJob, r.Scheme); err != nil {
		logger.Error(err, "unable to set owner reference on PatchJob")
		return err
	}

	if err := r.Create(ctx, patchJob); err != nil {
		if errors.IsAlreadyExists(err) {
			// Missing from a lagging cache; the next reconcile restores its Lease if needed
			return nil
		}
		logger.Error(err, "unable to create PatchJob", "node", node.Name)
		return err
	}

	if err := r.acquireLease(ctx, patchJob, group); err != nil {
		return err
	}

	if err := r.updateStatus(ctx, patchPlan, func(s *patchv1alpha1.PatchPlanStatus) {
		s.LastNodeScheduledAt = &metav1.Time{Time: time.Now()}
		s.Message = fmt.Sprintf("Patching node %s (group %s)", node.Name, group)
	}); err != nil {
		logger.Error(err, "unable to update PatchPlan status")
		return err
	}

	logger.Info("Created PatchJob", "node", node.Name, "group", group, "job", patchJob.Name)
	return nil
}

// failInvalidStrategy records that groups or strategy can't be resolved and keeps retrying so that
// fixing the spec self-heals. Nothing is scheduled meanwhile.
func (r *PatchPlanReconciler) failInvalidStrategy(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, cause error) (ctrl.Result, error) {
	log.FromContext(ctx).Info("invalid groups or strategy", "error", cause.Error())

	if err := r.updateStatus(ctx, patchPlan, func(s *patchv1alpha1.PatchPlanStatus) {
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type:               patchv1alpha1.ConditionPreflightPassed,
			Status:             metav1.ConditionFalse,
			Reason:             reasonInvalidStrategy,
			Message:            cause.Error(),
			ObservedGeneration: patchPlan.Generation,
		})
		s.Phase = patchv1alpha1.PatchPhaseFailed
		s.Message = "Invalid strategy: " + cause.Error()
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueWhenPreflightFailed}, nil
}
