package controllers

import (
	"context"
	"slices"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/scheduling"
)

const (
	testNamespace = "kangal-patch"
	cpLabel       = "node-role.kubernetes.io/control-plane"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, coordinationv1.AddToScheme, patchv1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func testNode(name string, labels ...string) corev1.Node {
	l := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		l[labels[i]] = labels[i+1]
	}
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
}

func testPlan(spec patchv1alpha1.PatchPlanSpec) *patchv1alpha1.PatchPlan {
	return &patchv1alpha1.PatchPlan{
		ObjectMeta: metav1.ObjectMeta{Name: "plan", UID: "plan-uid"},
		Spec:       spec,
	}
}

func testReconciler(t *testing.T, objs ...client.Object) *PatchPlanReconciler {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&patchv1alpha1.PatchPlan{}, &patchv1alpha1.PatchJob{}).
		WithIndex(&patchv1alpha1.PatchJob{}, "spec.patchPlanRef", func(o client.Object) []string {
			return []string{o.(*patchv1alpha1.PatchJob).Spec.PatchPlanRef}
		}).
		WithObjects(objs...).Build()
	return &PatchPlanReconciler{Client: c, Scheme: s, Namespace: testNamespace}
}

func listJobNodes(t *testing.T, r *PatchPlanReconciler) []string {
	t.Helper()
	var jobs patchv1alpha1.PatchJobList
	if err := r.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	var nodes []string
	for i := range jobs.Items {
		nodes = append(nodes, jobs.Items[i].Spec.NodeName)
	}
	slices.Sort(nodes)
	return nodes
}

func countLeases(t *testing.T, r *PatchPlanReconciler, group string) int {
	t.Helper()
	var leases coordinationv1.LeaseList
	if err := r.List(context.Background(), &leases, client.InNamespace(testNamespace),
		client.MatchingLabels{patchv1alpha1.LabelPatchPlan: "plan", patchv1alpha1.LabelGroup: group}); err != nil {
		t.Fatal(err)
	}
	return len(leases.Items)
}

func jobFor(node, group string, phase patchv1alpha1.PatchJobPhase) *patchv1alpha1.PatchJob {
	return &patchv1alpha1.PatchJob{
		ObjectMeta: metav1.ObjectMeta{Name: "plan-" + node, UID: types.UID("uid-" + node)},
		Spec:       patchv1alpha1.PatchJobSpec{NodeName: node, PatchPlanRef: "plan", Group: group},
		Status:     patchv1alpha1.PatchJobStatus{Phase: phase},
	}
}

func jobsByNode(jobs ...*patchv1alpha1.PatchJob) map[string]*patchv1alpha1.PatchJob {
	m := map[string]*patchv1alpha1.PatchJob{}
	for _, j := range jobs {
		m[j.Spec.NodeName] = j
	}
	return m
}

