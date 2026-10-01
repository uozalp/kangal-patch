package controllers

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

// requeueWhenRetentionBlocked is how often a due history purge is retried while jobs still run.
const requeueWhenRetentionBlocked = time.Hour

func isFinishedPhase(phase patchv1alpha1.PatchPhase) bool {
	return phase == patchv1alpha1.PatchPhaseCompleted ||
		phase == patchv1alpha1.PatchPhaseFailed ||
		phase == patchv1alpha1.PatchPhaseCancelled
}

// terminalResult is the result for a plan that has nothing left to schedule: it only needs to wake
// up again when its history is due to be purged.
func terminalResult(patchPlan *patchv1alpha1.PatchPlan) ctrl.Result {
	if patchPlan.Status.CompletionTime == nil {
		return ctrl.Result{}
	}
	wait := time.Until(patchPlan.Status.CompletionTime.Add(patchPlan.HistoryRetention()))
	if wait <= 0 {
		wait = requeueWhenRetentionBlocked
	}
	return ctrl.Result{RequeueAfter: wait}
}

// applyRetention removes the PatchJobs of a finished plan once spec.retention.history has passed
// since it finished, and reports whether the plan is purged and therefore frozen. Only terminal
// PatchJobs are removed; while one still runs nothing is deleted. The plan is frozen afterwards
// because without its jobs the scheduler would patch every node again.
func (r *PatchPlanReconciler) applyRetention(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan) (bool, error) {
	if !isFinishedPhase(patchPlan.Status.Phase) || patchPlan.Status.CompletionTime == nil {
		return false, nil
	}
	if patchPlan.Status.HistoryPurged {
		return true, nil
	}
	if time.Since(patchPlan.Status.CompletionTime.Time) < patchPlan.HistoryRetention() {
		return false, nil
	}

	jobs, err := r.listPatchJobs(ctx, patchPlan.Name)
	if err != nil {
		return false, err
	}
	for i := range jobs {
		if !jobs[i].Status.Phase.IsTerminal() {
			return false, nil
		}
	}

	for i := range jobs {
		if err := r.Delete(ctx, &jobs[i]); err != nil && !errors.IsNotFound(err) {
			return false, err
		}
	}

	original := patchPlan.DeepCopy()
	patchPlan.Status.HistoryPurged = true
	if err := r.patchStatus(ctx, original, patchPlan); err != nil {
		return false, err
	}

	log.FromContext(ctx).Info("purged PatchJob history", "jobs", len(jobs), "retention", patchPlan.HistoryRetention())
	return true, nil
}
