package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/release"
	"github.com/uozalp/kangal-patch/internal/rollout"
	"github.com/uozalp/kangal-patch/internal/slack"
)

type fakeNotifier struct {
	posts, updates int
	failNext       bool

	rolloutPosts, rolloutUpdates, failurePosts int
	lastRollout                                rollout.Snapshot
}

func (f *fakeNotifier) PostRollout(_ context.Context, _, _ string, snap rollout.Snapshot) (string, error) {
	f.rolloutPosts++
	f.lastRollout = snap
	return "2.1", nil
}

func (f *fakeNotifier) UpdateRollout(_ context.Context, _, _ string, snap rollout.Snapshot) error {
	f.rolloutUpdates++
	f.lastRollout = snap
	return nil
}

func (f *fakeNotifier) PostFailure(context.Context, string, string, rollout.Snapshot, []string) error {
	f.failurePosts++
	return nil
}

func (f *fakeNotifier) Required() int { return 1 }

func (f *fakeNotifier) Post(context.Context, *patchv1alpha1.PatchPlan, *patchv1alpha1.ApprovalRecord, *release.Release) (string, string, error) {
	if f.failNext {
		f.failNext = false
		return "", "", context.DeadlineExceeded
	}
	f.posts++
	return "C1", "1.1", nil
}

func (f *fakeNotifier) Update(context.Context, *patchv1alpha1.PatchPlan, *patchv1alpha1.ApprovalRecord, *release.Release) error {
	f.updates++
	return nil
}

type noReleases struct{}

func (noReleases) List(context.Context) ([]release.Release, error) { return nil, nil }

func approvalTemplate() *patchv1alpha1.PatchPlan {
	plan := autoUpdateTemplate()
	plan.Spec.Target.AutoUpdate.Approval = &patchv1alpha1.ApprovalSpec{Slack: &patchv1alpha1.SlackApprovalSpec{Enabled: true}}
	return plan
}

func approvalTestReconciler(t *testing.T, plan *patchv1alpha1.PatchPlan, others ...client.Object) (*PatchPlanReconciler, *fakeNotifier) {
	t.Helper()
	r := testReconciler(t, append([]client.Object{plan}, others...)...)
	n := &fakeNotifier{}
	r.Slack, r.APIReader, r.Releases = n, r.Client, noReleases{}
	return r, n
}

func mustVersion(t *testing.T, s string) release.Version {
	t.Helper()
	v, err := release.ParseVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestApprovalAnnounceDecideAndCreateChild(t *testing.T) {
	ctx := context.Background()
	plan := approvalTemplate()
	r, n := approvalTestReconciler(t, plan)
	target := &release.Release{Version: mustVersion(t, "v1.13.5")}

	// A failed post keeps the announcement in status and is retried without a new approval.
	n.failNext = true
	if err := r.announceRelease(ctx, plan, target, mustVersion(t, "v1.13.4")); err == nil {
		t.Fatal("post failure not reported")
	}
	if _, err := r.syncApproval(ctx, plan, true); err != nil || n.posts != 1 {
		t.Fatalf("err=%v posts=%d, want the retry to post", err, n.posts)
	}
	cur := currentApproval(plan)
	if cur == nil || cur.State != patchv1alpha1.ApprovalPending || cur.Slack.MessageTS != "1.1" || cur.ID != slack.ApprovalID(plan.UID, "v1.13.5") {
		t.Fatalf("current = %+v", cur)
	}

	// Announcing the same release again does not post twice.
	if err := r.announceRelease(ctx, plan, target, mustVersion(t, "v1.13.4")); err != nil || n.posts != 1 {
		t.Fatalf("err=%v posts=%d", err, n.posts)
	}

	// The approval is applied immediately, without waiting for the check interval.
	if err := r.updateApproval(ctx, plan, func(st *patchv1alpha1.ApprovalStatus) error {
		_, err := slack.Apply(st.Current, []string{"U1"}, "U1", slack.Approve, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileAutoUpdate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	var child patchv1alpha1.PatchPlan
	if err := r.Get(ctx, types.NamespacedName{Name: "auto-v1.13.5"}, &child); err != nil {
		t.Fatalf("child not created: %v", err)
	}
	if ch, ts := slackThread(&child); ch != "C1" || ts != "1.1" {
		t.Fatalf("child thread = %q %q, want the approval message", ch, ts)
	}
	if n.updates != 1 || currentApproval(plan) != nil || len(plan.Status.AutoUpdate.Approval.History) != 1 {
		t.Fatalf("updates=%d current=%v history=%v", n.updates, currentApproval(plan), plan.Status.AutoUpdate.Approval.History)
	}
}

func TestApprovalSupersedeAndExpire(t *testing.T) {
	ctx := context.Background()
	plan := approvalTemplate()
	plan.Spec.Target.AutoUpdate.Approval.Slack.ExpireAfter = &metav1.Duration{Duration: time.Hour}
	r, n := approvalTestReconciler(t, plan)
	current := mustVersion(t, "v1.13.0")

	if err := r.announceRelease(ctx, plan, &release.Release{Version: mustVersion(t, "v1.13.4")}, current); err != nil {
		t.Fatal(err)
	}
	if err := r.announceRelease(ctx, plan, &release.Release{Version: mustVersion(t, "v1.13.5")}, current); err != nil {
		t.Fatal(err)
	}
	st := plan.Status.AutoUpdate.Approval
	if st.Current.Version != "v1.13.5" || len(st.History) != 1 || st.History[0].State != patchv1alpha1.ApprovalSuperseded || n.posts != 2 || n.updates != 1 {
		t.Fatalf("current=%s history=%+v posts=%d updates=%d", st.Current.Version, st.History, n.posts, n.updates)
	}

	if err := r.updateApproval(ctx, plan, func(st *patchv1alpha1.ApprovalStatus) error {
		st.Current.ExpiresAt = &metav1.Time{Time: time.Now().Add(-time.Minute)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.syncApproval(ctx, plan, true); err != nil {
		t.Fatal(err)
	}
	st = plan.Status.AutoUpdate.Approval
	if st.Current != nil || st.History[0].State != patchv1alpha1.ApprovalExpired || !slack.Excluded(st)["v1.13.5"] {
		t.Fatalf("current=%v history=%+v", st.Current, st.History)
	}
}

func TestApprovalEnabledWithoutSlackConfigNeverCreatesChild(t *testing.T) {
	ctx := context.Background()
	plan := approvalTemplate()
	r := testReconciler(t, plan)
	r.SlackErr = context.Canceled

	if _, err := r.reconcileAutoUpdate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Status.Message, "invalid") || plan.Status.Phase != patchv1alpha1.PatchPhaseWatching {
		t.Fatalf("phase=%s message=%q", plan.Status.Phase, plan.Status.Message)
	}
}
