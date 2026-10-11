package drain

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newPod(name string, mutate func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: "n1"},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func newDrainer(t *testing.T, evict func(context.Context, client.Client, string, client.Object, client.Object, ...client.SubResourceCreateOption) error, objs ...client.Object) *Drainer {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		})
	if evict != nil {
		b = b.WithInterceptorFuncs(interceptor.Funcs{SubResourceCreate: evict})
	}
	return NewDrainer(b.Build())
}

func podExists(t *testing.T, d *Drainer, name string) bool {
	t.Helper()
	err := d.client.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &corev1.Pod{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func pdbRefuses(context.Context, client.Client, string, client.Object, client.Object, ...client.SubResourceCreateOption) error {
	return apierrors.NewTooManyRequests("disruption budget", 1)
}

func TestDrainNodePDB(t *testing.T) {
	for _, tc := range []struct {
		name        string
		respectPDBs bool
		wantBlocked bool
		wantGone    bool
	}{
		{"respecting PDBs keeps the pod and reports blocked", true, true, false},
		{"ignoring PDBs deletes the pod", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDrainer(t, pdbRefuses, newPod("app", nil))
			blocked, err := d.DrainNode(context.Background(), "n1", DrainOptions{RespectPDBs: tc.respectPDBs, Timeout: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if blocked != tc.wantBlocked || podExists(t, d, "app") == tc.wantGone {
				t.Fatalf("blocked=%v exists=%v, want blocked=%v gone=%v", blocked, podExists(t, d, "app"), tc.wantBlocked, tc.wantGone)
			}
		})
	}
}

func TestDrainNodeEvictsOnlyWorkloadPods(t *testing.T) {
	d := newDrainer(t, nil,
		newPod("app", nil),
		newPod("ds", func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds", APIVersion: "apps/v1", UID: "u"}}
		}),
		newPod("static", func(p *corev1.Pod) { p.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"} }),
	)
	blocked, err := d.DrainNode(context.Background(), "n1", DrainOptions{Timeout: time.Minute})
	if err != nil || blocked {
		t.Fatalf("blocked=%v err=%v", blocked, err)
	}
	if podExists(t, d, "app") || !podExists(t, d, "ds") || !podExists(t, d, "static") {
		t.Fatal("want only the workload pod evicted")
	}
}

func TestDrainNodeEvictionNotFoundIsNotBlocked(t *testing.T) {
	gone := func(context.Context, client.Client, string, client.Object, client.Object, ...client.SubResourceCreateOption) error {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "app")
	}
	d := newDrainer(t, gone, newPod("app", nil))
	if blocked, err := d.DrainNode(context.Background(), "n1", DrainOptions{RespectPDBs: true, Timeout: time.Minute}); blocked || err != nil {
		t.Fatalf("blocked=%v err=%v", blocked, err)
	}
}

func TestIsDrained(t *testing.T) {
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"workload pod", newPod("p", nil), false},
		{"terminating pod still counts", newPod("p", func(p *corev1.Pod) {
			p.Finalizers = []string{"test/hold"}
			p.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		}), false},
		{"succeeded pod", newPod("p", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }), true},
		{"failed pod", newPod("p", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }), true},
		{"pod on another node", newPod("p", func(p *corev1.Pod) { p.Spec.NodeName = "n2" }), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDrainer(t, nil, tc.pod)
			got, err := d.IsDrained(context.Background(), "n1")
			if err != nil || got != tc.want {
				t.Fatalf("got %v (err %v), want %v", got, err, tc.want)
			}
		})
	}
}

func TestUncordonOnlyOwnCordon(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	ctx := context.Background()

	for _, tc := range []struct {
		name         string
		adminCordon  bool
		wantUncordon bool
	}{
		{"operator cordon is released", false, true},
		{"admin cordon is kept", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{Unschedulable: tc.adminCordon}}
			d := NewDrainer(fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build())

			if err := d.CordonNode(ctx, "n1"); err != nil {
				t.Fatal(err)
			}
			if err := d.UncordonNode(ctx, "n1"); err != nil {
				t.Fatal(err)
			}

			var got corev1.Node
			if err := d.client.Get(ctx, types.NamespacedName{Name: "n1"}, &got); err != nil {
				t.Fatal(err)
			}
			if got.Spec.Unschedulable == tc.wantUncordon {
				t.Fatalf("unschedulable=%v, want %v", got.Spec.Unschedulable, !tc.wantUncordon)
			}
		})
	}
}
