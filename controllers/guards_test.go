package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/patchutil"
	"github.com/uozalp/kangal-patch/internal/scheduling"
)

func TestIsDowngrade(t *testing.T) {
	for _, tc := range []struct {
		current, target string
		want            bool
	}{
		{"v1.13.4", "v1.12.1", true},
		{"v1.13.4", "v1.13.3", true},
		{"v1.13.4", "v1.13.4", false},
		{"v1.13.4", "v1.14.0", false},
		{"v1.14.0", "v1.14.0-beta.0", true},
		{"garbage", "v1.14.0", false},
		{"v1.14.0", "garbage", false},
	} {
		if got := isDowngrade(tc.current, tc.target); got != tc.want {
			t.Errorf("isDowngrade(%q, %q) = %v, want %v", tc.current, tc.target, got, tc.want)
		}
	}
}

func TestCheckNoDowngradeNamesTheNewerNodes(t *testing.T) {
	res := &preflightResult{}
	checkNoDowngrade(patchv1alpha1.TargetSpec{TalosVersion: "v1.12.1"}, []nodeProbe{
		{node: "old", version: "v1.11.0"},
		{node: "new", version: "v1.13.0"},
	}, res)
	if len(res.failures) != 1 || res.failures[0].reason != reasonDowngrade {
		t.Fatalf("failures = %+v", res.failures)
	}

	res = &preflightResult{}
	checkNoDowngrade(patchv1alpha1.TargetSpec{KubernetesVersion: "v1.33.0"}, []nodeProbe{{node: "new", version: "v1.13.0"}}, res)
	if len(res.failures) != 0 {
		t.Fatalf("a Kubernetes-only plan must not trip the Talos check: %+v", res.failures)
	}
}

func TestCheckNoDowngradeOptInIsTalosOnly(t *testing.T) {
	probes := []nodeProbe{{node: "n1", version: "v1.13.0", kubernetesVersion: "v1.33.2"}}

	res := &preflightResult{}
	checkNoDowngrade(patchv1alpha1.TargetSpec{TalosVersion: "v1.12.1", AllowTalosDowngrade: true}, probes, res)
	if len(res.failures) != 0 {
		t.Fatalf("allowTalosDowngrade must let a Talos rollback through: %+v", res.failures)
	}

	res = &preflightResult{}
	checkNoDowngrade(patchv1alpha1.TargetSpec{KubernetesVersion: "v1.32.4", AllowTalosDowngrade: true}, probes, res)
	if len(res.failures) != 1 || res.failures[0].reason != reasonDowngrade {
		t.Fatalf("a Kubernetes downgrade must fail even with the flag: %+v", res.failures)
	}
}

func TestPatchJobDowngradeRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target patchv1alpha1.TargetSpec
		talos  string
		want   patchv1alpha1.PatchJobPhase
	}{
		{"talos rollback with flag", patchv1alpha1.TargetSpec{TalosVersion: "v1.11.0", AllowTalosDowngrade: true}, "v1.12.0", patchv1alpha1.PatchJobPhasePending},
		{"kubernetes downgrade with flag", patchv1alpha1.TargetSpec{KubernetesVersion: "v1.32.0", AllowTalosDowngrade: true}, "v1.12.0", patchv1alpha1.PatchJobPhaseFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newJobHarness(t, tc.talos)
			j := h.job()
			j.Spec.Target = tc.target
			if err := h.r.Update(context.Background(), j); err != nil {
				t.Fatal(err)
			}
			n := h.node()
			n.Status.NodeInfo.KubeletVersion = "v1.33.2"
			if err := h.r.Status().Update(context.Background(), n); err != nil {
				t.Fatal(err)
			}
			h.wantPhase(h.step(), tc.want)
		})
	}
}

func TestPatchJobRefusesDowngrade(t *testing.T) {
	h := newJobHarness(t, "v1.12.0") // job targets v1.11.0
	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseFailed)
	if len(h.talos.upgradeImages) != 0 {
		t.Fatal("Upgrade must not be called for a downgrade")
	}
}

func TestPatchJobUpgradeStartFailureReleasesCordon(t *testing.T) {
	h := newJobHarness(t, "v1.10.0")
	h.step() // init
	h.step() // cordon + drain
	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseUpgrading)
	if !h.node().Spec.Unschedulable {
		t.Fatal("node should be cordoned before the upgrade")
	}

	h.talos.upgradeErr = errors.New("image pull rejected")
	h.r.transientSince.Store(types.NamespacedName{Name: "job"}, time.Now().Add(-2*transientErrorDeadline))
	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseFailed)
	if h.node().Spec.Unschedulable {
		t.Error("node left cordoned although it never rebooted")
	}
}

func TestPatchJobKubeletAtTargetStillChecksControlPlanePods(t *testing.T) {
	h := newJobHarness(t, "v1.11.0")

	j := h.job()
	j.Spec.Target = patchv1alpha1.TargetSpec{KubernetesVersion: "v1.33.0"}
	if err := h.r.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	n := h.node()
	n.Labels = map[string]string{cpLabel: ""}
	n.Status.NodeInfo.KubeletVersion = "v1.33.0"
	if err := h.r.Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if err := h.r.Status().Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}

	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseUpgradingKubernetes)
}