func TestScheduleNodesHonoursGroupConcurrencyThroughLeases(t *testing.T) {
	spec := patchv1alpha1.PatchPlanSpec{
		Groups: map[string]patchv1alpha1.GroupSpec{
			"database": {Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"workload": "database"}}, Concurrency: 1},
			"workers":  {Concurrency: 2},
		},
		Strategy: patchv1alpha1.StrategySpec{Order: []string{"database", "workers"}},
	}
	plan := testPlan(spec)
	nodes := []corev1.Node{
		testNode("db-1", "workload", "database"), testNode("db-2", "workload", "database"),
		testNode("w-1"), testNode("w-2"), testNode("w-3"),
	}
	rollout, err := scheduling.Resolve(nodes, spec)
	if err != nil {
		t.Fatal(err)
	}
	r := testReconciler(t, plan)
	ctx := context.Background()

	// The database group allows one job; workers must not start before it has finished.
	active := map[string]int{}
	if _, err := r.scheduleNodes(ctx, plan, rollout, map[string]*patchv1alpha1.PatchJob{}, active); err != nil {
		t.Fatal(err)
	}
	if got := listJobNodes(t, r); !slices.Equal(got, []string{"db-1"}) {
		t.Fatalf("jobs = %v, want [db-1]", got)
	}
	if n := countLeases(t, r, "database"); n != 1 {
		t.Fatalf("database leases = %d, want 1", n)
	}

	// While db-1 runs and holds its lease nothing else is scheduled.
	plan.Status.LastNodeScheduledAt = nil
	db1 := jobFor("db-1", "database", patchv1alpha1.PatchJobPhaseRebooting)
	if _, err := r.scheduleNodes(ctx, plan, rollout, jobsByNode(db1), map[string]int{"database": 1}); err != nil {
		t.Fatal(err)
	}
	if got := listJobNodes(t, r); !slices.Equal(got, []string{"db-1"}) {
		t.Fatalf("jobs while db-1 is rebooting = %v, want [db-1]", got)
	}

	// db-1 done: db-2 follows.
	db1.Status.Phase = patchv1alpha1.PatchJobPhaseCompleted
	if _, err := r.scheduleNodes(ctx, plan, rollout, jobsByNode(db1), map[string]int{}); err != nil {
		t.Fatal(err)
	}
	if got := listJobNodes(t, r); !slices.Equal(got, []string{"db-1", "db-2"}) {
		t.Fatalf("jobs = %v, want [db-1 db-2]", got)
	}

	// Database group finished: workers start, two at once as configured.
	plan.Status.LastNodeScheduledAt = nil
	db2 := jobFor("db-2", "database", patchv1alpha1.PatchJobPhaseCompleted)
	if _, err := r.scheduleNodes(ctx, plan, rollout, jobsByNode(db1, db2), map[string]int{}); err != nil {
		t.Fatal(err)
	}
	if got := listJobNodes(t, r); !slices.Equal(got, []string{"db-1", "db-2", "w-1", "w-2"}) {
		t.Fatalf("jobs = %v, want workers w-1 and w-2 added", got)
	}
	if n := countLeases(t, r, "workers"); n != 2 {
		t.Fatalf("workers leases = %d, want 2", n)
	}
}

func TestScheduleNodesWaitsForDelayBetweenNodes(t *testing.T) {
	spec := patchv1alpha1.PatchPlanSpec{DelayBetweenNodes: metav1.Duration{Duration: time.Hour}}
	plan := testPlan(spec)
	plan.Status.LastNodeScheduledAt = &metav1.Time{Time: time.Now()}
	rollout, err := scheduling.Resolve([]corev1.Node{testNode("w-1")}, spec)
	if err != nil {
		t.Fatal(err)
	}
	r := testReconciler(t, plan)

	res, err := r.scheduleNodes(context.Background(), plan, rollout, map[string]*patchv1alpha1.PatchJob{}, map[string]int{})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter <= 0 || len(listJobNodes(t, r)) != 0 {
		t.Errorf("requeue=%v jobs=%v, want a wait and no jobs", res.RequeueAfter, listJobNodes(t, r))
	}
}

func TestScheduleNodesWaitsForControlPlaneBeforeKubernetesUpgradeOnWorkers(t *testing.T) {
	spec := patchv1alpha1.PatchPlanSpec{
		Target: patchv1alpha1.TargetSpec{KubernetesVersion: "v1.35.0"},
		// Deliberately invalid for a Kubernetes upgrade (preflight rejects it); the runtime gate is a second line of defence.
		Strategy: patchv1alpha1.StrategySpec{Order: []string{"workers", "controlPlane"}},
	}
	plan := testPlan(spec)
	rollout, err := scheduling.Resolve([]corev1.Node{testNode("w-1"), testNode("cp-1", cpLabel, "")}, spec)
	if err != nil {
		t.Fatal(err)
	}
	r := testReconciler(t, plan)

	if _, err := r.scheduleNodes(context.Background(), plan, rollout, map[string]*patchv1alpha1.PatchJob{}, map[string]int{}); err != nil {
		t.Fatal(err)
	}
	if got := listJobNodes(t, r); len(got) != 0 {
		t.Errorf("jobs = %v, want none until the control plane is done", got)
	}
}

