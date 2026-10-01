// Package scheduling resolves which group schedules each node of a PatchPlan.
package scheduling

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/nodeutil"
)

// DefaultOrder is used when spec.strategy.order is empty.
var DefaultOrder = []string{patchv1alpha1.GroupControlPlane, patchv1alpha1.GroupWorkers}

// Group is a group of the rollout with the nodes it schedules.
type Group struct {
	Name        string
	Concurrency int
	// Nodes are the nodes this group schedules, control plane nodes first, then by name.
	Nodes []corev1.Node
}

// Rollout is the outcome of resolving every selected node to the first group that matches it.
type Rollout struct {
	// Groups are in strategy.order.
	Groups []Group
	// Selected is the number of unique nodes picked by spec.nodeSelector.
	Selected int
	// Unassigned are the selected nodes no group in strategy.order matches.
	Unassigned []string
}

// Order returns the configured group order, or DefaultOrder when none is set.
func Order(spec patchv1alpha1.PatchPlanSpec) []string {
	if len(spec.Strategy.Order) == 0 {
		return DefaultOrder
	}
	return spec.Strategy.Order
}

// Resolve assigns every node to the first group in strategy.order that matches it. A node is
// assigned at most once, so overlapping groups never schedule a node twice.
func Resolve(nodes []corev1.Node, spec patchv1alpha1.PatchPlanSpec) (*Rollout, error) {
	order := Order(spec)

	matchers, err := compileGroups(order, spec.Groups)
	if err != nil {
		return nil, err
	}

	unique := make([]corev1.Node, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	for i := range nodes {
		if _, ok := seen[nodes[i].Name]; ok {
			continue
		}
		seen[nodes[i].Name] = struct{}{}
		unique = append(unique, nodes[i])
	}
	slices.SortFunc(unique, func(a, b corev1.Node) int {
		aCP, bCP := nodeutil.IsControlPlane(&a), nodeutil.IsControlPlane(&b)
		switch {
		case aCP && !bCP:
			return -1
		case !aCP && bCP:
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})

	rollout := &Rollout{Selected: len(unique)}
	claimed := make(map[string]struct{}, len(unique))
	for _, m := range matchers {
		group := Group{Name: m.name, Concurrency: m.concurrency}
		for i := range unique {
			node := &unique[i]
			if _, ok := claimed[node.Name]; ok {
				continue
			}
			if m.matches(node) {
				claimed[node.Name] = struct{}{}
				group.Nodes = append(group.Nodes, *node)
			}
		}
		rollout.Groups = append(rollout.Groups, group)
	}

	for i := range unique {
		if _, ok := claimed[unique[i].Name]; !ok {
			rollout.Unassigned = append(rollout.Unassigned, unique[i].Name)
		}
	}
	return rollout, nil
}

type groupMatcher struct {
	name        string
	concurrency int
	selector    labels.Selector // nil for built-in groups
}

func (m groupMatcher) matches(node *corev1.Node) bool {
	switch m.name {
	case patchv1alpha1.GroupControlPlane:
		return nodeutil.IsControlPlane(node)
	case patchv1alpha1.GroupWorkers:
		return !nodeutil.IsControlPlane(node)
	}
	return m.selector.Matches(labels.Set(node.Labels))
}

func isBuiltin(name string) bool {
	return name == patchv1alpha1.GroupControlPlane || name == patchv1alpha1.GroupWorkers
}

// compileGroups validates the strategy against the declared groups and prepares a matcher per
// entry of order.
func compileGroups(order []string, groups map[string]patchv1alpha1.GroupSpec) ([]groupMatcher, error) {
	var errs []error
	matchers := make([]groupMatcher, 0, len(order))
	seen := make(map[string]struct{}, len(order))

	for name, g := range groups {
		if isBuiltin(name) && g.Selector != nil {
			errs = append(errs, fmt.Errorf("group %q is built in and cannot have a selector", name))
		}
	}

	for _, name := range order {
		if _, dup := seen[name]; dup {
			errs = append(errs, fmt.Errorf("strategy.order lists group %q more than once", name))
			continue
		}
		seen[name] = struct{}{}

		spec := groups[name]
		m := groupMatcher{name: name, concurrency: max(spec.Concurrency, 1)}

		if !isBuiltin(name) {
			if _, declared := groups[name]; !declared {
				errs = append(errs, fmt.Errorf("strategy.order lists group %q which is neither built in (%s, %s) nor declared under groups",
					name, patchv1alpha1.GroupControlPlane, patchv1alpha1.GroupWorkers))
				continue
			}
			if msgs := validation.IsValidLabelValue(name); len(msgs) > 0 {
				errs = append(errs, fmt.Errorf("group name %q is not usable as a label value: %s", name, strings.Join(msgs, "; ")))
				continue
			}
			if spec.Selector == nil {
				errs = append(errs, fmt.Errorf("group %q needs a selector", name))
				continue
			}
			sel, err := metav1.LabelSelectorAsSelector(spec.Selector)
			if err != nil {
				errs = append(errs, fmt.Errorf("group %q has an invalid selector: %w", name, err))
				continue
			}
			m.selector = sel
		}
		matchers = append(matchers, m)
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return matchers, nil
}

// Nodes returns every node the rollout covers in the order they are scheduled.
func (r *Rollout) Nodes() []corev1.Node {
	var nodes []corev1.Node
	for i := range r.Groups {
		nodes = append(nodes, r.Groups[i].Nodes...)
	}
	return nodes
}

// Total is the number of unique nodes the rollout covers.
func (r *Rollout) Total() int {
	total := 0
	for i := range r.Groups {
		total += len(r.Groups[i].Nodes)
	}
	return total
}

// ControlPlaneNodes returns the control plane nodes the rollout covers.
func (r *Rollout) ControlPlaneNodes() []corev1.Node {
	var nodes []corev1.Node
	for i := range r.Groups {
		for j := range r.Groups[i].Nodes {
			if nodeutil.IsControlPlane(&r.Groups[i].Nodes[j]) {
				nodes = append(nodes, r.Groups[i].Nodes[j])
			}
		}
	}
	return nodes
}

// GroupOf returns the name of the group scheduling nodeName.
func (r *Rollout) GroupOf(nodeName string) (string, bool) {
	for i := range r.Groups {
		for j := range r.Groups[i].Nodes {
			if r.Groups[i].Nodes[j].Name == nodeName {
				return r.Groups[i].Name, true
			}
		}
	}
	return "", false
}

// CheckControlPlaneFirst verifies that every control plane node is scheduled by a group listed
// before every group scheduling a worker node.
func (r *Rollout) CheckControlPlaneFirst() error {
	lastControlPlane, firstWorker := -1, len(r.Groups)
	for i := range r.Groups {
		for j := range r.Groups[i].Nodes {
			if nodeutil.IsControlPlane(&r.Groups[i].Nodes[j]) {
				lastControlPlane = i
			} else if i < firstWorker {
				firstWorker = i
			}
		}
	}
	if lastControlPlane < 0 || firstWorker == len(r.Groups) {
		return nil
	}

	switch {
	case firstWorker < lastControlPlane:
		return fmt.Errorf("strategy.order schedules group %q (worker nodes) before group %q (control plane nodes)",
			r.Groups[firstWorker].Name, r.Groups[lastControlPlane].Name)
	case firstWorker == lastControlPlane:
		return fmt.Errorf("group %q schedules both control plane and worker nodes", r.Groups[firstWorker].Name)
	}
	return nil
}
