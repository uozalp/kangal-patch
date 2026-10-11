package controllers

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/patchutil"
)

func kubeProxy(image string, updated, ready int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: kubeProxyDaemonSetName, Namespace: kubeProxyNamespace},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "kube-proxy", Image: image}}},
		}},
		Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, UpdatedNumberScheduled: updated, NumberReady: ready},
	}
}

func TestEnsureKubeProxyUpgraded(t *testing.T) {
	ctx := context.Background()
	target := patchutil.BuildKubeProxyImage("v1.35.0")

	t.Run("no DaemonSet counts as done", func(t *testing.T) {
		done, err := testReconciler(t).ensureKubeProxyUpgraded(ctx, "v1.35.0")
		if err != nil || !done {
			t.Fatalf("done=%t err=%v", done, err)
		}
	})

	t.Run("old image is patched and the rollout awaited", func(t *testing.T) {
		r := testReconciler(t, kubeProxy("registry.k8s.io/kube-proxy:v1.34.0", 2, 2))
		done, err := r.ensureKubeProxyUpgraded(ctx, "v1.35.0")
		if err != nil || done {
			t.Fatalf("done=%t err=%v, want patched but not done", done, err)
		}
		var ds appsv1.DaemonSet
		if err := r.Get(ctx, types.NamespacedName{Name: kubeProxyDaemonSetName, Namespace: kubeProxyNamespace}, &ds); err != nil {
			t.Fatal(err)
		}
		if got := ds.Spec.Template.Spec.Containers[0].Image; got != target {
			t.Fatalf("image = %s, want %s", got, target)
		}
	})

	t.Run("target image with unfinished rollout is not done", func(t *testing.T) {
		done, err := testReconciler(t, kubeProxy(target, 1, 1)).ensureKubeProxyUpgraded(ctx, "v1.35.0")
		if err != nil || done {
			t.Fatalf("done=%t err=%v, want not done", done, err)
		}
	})

	t.Run("target image fully rolled out is done", func(t *testing.T) {
		done, err := testReconciler(t, kubeProxy(target, 2, 2)).ensureKubeProxyUpgraded(ctx, "v1.35.0")
		if err != nil || !done {
			t.Fatalf("done=%t err=%v, want done", done, err)
		}
	})
}

func TestCancelStopsSchedulingAndClearingItResumes(t *testing.T) {
	ctx := context.Background()
	plan := testPlan(patchv1alpha1.PatchPlanSpec{Cancelled: true})
	plan.Status.Phase = patchv1alpha1.PatchPhaseInProgress
	r := testReconciler(t, plan)

	if cancelled, err := r.ensureCancelledState(ctx, plan); err != nil || !cancelled {
		t.Fatalf("cancelled=%t err=%v", cancelled, err)
	}
	if plan.Status.Phase != patchv1alpha1.PatchPhaseCancelled || plan.Status.CompletionTime == nil {
		t.Fatalf("phase=%s completion=%v", plan.Status.Phase, plan.Status.CompletionTime)
	}

	plan.Spec.Cancelled = false
	if cancelled, err := r.ensureCancelledState(ctx, plan); err != nil || cancelled {
		t.Fatalf("cancelled=%t err=%v, want resumed", cancelled, err)
	}
	if err := r.markInProgress(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if plan.Status.Phase != patchv1alpha1.PatchPhaseInProgress || plan.Status.CompletionTime != nil {
		t.Fatalf("phase=%s completion=%v, want InProgress without completion time", plan.Status.Phase, plan.Status.CompletionTime)
	}
}

func TestPausedPlanPausesAndResumes(t *testing.T) {
	ctx := context.Background()
	plan := testPlan(patchv1alpha1.PatchPlanSpec{Paused: true})
	plan.Status.Phase = patchv1alpha1.PatchPhaseInProgress
	r := testReconciler(t, plan)

	if paused, err := r.ensurePauseState(ctx, plan, 0); err != nil || !paused || plan.Status.Phase != patchv1alpha1.PatchPhasePaused {
		t.Fatalf("paused=%t err=%v phase=%s", paused, err, plan.Status.Phase)
	}

	plan.Spec.Paused = false
	if paused, err := r.ensurePauseState(ctx, plan, 0); err != nil || paused || plan.Status.Phase != patchv1alpha1.PatchPhaseInProgress {
		t.Fatalf("paused=%t err=%v phase=%s, want resumed", paused, err, plan.Status.Phase)
	}
}

func autoUpdateTemplate() *patchv1alpha1.PatchPlan {
	plan := testPlan(patchv1alpha1.PatchPlanSpec{Target: patchv1alpha1.TargetSpec{
		AutoUpdate: &patchv1alpha1.AutoUpdateSpec{Enabled: true},
	}})
	plan.Name = "auto"
	return plan
}

func TestCreateChildPlanIsIdempotentAndClearsAutoUpdate(t *testing.T) {
	ctx := context.Background()
	parent := autoUpdateTemplate()
	parent.Spec.Paused = true
	r := testReconciler(t, parent)

	name, created, err := r.createChildPlan(ctx, parent, "v1.13.5")
	if err != nil || !created || name != "auto-v1.13.5" {
		t.Fatalf("name=%s created=%t err=%v", name, created, err)
	}
	var child patchv1alpha1.PatchPlan
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &child); err != nil {
		t.Fatal(err)
	}
	if child.Spec.Target.AutoUpdate != nil || child.Spec.Target.TalosVersion != "v1.13.5" || child.Spec.Paused {
		t.Fatalf("child spec = %+v", child.Spec)
	}
	if child.Labels[patchv1alpha1.LabelParentPlan] != "auto" {
		t.Fatalf("child labels = %v", child.Labels)
	}

	if _, created, err := r.createChildPlan(ctx, parent, "v1.13.5"); err != nil || created {
		t.Fatalf("created=%t err=%v, want the existing plan reported", created, err)
	}
}

func TestAutoUpdateWaitsForUnfinishedChild(t *testing.T) {
	ctx := context.Background()
	parent := autoUpdateTemplate()
	child := testPlan(patchv1alpha1.PatchPlanSpec{})
	child.Name = "auto-v1.13.5"
	child.UID = "child-uid"
	child.Labels = map[string]string{patchv1alpha1.LabelParentPlan: "auto"}
	child.Status.Phase = patchv1alpha1.PatchPhaseInProgress
	r := testReconciler(t, parent, child)

	res, err := r.reconcileAutoUpdate(ctx, parent)
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("res=%+v err=%v, want a requeue", res, err)
	}

	var plans patchv1alpha1.PatchPlanList
	if err := r.List(ctx, &plans); err != nil {
		t.Fatal(err)
	}
	if len(plans.Items) != 2 {
		t.Fatalf("plans = %d, want no new child while one is unfinished", len(plans.Items))
	}
	if parent.Status.Phase != patchv1alpha1.PatchPhaseWatching {
		t.Fatalf("template phase = %s, want Watching", parent.Status.Phase)
	}
}
