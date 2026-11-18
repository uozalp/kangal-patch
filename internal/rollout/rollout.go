// Package rollout describes the progress of a PatchPlan in a form that notification channels such
// as Slack render, independent of the channel.
package rollout

import (
	"time"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

// Node is the progress of one node.
type Node struct {
	Name  string
	Group string
	// Phase is Pending for a node that has no PatchJob yet.
	Phase patchv1alpha1.PatchJobPhase
	// Message is the PatchJob message, only kept for failed nodes.
	Message string
	// Duration is how long the node took; zero until its PatchJob has finished.
	Duration time.Duration
}

// Snapshot is the complete state of a rollout. Channels render it and decide from the rendering
// whether anything changed, so a missed update is repaired by the next one.
type Snapshot struct {
	Plan    string
	Version string
	Phase   patchv1alpha1.PatchPhase
	// Message is the plan status message.
	Message string
	// Duration is how long the plan took; zero until it has finished.
	Duration time.Duration
	// Halted is true when the rollout stopped and needs manual action.
	Halted bool
	// Nodes are in rollout order.
	Nodes []Node
}

// Finished reports whether the rollout reached a terminal phase.
func (s Snapshot) Finished() bool {
	switch s.Phase {
	case patchv1alpha1.PatchPhaseCompleted, patchv1alpha1.PatchPhaseFailed, patchv1alpha1.PatchPhaseCancelled:
		return true
	}
	return false
}

// Count returns the number of nodes in phase.
func (s Snapshot) Count(phase patchv1alpha1.PatchJobPhase) int {
	n := 0
	for i := range s.Nodes {
		if s.Nodes[i].Phase == phase {
			n++
		}
	}
	return n
}
