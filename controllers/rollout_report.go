package controllers

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/nodeutil"
	"github.com/uozalp/kangal-patch/internal/rollout"
	"github.com/uozalp/kangal-patch/internal/scheduling"
	"github.com/uozalp/kangal-patch/internal/slack"
)

// slackThread returns where a child plan reports its progress: the thread of the Slack message that
// announced its release. Plans without it are not reported.
func slackThread(plan *patchv1alpha1.PatchPlan) (channelID, threadTS string) {
	return plan.Annotations[patchv1alpha1.AnnotationSlackChannel], plan.Annotations[patchv1alpha1.AnnotationSlackThread]
}

// reportRollout keeps the Slack thread of the plan's release in step with the rollout: one status
// message with a table of the nodes that is edited as they progress, and a separate reply when a
// node fails (saying so if the rollout halted and needs manual action). It only calls Slack when
// something changed, and returns an error if Slack failed and the call should be retried.
func (r *PatchPlanReconciler) reportRollout(ctx context.Context, plan *patchv1alpha1.PatchPlan) error {
	channelID, threadTS := slackThread(plan)
	if r.Slack == nil || channelID == "" || threadTS == "" || plan.Status.Phase == "" || plan.Status.HistoryPurged {
		return nil
	}

	// Read uncached: a stale cache must never make us post the status message a second time.
	var fresh patchv1alpha1.PatchPlan
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(plan), &fresh); err != nil {
		return err
	}
	ts, shownKey, announced := fresh.Annotations[patchv1alpha1.AnnotationSlackStatusTS],
		fresh.Annotations[patchv1alpha1.AnnotationSlackStatusKey], fresh.Annotations[patchv1alpha1.AnnotationSlackFailures]

	snap, err := r.rolloutSnapshot(ctx, plan)
	if err != nil {
		return err
	}

	callCtx, cancel := context.WithTimeout(ctx, slackCallTimeout)
	defer cancel()

	if key := slack.RolloutKey(snap); ts == "" || shownKey != key {
		if ts == "" {
			if ts, err = r.Slack.PostRollout(callCtx, channelID, threadTS, snap); err != nil {
				return fmt.Errorf("failed to post the rollout status to Slack: %w", err)
			}
		} else if err := r.Slack.UpdateRollout(callCtx, channelID, ts, snap); err != nil {
			return fmt.Errorf("failed to update the rollout status in Slack: %w", err)
		}
		err := r.setSlackAnnotations(ctx, plan, map[string]string{
			patchv1alpha1.AnnotationSlackStatusTS:  ts,
			patchv1alpha1.AnnotationSlackStatusKey: key,
		})
		if err != nil {
			return err
		}
	}

	known := strings.Split(announced, ",")
	var failed []string
	for i := range snap.Nodes {
		if n := &snap.Nodes[i]; n.Phase == patchv1alpha1.PatchJobPhaseFailed && !slices.Contains(known, n.Name) {
			failed = append(failed, n.Name)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	if err := r.Slack.PostFailure(callCtx, channelID, threadTS, snap, failed); err != nil {
		return fmt.Errorf("failed to post the node failure to Slack: %w", err)
	}
	// shortcut: a node that fails again after its failed job was deleted is not announced again
	return r.setSlackAnnotations(ctx, plan, map[string]string{
		patchv1alpha1.AnnotationSlackFailures: strings.Trim(announced+","+strings.Join(failed, ","), ","),
	})
}

func (r *PatchPlanReconciler) setSlackAnnotations(ctx context.Context, plan *patchv1alpha1.PatchPlan, values map[string]string) error {
	original := plan.DeepCopy()
	if plan.Annotations == nil {
		plan.Annotations = map[string]string{}
	}
	maps.Copy(plan.Annotations, values)
	return r.Patch(ctx, plan, client.MergeFrom(original))
}

// rolloutSnapshot describes the plan's progress from its status and PatchJobs.
func (r *PatchPlanReconciler) rolloutSnapshot(ctx context.Context, plan *patchv1alpha1.PatchPlan) (rollout.Snapshot, error) {
	nodes, err := nodeutil.ListMatchingNodes(ctx, r.Client, plan.Spec.NodeSelector)
	if err != nil {
		return rollout.Snapshot{}, err
	}
	jobs, err := r.listPatchJobs(ctx, plan.Name)
	if err != nil {
		return rollout.Snapshot{}, err
	}
	jobsByNode := make(map[string]*patchv1alpha1.PatchJob, len(jobs))
	for i := range jobs {
		jobsByNode[jobs[i].Spec.NodeName] = &jobs[i]
	}

	snap := rollout.Snapshot{
		Plan:     plan.Name,
		Version:  plan.Spec.Target.TalosVersion,
		Phase:    plan.Status.Phase,
		Message:  plan.Status.Message,
		Duration: elapsed(plan.Status.StartTime, plan.Status.CompletionTime),
		Halted:   halted(plan),
	}

	// An invalid strategy is reported through the plan itself; the table is simply empty then.
	if resolved, err := scheduling.Resolve(nodes, plan.Spec); err == nil {
		for i := range resolved.Groups {
			g := &resolved.Groups[i]
			for j := range g.Nodes {
				snap.Nodes = append(snap.Nodes, nodeProgress(g.Name, g.Nodes[j].Name, jobsByNode[g.Nodes[j].Name]))
			}
		}
	}
	return snap, nil
}

func nodeProgress(group, name string, job *patchv1alpha1.PatchJob) rollout.Node {
	n := rollout.Node{Name: name, Group: group, Phase: patchv1alpha1.PatchJobPhasePending}
	if job == nil {
		return n
	}
	if job.Status.Phase != "" {
		n.Phase = job.Status.Phase
	}
	if n.Phase == patchv1alpha1.PatchJobPhaseFailed {
		n.Message = job.Status.Message
	}
	n.Duration = elapsed(job.Status.StartTime, job.Status.CompletionTime)
	return n
}

// halted reports whether the plan stopped because of failures or an invalid setup and waits for
// someone to act: it failed, or the Pause failure policy paused it.
func halted(plan *patchv1alpha1.PatchPlan) bool {
	switch plan.Status.Phase {
	case patchv1alpha1.PatchPhaseFailed:
		return true
	case patchv1alpha1.PatchPhasePaused:
		policy := plan.Spec.FailurePolicy
		return policy.Type == patchv1alpha1.FailurePolicyPause && policy.Exceeded(newFailures(plan, plan.Status.FailedNodes))
	}
	return false
}

// elapsed is the time between start and end, or zero while either is unknown.
func elapsed(start, end *metav1.Time) time.Duration {
	if start == nil || end == nil {
		return 0
	}
	return end.Sub(start.Time)
}
