package slack

import (
	"context"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

// UpdateApproval reads plan fresh from reader, applies mutate to its approval status and writes it
// back with an optimistic lock, retrying on conflict. mutate must be safe to run more than once. On
// return plan holds the latest server state. Concurrent writers (the socket listeners on every
// replica and the reconciler) therefore never overwrite each other's votes.
func UpdateApproval(ctx context.Context, reader client.Reader, writer client.StatusClient, plan *patchv1alpha1.PatchPlan, mutate func(*patchv1alpha1.ApprovalStatus) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := reader.Get(ctx, client.ObjectKeyFromObject(plan), plan); err != nil {
			return err
		}
		original := plan.DeepCopy()
		if plan.Status.AutoUpdate == nil {
			plan.Status.AutoUpdate = &patchv1alpha1.AutoUpdateStatus{}
		}
		if plan.Status.AutoUpdate.Approval == nil {
			plan.Status.AutoUpdate.Approval = &patchv1alpha1.ApprovalStatus{}
		}
		if err := mutate(plan.Status.AutoUpdate.Approval); err != nil {
			return err
		}
		// Do not let an unchanged status gain empty objects.
		if a := plan.Status.AutoUpdate.Approval; a.Current == nil && len(a.History) == 0 {
			plan.Status.AutoUpdate.Approval = nil
		}
		if original.Status.AutoUpdate == nil && equality.Semantic.DeepEqual(*plan.Status.AutoUpdate, patchv1alpha1.AutoUpdateStatus{}) {
			plan.Status.AutoUpdate = nil
		}
		if equality.Semantic.DeepEqual(original.Status, plan.Status) {
			return nil
		}
		return writer.Status().Patch(ctx, plan, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
	})
}