func TestSyncLeasesReleasesFinishedJobsAndRestoresMissingOnes(t *testing.T) {
	spec := patchv1alpha1.PatchPlanSpec{}
	plan := testPlan(spec)
	rollout, err := scheduling.Resolve([]corev1.Node{testNode("w-1"), testNode("w-2")}, spec)
	if err != nil {
		t.Fatal(err)
	}

	done := jobFor("w-1", "workers", patchv1alpha1.PatchJobPhaseCompleted)
	running := jobFor("w-2", "workers", patchv1alpha1.PatchJobPhaseUpgrading)
	staleLease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: done.Name, Namespace: testNamespace,
		Labels: map[string]string{
			patchv1alpha1.LabelPatchPlan: "plan", patchv1alpha1.LabelGroup: "workers", patchv1alpha1.LabelPatchJob: done.Name,
		},
	}}
	r := testReconciler(t, plan, done, running, staleLease)

	jobs := []patchv1alpha1.PatchJob{*done, *running}
	active, err := r.syncLeases(context.Background(), plan, rollout, jobs)
	if err != nil {
		t.Fatal(err)
	}

	if active["workers"] != 1 {
		t.Errorf("active workers = %d, want 1", active["workers"])
	}
	var leases coordinationv1.LeaseList
	if err := r.List(context.Background(), &leases, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(leases.Items) != 1 || leases.Items[0].Name != running.Name {
		t.Fatalf("leases = %v, want only the lease of %s", leases.Items, running.Name)
	}
	if leases.Items[0].Labels[patchv1alpha1.LabelGroup] != "workers" {
		t.Errorf("lease labels = %v", leases.Items[0].Labels)
	}
}

func TestApplyRetentionPurgesFinishedHistoryOnlyOnceDue(t *testing.T) {
	finished := metav1.NewTime(time.Now().Add(-8 * 24 * time.Hour))
	plan := testPlan(patchv1alpha1.PatchPlanSpec{})
	plan.Status.Phase = patchv1alpha1.PatchPhaseCompleted
	plan.Status.CompletionTime = &finished

	done := jobFor("w-1", "workers", patchv1alpha1.PatchJobPhaseCompleted)
	running := jobFor("w-2", "workers", patchv1alpha1.PatchJobPhaseRebooting)
	r := testReconciler(t, plan, done, running)
	ctx := context.Background()

	purged, err := r.applyRetention(ctx, plan)
	if err != nil || purged {
		t.Fatalf("purged=%t err=%v, want nothing purged while a job runs", purged, err)
	}
	if got := listJobNodes(t, r); len(got) != 2 {
		t.Fatalf("jobs = %v, want both kept", got)
	}

	var stored patchv1alpha1.PatchJob
	if err := r.Get(ctx, client.ObjectKeyFromObject(running), &stored); err != nil {
		t.Fatal(err)
	}
	stored.Status.Phase = patchv1alpha1.PatchJobPhaseFailed
	if err := r.Status().Update(ctx, &stored); err != nil {
		t.Fatal(err)
	}
	purged, err = r.applyRetention(ctx, plan)
	if err != nil || !purged {
		t.Fatalf("purged=%t err=%v, want purged", purged, err)
	}
	if got := listJobNodes(t, r); len(got) != 0 {
		t.Fatalf("jobs = %v, want none", got)
	}
	if !plan.Status.HistoryPurged {
		t.Error("HistoryPurged not set")
	}

	recent := metav1.NewTime(time.Now().Add(-time.Hour))
	plan2 := testPlan(patchv1alpha1.PatchPlanSpec{})
	plan2.Status.Phase = patchv1alpha1.PatchPhaseCompleted
	plan2.Status.CompletionTime = &recent
	purged, err = testReconciler(t, plan2).applyRetention(ctx, plan2)
	if err != nil || purged {
		t.Errorf("purged=%t err=%v, want recent history kept", purged, err)
	}
}

