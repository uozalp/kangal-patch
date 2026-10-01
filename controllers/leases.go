package controllers

import (
	"context"
	"fmt"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/scheduling"
)

// groupOfJob returns the group whose concurrency slot the job occupies.
func groupOfJob(job *patchv1alpha1.PatchJob, rollout *scheduling.Rollout) string {
	if job.Spec.Group != "" {
		return job.Spec.Group
	}
	group, _ := rollout.GroupOf(job.Spec.NodeName)
	return group
}

// syncLeases makes the Leases of the plan mirror its unfinished PatchJobs: the Lease of a finished
// PatchJob is released, and an unfinished PatchJob without one gets it back. It returns the number
// of held Leases per group, which is the live concurrency.
func (r *PatchPlanReconciler) syncLeases(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, rollout *scheduling.Rollout, jobs []patchv1alpha1.PatchJob) (map[string]int, error) {
	logger := log.FromContext(ctx)

	var leaseList coordinationv1.LeaseList
	if err := r.List(ctx, &leaseList,
		client.InNamespace(r.Namespace),
		client.MatchingLabels{patchv1alpha1.LabelPatchPlan: patchPlan.Name},
	); err != nil {
		return nil, fmt.Errorf("failed to list leases: %w", err)
	}

	jobsByName := make(map[string]*patchv1alpha1.PatchJob, len(jobs))
	for i := range jobs {
		jobsByName[jobs[i].Name] = &jobs[i]
	}

	active := map[string]int{}
	held := make(map[string]struct{}, len(leaseList.Items))

	for i := range leaseList.Items {
		lease := &leaseList.Items[i]
		jobName := lease.Labels[patchv1alpha1.LabelPatchJob]
		job, ok := jobsByName[jobName]

		// A Lease whose job is gone is garbage collected through its owner reference; the job may
		// merely be missing from a lagging cache, so it is left alone here.
		if !ok {
			continue
		}
		if job.Status.Phase.IsTerminal() {
			if err := r.Delete(ctx, lease); err != nil && !errors.IsNotFound(err) {
				return nil, fmt.Errorf("failed to release lease %s: %w", lease.Name, err)
			}
			logger.Info("released lease", "lease", lease.Name, "node", job.Spec.NodeName, "phase", job.Status.Phase)
			continue
		}

		active[groupOfJob(job, rollout)]++
		held[jobName] = struct{}{}
	}

	for i := range jobs {
		job := &jobs[i]
		if job.Status.Phase.IsTerminal() || !job.DeletionTimestamp.IsZero() {
			continue
		}
		if _, ok := held[job.Name]; ok {
			continue
		}

		group := groupOfJob(job, rollout)
		if err := r.acquireLease(ctx, job, group); err != nil {
			return nil, err
		}
		active[group]++
	}

	return active, nil
}

// acquireLease creates the Lease holding the concurrency slot of an unfinished PatchJob.
func (r *PatchPlanReconciler) acquireLease(ctx context.Context, job *patchv1alpha1.PatchJob, group string) error {
	logger := log.FromContext(ctx)

	acquired := metav1.NewMicroTime(metav1.Now().Time)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name,
			Namespace: r.Namespace,
			Labels: map[string]string{
				patchv1alpha1.LabelPatchPlan: job.Spec.PatchPlanRef,
				patchv1alpha1.LabelGroup:     group,
				patchv1alpha1.LabelNode:      job.Spec.NodeName,
				patchv1alpha1.LabelPatchJob:  job.Name,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &job.Name,
			AcquireTime:    &acquired,
		},
	}

	if err := ctrl.SetControllerReference(job, lease, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on lease %s: %w", lease.Name, err)
	}

	if err := r.Create(ctx, lease); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create lease %s: %w", lease.Name, err)
	}

	logger.Info("acquired lease", "lease", lease.Name, "group", group, "node", job.Spec.NodeName)
	return nil
}
