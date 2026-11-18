package controllers

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/release"
	"github.com/uozalp/kangal-patch/internal/slack"
)

const (
	requeueSlackRetry = 30 * time.Second
	slackCallTimeout  = 30 * time.Second
)

// approvalOn reports whether this template needs Slack approval for new releases.
func approvalOn(spec *patchv1alpha1.AutoUpdateSpec) bool { return spec.SlackApprovalEnabled() }

// slackUnavailable explains why a plan with Slack approval cannot proceed, or returns "".
func (r *PatchPlanReconciler) slackUnavailable() string {
	if r.Slack != nil {
		return ""
	}
	if r.SlackErr != nil {
		return "Slack approval is enabled but the operator's Slack configuration is invalid: " + r.SlackErr.Error()
	}
	return "Slack approval is enabled but the operator has no Slack configuration"
}

func (r *PatchPlanReconciler) updateApproval(ctx context.Context, plan *patchv1alpha1.PatchPlan, mutate func(*patchv1alpha1.ApprovalStatus) error) error {
	return slack.UpdateApproval(ctx, r.APIReader, r.Client, plan, mutate)
}

func currentApproval(plan *patchv1alpha1.PatchPlan) *patchv1alpha1.ApprovalRecord {
	if au := plan.Status.AutoUpdate; au != nil && au.Approval != nil {
		return au.Approval.Current
	}
	return nil
}

// cancelApproval marks a pending approval of a cancelled template as cancelled.
func (r *PatchPlanReconciler) cancelApproval(ctx context.Context, plan *patchv1alpha1.PatchPlan) error {
	if c := currentApproval(plan); c == nil || c.State != patchv1alpha1.ApprovalPending {
		return nil
	}
	return r.updateApproval(ctx, plan, func(st *patchv1alpha1.ApprovalStatus) error {
		if st.Current != nil && st.Current.State == patchv1alpha1.ApprovalPending {
			st.Current.State = patchv1alpha1.ApprovalCancelled
		}
		return nil
	})
}

