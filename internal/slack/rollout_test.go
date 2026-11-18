package slack

import (
	"fmt"
	"strings"
	"testing"
	"time"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/rollout"
)

func TestRolloutRendersTableAndSummary(t *testing.T) {
	snap := rollout.Snapshot{
		Plan: "auto-v1.13.5", Version: "v1.13.5", Phase: patchv1alpha1.PatchPhaseFailed,
		Message: "failure policy Halt", Duration: 72 * time.Minute, Halted: true,
		Nodes: []rollout.Node{
			{Name: "cp-1", Group: "controlPlane", Phase: patchv1alpha1.PatchJobPhaseCompleted, Duration: 12 * time.Minute},
			{Name: "cp-0", Group: "controlPlane", Phase: patchv1alpha1.PatchJobPhaseCompleted},
			{Name: "cp-2", Group: "controlPlane", Phase: patchv1alpha1.PatchJobPhaseFailed, Message: "drain <timed out>"},
			{Name: "w-1", Group: "workers", Phase: patchv1alpha1.PatchJobPhasePending},
		},
	}
	got := renderRollout(snap)
	for _, want := range []string{"*Failed:* 2/4 nodes patched, 1 failed in 1h12m", "failure policy Halt", "cp-1", "12m", "Completed  0s", "*cp-2*: drain &lt;timed out&gt;"} {
		if !strings.Contains(got.String(), want) {
			t.Errorf("rendering lacks %q:\n%s", want, got)
		}
	}
	if text := failureText(snap, []string{"cp-2"}); !strings.Contains(text, "Node failed:* cp-2") || !strings.Contains(text, "Halted, manual action needed") {
		t.Errorf("failure text = %q", text)
	}

	other := snap
	other.Nodes = append([]rollout.Node(nil), snap.Nodes...)
	other.Nodes[2].Phase = patchv1alpha1.PatchJobPhaseDraining
	same := snap
	same.Nodes = append([]rollout.Node(nil), snap.Nodes...)
	if RolloutKey(snap) == RolloutKey(other) || RolloutKey(snap) != RolloutKey(same) {
		t.Error("key must change with the table and stay stable otherwise")
	}
}

func TestNodeTableDropsFinishedNodesWhenTooLong(t *testing.T) {
	var nodes []rollout.Node
	for i := range 200 {
		nodes = append(nodes, rollout.Node{Name: fmt.Sprintf("node-%03d", i), Group: "workers", Phase: patchv1alpha1.PatchJobPhaseCompleted, Duration: time.Minute})
	}
	nodes = append(nodes, rollout.Node{Name: "node-running", Group: "workers", Phase: patchv1alpha1.PatchJobPhaseRebooting})

	table := nodeTable(nodes)
	if len([]rune(table)) > maxTableRunes || !strings.Contains(table, "node-running") || !strings.Contains(table, "200 more nodes not shown") {
		t.Fatalf("table = %q", table)
	}
}