func TestFailurePolicyExceeded(t *testing.T) {
	tests := []struct {
		name   string
		policy patchv1alpha1.FailurePolicySpec
		failed int
		want   bool
	}{
		{"default triggers at first failure", patchv1alpha1.FailurePolicySpec{}, 1, true},
		{"no failures", patchv1alpha1.FailurePolicySpec{Type: patchv1alpha1.FailurePolicyHalt, MaxFailures: 1}, 0, false},
		{"at max", patchv1alpha1.FailurePolicySpec{Type: patchv1alpha1.FailurePolicyHalt, MaxFailures: 2}, 2, true},
		{"below max", patchv1alpha1.FailurePolicySpec{Type: patchv1alpha1.FailurePolicyPause, MaxFailures: 2}, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.Exceeded(tt.failed); got != tt.want {
				t.Errorf("Exceeded(%d) = %t, want %t", tt.failed, got, tt.want)
			}
		})
	}
}

func TestFailurePolicyHaltMarksPlanFailed(t *testing.T) {
	plan := testPlan(patchv1alpha1.PatchPlanSpec{FailurePolicy: patchv1alpha1.FailurePolicySpec{Type: patchv1alpha1.FailurePolicyHalt, MaxFailures: 1}})
	r := testReconciler(t, plan)

	exceeded, err := r.failureBudgetExceeded(context.Background(), plan, 1)
	if err != nil || !exceeded {
		t.Fatalf("exceeded=%t err=%v", exceeded, err)
	}
	if plan.Status.Phase != patchv1alpha1.PatchPhaseFailed || plan.Status.CompletionTime == nil || plan.Spec.Paused {
		t.Errorf("phase=%s paused=%t completion=%v", plan.Status.Phase, plan.Spec.Paused, plan.Status.CompletionTime)
	}
}

func TestFailurePolicyPausePausesAndResumeAcceptsFailures(t *testing.T) {
	ctx := context.Background()
	plan := testPlan(patchv1alpha1.PatchPlanSpec{FailurePolicy: patchv1alpha1.FailurePolicySpec{Type: patchv1alpha1.FailurePolicyPause, MaxFailures: 1}})
	plan.Status.Phase = patchv1alpha1.PatchPhaseInProgress
	r := testReconciler(t, plan)

	exceeded, err := r.failureBudgetExceeded(ctx, plan, 1)
	if err != nil || !exceeded {
		t.Fatalf("exceeded=%t err=%v", exceeded, err)
	}
	if !plan.Spec.Paused || plan.Status.Phase != patchv1alpha1.PatchPhasePaused || plan.Status.CompletionTime != nil {
		t.Fatalf("paused=%t phase=%s completion=%v", plan.Spec.Paused, plan.Status.Phase, plan.Status.CompletionTime)
	}

	// The user resumes without touching the failed job: its failure is accepted
	plan.Spec.Paused = false
	if paused, err := r.ensurePauseState(ctx, plan, 1); err != nil || paused {
		t.Fatalf("paused=%t err=%v, want resumed", paused, err)
	}
	if plan.Status.FailuresAcknowledged != 1 || plan.Status.Phase != patchv1alpha1.PatchPhaseInProgress {
		t.Fatalf("acknowledged=%d phase=%s", plan.Status.FailuresAcknowledged, plan.Status.Phase)
	}
	if exceeded, err := r.failureBudgetExceeded(ctx, plan, 1); err != nil || exceeded {
		t.Fatalf("exceeded=%t err=%v, want the accepted failure ignored", exceeded, err)
	}

	// A further failure pauses again
	if exceeded, err := r.failureBudgetExceeded(ctx, plan, 2); err != nil || !exceeded {
		t.Fatalf("exceeded=%t err=%v, want a new failure to pause", exceeded, err)
	}
}

