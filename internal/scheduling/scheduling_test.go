package scheduling

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

func node(name string, labelPairs ...string) corev1.Node {
	l := map[string]string{}
	for i := 0; i+1 < len(labelPairs); i += 2 {
		l[labelPairs[i]] = labelPairs[i+1]
	}
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
}

func matchLabels(k, v string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{k: v}}
}

func names(nodes []corev1.Node) []string {
	out := make([]string, len(nodes))
	for i := range nodes {
		out[i] = nodes[i].Name
	}
	return out
}

const cp = "node-role.kubernetes.io/control-plane"

func TestResolveFirstMatchingGroupWins(t *testing.T) {
	nodes := []corev1.Node{
		node("db-1", "workload", "database", "accelerator", "nvidia"),
		node("gpu-1", "accelerator", "nvidia"),
		node("w-1"),
		node("cp-1", cp, ""),
	}
	spec := patchv1alpha1.PatchPlanSpec{
		Groups: map[string]patchv1alpha1.GroupSpec{
			"database": {Selector: matchLabels("workload", "database"), Concurrency: 1},
			"gpu":      {Selector: matchLabels("accelerator", "nvidia"), Concurrency: 2},
		},
		Strategy: patchv1alpha1.StrategySpec{Order: []string{"database", "gpu", "workers", "controlPlane"}},
	}

	got, err := Resolve(nodes, spec)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := map[string][]string{
		"database":     {"db-1"},
		"gpu":          {"gpu-1"},
		"workers":      {"w-1"},
		"controlPlane": {"cp-1"},
	}
	for _, g := range got.Groups {
		if !slices.Equal(names(g.Nodes), want[g.Name]) {
			t.Errorf("group %s = %v, want %v", g.Name, names(g.Nodes), want[g.Name])
		}
	}
	if got.Total() != 4 || got.Selected != 4 || len(got.Unassigned) != 0 {
		t.Errorf("total=%d selected=%d unassigned=%v", got.Total(), got.Selected, got.Unassigned)
	}
	if got.Groups[1].Concurrency != 2 || got.Groups[2].Concurrency != 1 {
		t.Errorf("concurrency = %d/%d, want 2/1", got.Groups[1].Concurrency, got.Groups[2].Concurrency)
	}
}

func TestResolveRespectsOrderExactly(t *testing.T) {
	nodes := []corev1.Node{node("w-1"), node("cp-1", cp, "")}
	spec := patchv1alpha1.PatchPlanSpec{
		Strategy: patchv1alpha1.StrategySpec{Order: []string{"workers", "controlPlane"}},
	}

	got, err := Resolve(nodes, spec)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Groups[0].Name != "workers" || got.Groups[1].Name != "controlPlane" {
		t.Errorf("order = %s, %s", got.Groups[0].Name, got.Groups[1].Name)
	}
}