func staticPod(component, image string, ready bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: component + "-n1", Namespace: kubeSystemNamespace, Labels: map[string]string{"component": component}},
		Spec:       corev1.PodSpec{NodeName: "n1"},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Image: image, Ready: ready}}},
	}
}

func TestControlPlaneStaticPodsReadyNeedsAllThreeWithTargetImage(t *testing.T) {
	const v = "v1.33.0"
	api := staticPod("kube-apiserver", patchutil.BuildAPIServerImage(v), true)
	cm := staticPod("kube-controller-manager", patchutil.BuildControllerManagerImage(v), true)
	sched := staticPod("kube-scheduler", patchutil.BuildSchedulerImage(v), true)
	oldSched := staticPod("kube-scheduler", patchutil.BuildSchedulerImage("v1.32.0"), true)
	notReady := staticPod("kube-scheduler", patchutil.BuildSchedulerImage(v), false)

	for _, tc := range []struct {
		name string
		pods []*corev1.Pod
		want bool
	}{
		{"all current and ready", []*corev1.Pod{api, cm, sched}, true},
		{"scheduler missing", []*corev1.Pod{api, cm}, false},
		{"scheduler on old image", []*corev1.Pod{api, cm, oldSched}, false},
		{"scheduler not ready", []*corev1.Pod{api, cm, notReady}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newJobHarness(t, "v1.11.0")
			for _, p := range tc.pods {
				if err := h.r.Create(context.Background(), p.DeepCopy()); err != nil {
					t.Fatal(err)
				}
			}
			got, err := h.r.controlPlaneStaticPodsReady(context.Background(), "n1", v)
			if err != nil || got != tc.want {
				t.Fatalf("ready = %v, err = %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestCheckConflictingPlansFailsOnlyAgainstBlockingPlans(t *testing.T) {
	now := time.Now()
	node := testNode("n1")
	running := testPlan(patchv1alpha1.PatchPlanSpec{})
	running.Name = "running"
	running.Status.Phase = patchv1alpha1.PatchPhaseInProgress
	finished := testPlan(patchv1alpha1.PatchPlanSpec{})
	finished.Name = "finished"
	finished.Status.Phase = patchv1alpha1.PatchPhaseCompleted
	mine := testPlan(patchv1alpha1.PatchPlanSpec{})
	mine.Name = "mine"
	mine.CreationTimestamp = metav1.NewTime(now)

	r := testReconciler(t, &node, running, finished)

	res := &preflightResult{}
	if err := r.checkConflictingPlans(context.Background(), mine, []corev1.Node{node}, res); err != nil {
		t.Fatal(err)
	}
	if len(res.failures) != 1 || res.failures[0].reason != reasonPlanConflict {
		t.Fatalf("failures = %+v, want one PlanConflict (against the running plan only)", res.failures)
	}

	other := testNode("n2")
	res = &preflightResult{}
	if err := r.checkConflictingPlans(context.Background(), mine, []corev1.Node{other}, res); err != nil {
		t.Fatal(err)
	}
	if len(res.failures) != 0 {
		t.Fatalf("plan on disjoint nodes must not conflict: %+v", res.failures)
	}
}

func TestEnsureKubeProxyWaitsForEveryControlPlaneJob(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: kubeProxyDaemonSetName, Namespace: kubeProxyNamespace},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "kube-proxy", Image: patchutil.BuildKubeProxyImage("v1.32.0")}},
		}}},
	}
	cp1, cp2 := testNode("cp1", cpLabel, ""), testNode("cp2", cpLabel, "")
	plan := testPlan(patchv1alpha1.PatchPlanSpec{Target: patchv1alpha1.TargetSpec{KubernetesVersion: "v1.33.0"}})
	r := testReconciler(t, plan, ds)
	rollout, err := scheduling.Resolve([]corev1.Node{cp1, cp2}, plan.Spec)
	if err != nil {
		t.Fatal(err)
	}

	image := func() string {
		var got appsv1.DaemonSet
		if err := r.Get(context.Background(), types.NamespacedName{Name: kubeProxyDaemonSetName, Namespace: kubeProxyNamespace}, &got); err != nil {
			t.Fatal(err)
		}
		return got.Spec.Template.Spec.Containers[0].Image
	}

	jobs := jobsByNode(jobFor("cp1", "controlPlane", patchv1alpha1.PatchJobPhaseCompleted), jobFor("cp2", "controlPlane", patchv1alpha1.PatchJobPhaseRebooting))
	if wait, err := r.ensureKubeProxy(context.Background(), plan, rollout, jobs); err != nil || wait != 0 {
		t.Fatalf("wait = %v, err = %v; must do nothing while a control plane job runs", wait, err)
	}
	if image() != patchutil.BuildKubeProxyImage("v1.32.0") {
		t.Fatal("kube-proxy was bumped before every control plane node finished")
	}

	jobs = jobsByNode(jobFor("cp1", "controlPlane", patchv1alpha1.PatchJobPhaseCompleted), jobFor("cp2", "controlPlane", patchv1alpha1.PatchJobPhaseCompleted))
	if wait, err := r.ensureKubeProxy(context.Background(), plan, rollout, jobs); err != nil || wait == 0 {
		t.Fatalf("wait = %v, err = %v; want a wait for the rollout", wait, err)
	}
	if image() != patchutil.BuildKubeProxyImage("v1.33.0") {
		t.Fatal("kube-proxy not bumped once the control plane completed")
	}
}