func TestSummarizeCountsEachNodeOnce(t *testing.T) {
	spec := patchv1alpha1.PatchPlanSpec{
		Groups: map[string]patchv1alpha1.GroupSpec{
			"db": {Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"workload": "database"}}},
		},
		Strategy: patchv1alpha1.StrategySpec{Order: []string{"db", "workers", "controlPlane"}},
	}
	nodes := []corev1.Node{
		testNode("db-1", "workload", "database"), testNode("w-1"), testNode("w-2"), testNode("cp-1", cpLabel, ""),
	}
	rollout, err := scheduling.Resolve(nodes, spec)
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan(spec)
	jobs := jobsByNode(
		jobFor("db-1", "db", patchv1alpha1.PatchJobPhaseCompleted),
		jobFor("w-1", "workers", patchv1alpha1.PatchJobPhaseUpgradingKubernetes),
	)

	counts := summarize(plan, rollout, jobs, map[string]int{"workers": 1})

	if counts != (JobCounts{Completed: 1, Running: 1, Pending: 2}) {
		t.Errorf("counts = %+v", counts)
	}
	if plan.Status.TotalNodes != 4 || len(plan.Status.Groups) != 3 || plan.Status.Groups[1].Active != 1 {
		t.Errorf("status = %+v", plan.Status)
	}
}

func TestCheckUpgradeOrder(t *testing.T) {
	spec := patchv1alpha1.PatchPlanSpec{Strategy: patchv1alpha1.StrategySpec{Order: []string{"workers", "controlPlane"}}}
	rollout, err := scheduling.Resolve([]corev1.Node{testNode("w-1"), testNode("cp-1", cpLabel, "")}, spec)
	if err != nil {
		t.Fatal(err)
	}

	res := &preflightResult{}
	checkUpgradeOrder(patchv1alpha1.TargetSpec{KubernetesVersion: "v1.35.0"}, rollout, res)
	if len(res.failures) != 1 || res.failures[0].reason != reasonControlPlaneOrder {
		t.Errorf("failures = %+v, want one %s", res.failures, reasonControlPlaneOrder)
	}

	res = &preflightResult{}
	checkUpgradeOrder(patchv1alpha1.TargetSpec{TalosVersion: "v1.13.0"}, rollout, res)
	if len(res.failures) != 0 {
		t.Errorf("a Talos-only upgrade may use any order, got %+v", res.failures)
	}
}

func TestCheckVersionCompatibilityIsAMatrix(t *testing.T) {
	probes := []nodeProbe{{node: "n1", version: "v1.12.0", kubernetesVersion: "v1.32.0"}}

	tests := []struct {
		name     string
		target   patchv1alpha1.TargetSpec
		wantFail bool
	}{
		{"supported target pair", patchv1alpha1.TargetSpec{TalosVersion: "v1.13.0", KubernetesVersion: "v1.35.0"}, false},
		{"kubernetes outside target talos range", patchv1alpha1.TargetSpec{TalosVersion: "v1.13.0", KubernetesVersion: "v1.38.0"}, true},
		{"talos-only upgrade must support the running kubelet", patchv1alpha1.TargetSpec{TalosVersion: "v1.14.0"}, true},
		{"talos-only upgrade keeping a supported kubelet", patchv1alpha1.TargetSpec{TalosVersion: "v1.13.0"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := &preflightResult{}
			checkVersionCompatibility(context.Background(), tt.target, probes, res)
			if failed := len(res.failures) > 0; failed != tt.wantFail {
				t.Errorf("failed=%t (%+v), want %t", failed, res.failures, tt.wantFail)
			}
		})
	}
}