func TestResolveDefaultOrderAndUnassigned(t *testing.T) {
	nodes := []corev1.Node{node("w-1"), node("cp-1", cp, "")}

	got, err := Resolve(nodes, patchv1alpha1.PatchPlanSpec{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Groups[0].Name != "controlPlane" || got.Groups[1].Name != "workers" {
		t.Errorf("default order = %s, %s", got.Groups[0].Name, got.Groups[1].Name)
	}

	onlyCP := patchv1alpha1.PatchPlanSpec{Strategy: patchv1alpha1.StrategySpec{Order: []string{"controlPlane"}}}
	got, err = Resolve(nodes, onlyCP)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Total() != 1 || !slices.Equal(got.Unassigned, []string{"w-1"}) || got.Selected != 2 {
		t.Errorf("total=%d unassigned=%v selected=%d", got.Total(), got.Unassigned, got.Selected)
	}
}

func TestResolveCountsDuplicateNodesOnce(t *testing.T) {
	nodes := []corev1.Node{node("w-1"), node("w-1")}

	got, err := Resolve(nodes, patchv1alpha1.PatchPlanSpec{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Total() != 1 || got.Selected != 1 {
		t.Errorf("total=%d selected=%d, want 1", got.Total(), got.Selected)
	}
}

func TestResolveInvalidStrategy(t *testing.T) {
	tests := []struct {
		name    string
		spec    patchv1alpha1.PatchPlanSpec
		wantErr string
	}{
		{
			name:    "unknown group",
			spec:    patchv1alpha1.PatchPlanSpec{Strategy: patchv1alpha1.StrategySpec{Order: []string{"kafka", "workers"}}},
			wantErr: `"kafka"`,
		},
		{
			name:    "duplicate group",
			spec:    patchv1alpha1.PatchPlanSpec{Strategy: patchv1alpha1.StrategySpec{Order: []string{"workers", "workers"}}},
			wantErr: "more than once",
		},
		{
			name: "custom group without selector",
			spec: patchv1alpha1.PatchPlanSpec{
				Groups:   map[string]patchv1alpha1.GroupSpec{"kafka": {}},
				Strategy: patchv1alpha1.StrategySpec{Order: []string{"kafka"}},
			},
			wantErr: "needs a selector",
		},
		{
			name: "built-in group with selector",
			spec: patchv1alpha1.PatchPlanSpec{
				Groups: map[string]patchv1alpha1.GroupSpec{"workers": {Selector: matchLabels("a", "b")}},
			},
			wantErr: "built in",
		},
		{
			name: "invalid selector",
			spec: patchv1alpha1.PatchPlanSpec{
				Groups: map[string]patchv1alpha1.GroupSpec{"kafka": {Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "a", Operator: "Bogus"}},
				}}},
				Strategy: patchv1alpha1.StrategySpec{Order: []string{"kafka"}},
			},
			wantErr: "invalid selector",
		},
		{
			name: "group name not usable as label value",
			spec: patchv1alpha1.PatchPlanSpec{
				Groups:   map[string]patchv1alpha1.GroupSpec{"bad name": {Selector: matchLabels("a", "b")}},
				Strategy: patchv1alpha1.StrategySpec{Order: []string{"bad name"}},
			},
			wantErr: "label value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Resolve([]corev1.Node{node("w-1")}, tt.spec)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestResolveSchedulesControlPlaneNodesFirstWithinGroup(t *testing.T) {
	nodes := []corev1.Node{node("a-worker", "tier", "x"), node("z-cp", cp, "", "tier", "x")}
	spec := patchv1alpha1.PatchPlanSpec{
		Groups:   map[string]patchv1alpha1.GroupSpec{"x": {Selector: matchLabels("tier", "x")}},
		Strategy: patchv1alpha1.StrategySpec{Order: []string{"x"}},
	}

	got, err := Resolve(nodes, spec)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !slices.Equal(names(got.Groups[0].Nodes), []string{"z-cp", "a-worker"}) {
		t.Errorf("nodes = %v", names(got.Groups[0].Nodes))
	}
}

func TestCheckControlPlaneFirst(t *testing.T) {
	nodes := []corev1.Node{node("w-1", "tier", "x"), node("cp-1", cp, "")}
	mixed := map[string]patchv1alpha1.GroupSpec{"x": {Selector: &metav1.LabelSelector{}}}

	tests := []struct {
		name    string
		spec    patchv1alpha1.PatchPlanSpec
		wantErr string
	}{
		{
			name: "control plane first",
			spec: patchv1alpha1.PatchPlanSpec{Strategy: patchv1alpha1.StrategySpec{Order: []string{"controlPlane", "workers"}}},
		},
		{
			name:    "workers first",
			spec:    patchv1alpha1.PatchPlanSpec{Strategy: patchv1alpha1.StrategySpec{Order: []string{"workers", "controlPlane"}}},
			wantErr: `schedules group "workers"`,
		},
		{
			name: "only workers scheduled",
			spec: patchv1alpha1.PatchPlanSpec{Strategy: patchv1alpha1.StrategySpec{Order: []string{"workers"}}},
		},
		{
			name:    "mixed group",
			spec:    patchv1alpha1.PatchPlanSpec{Groups: mixed, Strategy: patchv1alpha1.StrategySpec{Order: []string{"x"}}},
			wantErr: "both control plane and worker",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rollout, err := Resolve(nodes, tt.spec)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			err = rollout.CheckControlPlaneFirst()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