// syncApproval drives the current approval: it expires it, creates the child PatchPlan once it is
// approved (when apply is set), keeps the Slack message in step with the status and finally moves a
// resolved approval to the history. It returns the name of a child plan it created. A returned
// error means Slack or the API failed and the call should be retried.
func (r *PatchPlanReconciler) syncApproval(ctx context.Context, plan *patchv1alpha1.PatchPlan, apply bool) (string, error) {
	cur := currentApproval(plan)
	if cur == nil {
		return "", nil
	}

	if cur.State == patchv1alpha1.ApprovalPending && cur.ExpiresAt != nil && !time.Now().Before(cur.ExpiresAt.Time) {
		err := r.updateApproval(ctx, plan, func(st *patchv1alpha1.ApprovalStatus) error {
			if c := st.Current; c != nil && c.ID == cur.ID && c.State == patchv1alpha1.ApprovalPending {
				c.State = patchv1alpha1.ApprovalExpired
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}

	var created string
	if cur = currentApproval(plan); apply && cur != nil && cur.State == patchv1alpha1.ApprovalApproved && !cur.Applied {
		name, _, err := r.createChildPlan(ctx, plan, cur.Version, cur.Slack)
		if err != nil {
			return "", err
		}
		created = name
		err = r.updateApproval(ctx, plan, func(st *patchv1alpha1.ApprovalStatus) error {
			if c := st.Current; c != nil && c.ID == cur.ID && c.State == patchv1alpha1.ApprovalApproved {
				c.Applied = true
			}
			return nil
		})
		if err != nil {
			return created, err
		}
	}

	if cur = currentApproval(plan); cur == nil {
		return created, nil
	}
	if err := r.syncSlackMessage(ctx, plan, cur); err != nil {
		return created, err
	}

	if cur = currentApproval(plan); cur != nil && slack.Resolved(cur) && messageInSync(cur) {
		err := r.updateApproval(ctx, plan, func(st *patchv1alpha1.ApprovalStatus) error {
			if c := st.Current; c != nil && c.ID == cur.ID && slack.Resolved(c) && messageInSync(c) {
				slack.Archive(st)
			}
			return nil
		})
		if err != nil {
			return created, err
		}
	}
	return created, nil
}

// messageInSync reports whether there is nothing left to show in Slack for rec.
func messageInSync(rec *patchv1alpha1.ApprovalRecord) bool {
	return rec.Slack == nil || rec.Slack.MessageTS == "" && rec.State != patchv1alpha1.ApprovalPending ||
		rec.RenderedState == slack.RenderKey(rec)
}

// syncSlackMessage posts the announcement if it has not been posted yet, or re-renders the message
// when the approval changed. The rendered key is stored so a failure is retried from status alone.
func (r *PatchPlanReconciler) syncSlackMessage(ctx context.Context, plan *patchv1alpha1.PatchPlan, rec *patchv1alpha1.ApprovalRecord) error {
	posted := rec.Slack != nil && rec.Slack.MessageTS != ""
	switch {
	case !posted && rec.State != patchv1alpha1.ApprovalPending:
		return nil // never announced, so nothing to update
	case posted && rec.RenderedState == slack.RenderKey(rec):
		return nil
	}

	rendered := rec.DeepCopy()
	callCtx, cancel := context.WithTimeout(ctx, slackCallTimeout)
	defer cancel()
	rel := r.releaseFor(callCtx, rendered.Version)

	ref := rendered.Slack
	if !posted {
		channelID, ts, err := r.Slack.Post(callCtx, plan, rendered, rel)
		if err != nil {
			return fmt.Errorf("failed to post the Slack message: %w", err)
		}
		ref = &patchv1alpha1.SlackMessageRef{ChannelID: channelID, MessageTS: ts}
	} else if err := r.Slack.Update(callCtx, plan, rendered, rel); err != nil {
		return fmt.Errorf("failed to update the Slack message: %w", err)
	}

	return r.updateApproval(ctx, plan, func(st *patchv1alpha1.ApprovalStatus) error {
		if c := st.Current; c != nil && c.ID == rendered.ID {
			c.Slack = ref
			c.RenderedState = slack.RenderKey(rendered)
		}
		return nil
	})
}

// releaseFor looks up the release notes of version; the message is still useful without them.
func (r *PatchPlanReconciler) releaseFor(ctx context.Context, version string) *release.Release {
	releases, err := r.Releases.List(ctx)
	if err != nil {
		log.FromContext(ctx).Info("release notes unavailable for the Slack message", "error", err.Error())
		return nil
	}
	for i := range releases {
		if releases[i].Version.String() == version {
			return &releases[i]
		}
	}
	return nil
}

// announceRelease reconciles the pending approval with the release the auto-update selected: a
// pending approval for any other release is superseded, and a new release is announced.
// target is nil when there is nothing to roll out.
func (r *PatchPlanReconciler) announceRelease(ctx context.Context, plan *patchv1alpha1.PatchPlan, target *release.Release, previous release.Version) error {
	spec := plan.Spec.Target.AutoUpdate
	targetVersion := ""
	if target != nil {
		targetVersion = target.Version.String()
	}

	if c := currentApproval(plan); c != nil && c.State == patchv1alpha1.ApprovalPending && c.Version != targetVersion {
		err := r.updateApproval(ctx, plan, func(st *patchv1alpha1.ApprovalStatus) error {
			if c := st.Current; c != nil && c.State == patchv1alpha1.ApprovalPending && c.Version != targetVersion {
				c.State = patchv1alpha1.ApprovalSuperseded
			}
			return nil
		})
		if err != nil {
			return err
		}
		if _, err := r.syncApproval(ctx, plan, false); err != nil {
			return err
		}
	}
	if target == nil || currentApproval(plan) != nil {
		return nil
	}

	now := time.Now()
	rec := patchv1alpha1.ApprovalRecord{
		ID:                slack.ApprovalID(plan.UID, targetVersion),
		Version:           targetVersion,
		PreviousVersion:   previous.String(),
		State:             patchv1alpha1.ApprovalPending,
		RequiredApprovals: int32(r.Slack.Required()),
		AnnouncedAt:       metav1.NewTime(now),
	}
	if d := spec.Approval.Slack.ExpireAfter; d != nil {
		rec.ExpiresAt = &metav1.Time{Time: now.Add(d.Duration)}
	}
	// Persist before posting so a restart never loses track of the announcement.
	err := r.updateApproval(ctx, plan, func(st *patchv1alpha1.ApprovalStatus) error {
		if st.Current == nil {
			st.Current = rec.DeepCopy()
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, err = r.syncApproval(ctx, plan, false)
	return err
}
