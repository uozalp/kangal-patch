package controllers

import (
	"context"
	"testing"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

func slackChildPlan() *patchv1alpha1.PatchPlan {
	plan := testPlan(patchv1alpha1.PatchPlanSpec{
		Target:        patchv1alpha1.TargetSpec{TalosVersion: "v1.13.5"},
		Strategy:      patchv1alpha1.StrategySpec{Order: []string{"controlPlane", "workers"}},
		FailurePolicy: patchv1alpha1.FailurePolicySpec{Type: patchv1alpha1.FailurePolicyHalt, MaxFailures: 1},
	})
	plan.Annotations = map[string]string{
		patchv1alpha1.AnnotationSlackChannel: "C1",
		patchv1alpha1.AnnotationSlackThread:  "1.1",
	}
	plan.Status.Phase = patchv1alpha1.PatchPhaseInProgress
	return plan
}

func TestReportRolloutPostsOnceThenEditsOnChange(t *testing.T) {
	ctx := context.Background()
	cp, worker := testNode("cp-1", cpLabel, ""), testNode("w-1")
	plan := slackChildPlan()
	job := jobFor("cp-1", "controlPlane", patchv1alpha1.PatchJobPhaseDraining)
	r, n := approvalTestReconciler(t, plan, &cp, &worker, job)

	for range 2 {
		if err := r.reportRollout(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	if n.rolloutPosts != 1 || n.rolloutUpdates != 0 || plan.Annotations[patchv1alpha1.AnnotationSlackStatusTS] != "2.1" {
		t.Fatalf("posts=%d updates=%d annotations=%v, want one post and no edit while nothing changed", n.rolloutPosts, n.rolloutUpdates, plan.Annotations)
	}
	if got := n.lastRollout.Nodes; len(got) != 2 || got[0].Name != "cp-1" || got[0].Phase != patchv1alpha1.PatchJobPhaseDraining || got[1].Phase != patchv1alpha1.PatchJobPhasePending {
		t.Fatalf("nodes = %+v", got)
	}

	// A phase change edits the same message.
	job.Status.Phase = patchv1alpha1.PatchJobPhaseUpgrading
	if err := r.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := r.reportRollout(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if n.rolloutPosts != 1 || n.rolloutUpdates != 1 {
		t.Fatalf("posts=%d updates=%d, want the existing message edited", n.rolloutPosts, n.rolloutUpdates)
	}
}

func TestReportRolloutNeverRepostsAndAnnouncesEachFailureOnce(t *testing.T) {
	ctx := context.Background()
	cp := testNode("cp-1", cpLabel, "")
	plan := slackChildPlan()
	plan.Status.Phase = patchv1alpha1.PatchPhaseFailed
	plan.Status.Message = "failure policy Halt: 1 failed node(s), maxFailures 1"
	r, n := approvalTestReconciler(t, plan, &cp, jobFor("cp-1", "controlPlane", patchv1alpha1.PatchJobPhaseFailed))

	// A stale copy of the plan, as a lagging cache would hand out, must not cause a second post.
	for range 3 {
		stale := slackChildPlan()
		stale.Status = plan.Status
		stale.Name, stale.UID = plan.Name, plan.UID
		stale.ResourceVersion = ""
		if err := r.reportRollout(ctx, stale); err != nil {
			t.Fatal(err)
		}
	}
	if n.rolloutPosts != 1 || n.failurePosts != 1 || !n.lastRollout.Halted {
		t.Fatalf("posts=%d failurePosts=%d halted=%t, want one status message and one failure notice", n.rolloutPosts, n.failurePosts, n.lastRollout.Halted)
	}

	plain := slackChildPlan()
	plain.Name, plain.Annotations = "plain", nil
	r, n = approvalTestReconciler(t, plain, &cp)
	if err := r.reportRollout(ctx, plain); err != nil || n.rolloutPosts != 0 {
		t.Fatalf("err=%v posts=%d, a plan without an approval thread must not post", err, n.rolloutPosts)
	}
}
