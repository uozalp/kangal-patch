package slack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

// Action is a decision made in Slack.
type Action string

const (
	Approve Action = "approve"
	Reject  Action = "reject"
)

// MaxHistory is the number of resolved approvals kept in the status.
const MaxHistory = 10

// ErrNotApprover is returned when the user is not in the configured approver list.
var ErrNotApprover = errors.New("user is not an approver")

// ApprovalID identifies the approval of version on one plan; it is stable across retries and differs
// for a recreated plan.
func ApprovalID(planUID types.UID, version string) string {
	sum := sha256.Sum256([]byte(string(planUID) + "/" + version))
	return hex.EncodeToString(sum[:8])
}

// Apply records the decision of user on rec. It reports whether rec changed; a decision on an
// approval that is no longer pending, or a repeated approval, is a no-op.
func Apply(rec *patchv1alpha1.ApprovalRecord, approvers []string, user string, action Action, now time.Time) (bool, error) {
	if !slices.Contains(approvers, user) {
		return false, ErrNotApprover
	}
	if rec.State != patchv1alpha1.ApprovalPending || (rec.ExpiresAt != nil && !now.Before(rec.ExpiresAt.Time)) {
		return false, nil
	}

	vote := patchv1alpha1.ApprovalVote{UserID: user, Time: metav1.NewTime(now)}
	switch action {
	case Approve:
		if slices.ContainsFunc(rec.Approvals, func(v patchv1alpha1.ApprovalVote) bool { return v.UserID == user }) {
			return false, nil
		}
		rec.Approvals = append(rec.Approvals, vote)
		if len(rec.Approvals) >= int(rec.RequiredApprovals) {
			rec.State = patchv1alpha1.ApprovalApproved
		}
	case Reject:
		rec.Rejections = append(rec.Rejections, vote)
		rec.State = patchv1alpha1.ApprovalRejected
	default:
		return false, fmt.Errorf("unknown action %q", action)
	}
	return true, nil
}

// RenderKey identifies what the Slack message of rec should show, so a stale message can be detected.
func RenderKey(rec *patchv1alpha1.ApprovalRecord) string {
	return fmt.Sprintf("%s:%d/%d:%d", rec.State, len(rec.Approvals), rec.RequiredApprovals, len(rec.Rejections))
}

// Resolved reports whether rec needs no further decision or action, other than a final message sync.
func Resolved(rec *patchv1alpha1.ApprovalRecord) bool {
	return rec.State != patchv1alpha1.ApprovalPending && (rec.State != patchv1alpha1.ApprovalApproved || rec.Applied)
}

// Archive moves the current approval to the history, keeping the newest MaxHistory entries.
func Archive(st *patchv1alpha1.ApprovalStatus) {
	if st.Current == nil {
		return
	}
	st.History = append([]patchv1alpha1.ApprovalRecord{*st.Current}, st.History...)
	if len(st.History) > MaxHistory {
		st.History = st.History[:MaxHistory]
	}
	st.Current = nil
}

// Excluded returns the versions that were rejected or expired and must not be announced again.
func Excluded(st *patchv1alpha1.ApprovalStatus) map[string]bool {
	out := map[string]bool{}
	if st == nil {
		return out
	}
	records := st.History
	if st.Current != nil {
		records = append(slices.Clone(records), *st.Current)
	}
	for _, rec := range records {
		if rec.State == patchv1alpha1.ApprovalRejected || rec.State == patchv1alpha1.ApprovalExpired {
			out[rec.Version] = true
		}
	}
	return out
}
