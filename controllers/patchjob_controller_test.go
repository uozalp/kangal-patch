package controllers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/drain"
	"github.com/uozalp/kangal-patch/internal/talos"
)

func TestRetryOrFailRetriesUntilDeadline(t *testing.T) {
	s := testScheme(t)
	job := &patchv1alpha1.PatchJob{ObjectMeta: metav1.ObjectMeta{Name: "job"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(job).
		WithStatusSubresource(&patchv1alpha1.PatchJob{}).Build()
	r := &PatchJobReconciler{Client: c, Scheme: s}
	ctx := context.Background()

	var cur patchv1alpha1.PatchJob
	if err := c.Get(ctx, types.NamespacedName{Name: "job"}, &cur); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("api blip")

	if _, err := r.retryOrFail(ctx, cur.DeepCopy(), &cur, "step", cause); !errors.Is(err, cause) {
		t.Fatalf("want error to be returned for retry, got %v", err)
	}
	if cur.Status.Phase == patchv1alpha1.PatchJobPhaseFailed {
		t.Fatal("job failed on the first transient error")
	}

	r.transientSince.Store(client.ObjectKeyFromObject(&cur), time.Now().Add(-2*transientErrorDeadline))
	_, _ = r.retryOrFail(ctx, cur.DeepCopy(), &cur, "step", cause)
	if cur.Status.Phase != patchv1alpha1.PatchJobPhaseFailed {
		t.Fatalf("want Failed after deadline, got %q", cur.Status.Phase)
	}
}

type fakeTalos struct {
	version       string
	schematic     string
	upgradeImages []string
	upgradeErr    error
}

func (f *fakeTalos) GetVersion(context.Context, string) (string, error) { return f.version, nil }
func (f *fakeTalos) GetSchematicID(context.Context, string) (string, error) {
	return f.schematic, nil
}
func (f *fakeTalos) Upgrade(_ context.Context, _, image string) error {
	f.upgradeImages = append(f.upgradeImages, image)
	return f.upgradeErr
}
func (f *fakeTalos) PatchKubeletVersion(context.Context, string, string) error { return nil }
func (f *fakeTalos) PatchControlPlaneVersion(context.Context, string, string, string, string) error {
	return nil
}
func (f *fakeTalos) Close() error { return nil }

type jobHarness struct {
	t     *testing.T
	r     *PatchJobReconciler
	talos *fakeTalos
}

func newJobHarness(t *testing.T, talosVersion string, extra ...client.Object) *jobHarness {
	t.Helper()
	s := testScheme(t)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}},
			NodeInfo:  corev1.NodeSystemInfo{BootID: "boot-1"},
		},
	}
	plan := &patchv1alpha1.PatchPlan{
		ObjectMeta: metav1.ObjectMeta{Name: "plan"},
		Spec: patchv1alpha1.PatchPlanSpec{
			TalosConfig: patchv1alpha1.TalosConfig{
				Endpoints: []string{"10.0.0.1"},
				SecretRef: &patchv1alpha1.SecretReference{Name: "talos", Namespace: "ns"},
			},
			DrainTimeout:  metav1.Duration{Duration: time.Minute},
			RebootTimeout: metav1.Duration{Duration: time.Minute},
		},
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "talos", Namespace: "ns"},
		Data:       map[string][]byte{"ca.crt": nil, "tls.crt": nil, "tls.key": nil},
	}
	job := &patchv1alpha1.PatchJob{
		ObjectMeta: metav1.ObjectMeta{Name: "job"},
		Spec: patchv1alpha1.PatchJobSpec{
			NodeName: "n1", PatchPlanRef: "plan",
			Target: patchv1alpha1.TargetSpec{TalosVersion: "v1.11.0"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&patchv1alpha1.PatchJob{}, &corev1.Node{}).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		WithObjects(append([]client.Object{node, plan, job, creds}, extra...)...).Build()

	ft := &fakeTalos{version: talosVersion}
	return &jobHarness{t: t, r: &PatchJobReconciler{
		Client: c, Scheme: s, Namespace: "ns",
		NewTalosClient: func(*talos.Config) (talosAPI, error) { return ft, nil },
	}, talos: ft}
}

// step reconciles the job once and returns its phase afterwards. Errors are ignored: wrong
// transitions show up as a phase mismatch.
func (h *jobHarness) step() patchv1alpha1.PatchJobPhase {
	h.t.Helper()
	_, _ = h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "job"}})
	return h.job().Status.Phase
}

func (h *jobHarness) job() *patchv1alpha1.PatchJob {
	h.t.Helper()
	var j patchv1alpha1.PatchJob
	if err := h.r.Get(context.Background(), types.NamespacedName{Name: "job"}, &j); err != nil {
		h.t.Fatal(err)
	}
	return &j
}

func (h *jobHarness) node() *corev1.Node {
	h.t.Helper()
	var n corev1.Node
	if err := h.r.Get(context.Background(), types.NamespacedName{Name: "n1"}, &n); err != nil {
		h.t.Fatal(err)
	}
	return &n
}

// rebooted simulates the node coming back with the given boot ID, Ready.
func (h *jobHarness) rebooted(bootID string) {
	h.t.Helper()
	n := h.node()
	n.Status.NodeInfo.BootID = bootID
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if err := h.r.Status().Update(context.Background(), n); err != nil {
		h.t.Fatal(err)
	}
}

func (h *jobHarness) wantPhase(got, want patchv1alpha1.PatchJobPhase) {
	h.t.Helper()
	if got != want {
		h.t.Fatalf("phase = %q, want %q (message %q)", got, want, h.job().Status.Message)
	}
}

var managedBy = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs", APIVersion: "apps/v1", UID: "u", Controller: ptr.To(true)}}

func TestPatchJobWalksPendingToCompleted(t *testing.T) {
	workload := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default", OwnerReferences: managedBy},
		Spec:       corev1.PodSpec{NodeName: "n1"},
	}
	h := newJobHarness(t, "v1.10.0", workload)

	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhasePending)
	if j := h.job(); j.Status.TargetTalosVersion != "v1.11.0" || j.Status.CurrentTalosVersion != "v1.10.0" {
		t.Fatalf("versions not recorded: %+v", j.Status)
	}

	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseDraining)
	if !h.node().Spec.Unschedulable {
		t.Fatal("node not cordoned while draining")
	}

	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseUpgrading)

	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseRebooting)
	if want := []string{"ghcr.io/siderolabs/installer:v1.11.0"}; !slices.Equal(h.talos.upgradeImages, want) {
		t.Fatalf("upgrade images = %v, want %v", h.talos.upgradeImages, want)
	}
	if got := h.job().Status.PreUpgradeBootID; got != "boot-1" {
		t.Fatalf("PreUpgradeBootID = %q, want boot-1", got)
	}

	// Still on the old version and boot: keep waiting.
	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseRebooting)

	// New version but kubelet has not reported the new boot yet: not healthy, keep waiting.
	h.talos.version = "v1.11.0"
	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseRebooting)
	if !h.node().Spec.Unschedulable {
		t.Fatal("node uncordoned before it was healthy")
	}

	h.rebooted("boot-2")
	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseCompleted)
	if h.node().Spec.Unschedulable {
		t.Fatal("node still cordoned after completion")
	}
	if h.job().Status.CompletionTime == nil {
		t.Fatal("CompletionTime not set")
	}
}

func TestPatchJobSchematicChangeAtSameVersionUpgrades(t *testing.T) {
	const want = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"
	tests := []struct {
		name      string
		schematic string
		wantPhase patchv1alpha1.PatchJobPhase
	}{
		{"other schematic", "a1a1", patchv1alpha1.PatchJobPhasePending},
		{"same schematic", want, patchv1alpha1.PatchJobPhaseCompleted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newJobHarness(t, "v1.11.0")
			h.talos.schematic = tc.schematic
			j := h.job()
			j.Spec.Target = patchv1alpha1.TargetSpec{TalosVersion: "v1.11.0", Source: "factory", Installer: "metal", SchematicID: want}
			if err := h.r.Update(context.Background(), j); err != nil {
				t.Fatal(err)
			}
			h.wantPhase(h.step(), tc.wantPhase)
		})
	}
}

func TestPatchJobAlreadyAtTargetCompletesWithoutUpgrade(t *testing.T) {
	h := newJobHarness(t, "v1.11.0")
	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseCompleted)
	if len(h.talos.upgradeImages) != 0 {
		t.Fatalf("unexpected upgrade: %v", h.talos.upgradeImages)
	}
}

func TestPatchJobRebootingFailures(t *testing.T) {
	tests := []struct {
		name        string
		version     string // version the node reports
		bootID      string // boot ID the node reports
		startedAgo  time.Duration
		wantPhase   patchv1alpha1.PatchJobPhase
		wantMessage string
	}{
		{"rebooted into old partition", "v1.10.0", "boot-2", time.Second, patchv1alpha1.PatchJobPhaseFailed, "upgrade rolled back"},
		{"not rebooted yet, within timeout", "v1.10.0", "boot-1", time.Second, patchv1alpha1.PatchJobPhaseRebooting, ""},
		{"never reached target, timed out", "v1.10.0", "boot-1", time.Hour, patchv1alpha1.PatchJobPhaseFailed, "did not reach target version"},
		{"target version but kubelet silent, timed out", "v1.11.0", "boot-1", time.Hour, patchv1alpha1.PatchJobPhaseFailed, "did not become healthy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newJobHarness(t, tc.version)
			h.step() // init
			j := h.job()
			j.Status.Phase = patchv1alpha1.PatchJobPhaseRebooting
			j.Status.PreUpgradeBootID = "boot-1"
			j.Status.RebootStartTime = &metav1.Time{Time: time.Now().Add(-tc.startedAgo)}
			if err := h.r.Status().Update(context.Background(), j); err != nil {
				t.Fatal(err)
			}
			if tc.bootID != "boot-1" {
				h.rebooted(tc.bootID)
			}

			h.wantPhase(h.step(), tc.wantPhase)
			if msg := h.job().Status.Message; !strings.Contains(msg, tc.wantMessage) {
				t.Fatalf("message %q does not contain %q", msg, tc.wantMessage)
			}
		})
	}
}

func TestPatchJobDrainTimesOutWhenPodsRemain(t *testing.T) {
	stuck := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "stuck", Namespace: "default", Finalizers: []string{"test/hold"}, OwnerReferences: managedBy},
		Spec:       corev1.PodSpec{NodeName: "n1"},
	}
	h := newJobHarness(t, "v1.10.0", stuck)
	h.step() // init
	h.step() // Pending -> Draining (eviction leaves the pod terminating)
	h.wantPhase(h.job().Status.Phase, patchv1alpha1.PatchJobPhaseDraining)

	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseDraining)

	j := h.job()
	j.Status.DrainStartTime = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	if err := h.r.Status().Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseFailed)
	if h.node().Spec.Unschedulable {
		t.Error("node left cordoned after drain timeout")
	}
}

func TestPatchJobWalksKubernetesUpgradeToCompleted(t *testing.T) {
	h := newJobHarness(t, "v1.11.0")
	j := h.job()
	j.Spec.Target = patchv1alpha1.TargetSpec{KubernetesVersion: "v1.34.0"}
	if err := h.r.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}

	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseUpgradingKubernetes)
	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseValidatingKubernetes)

	// Kubelet still on the old version: keep waiting.
	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseValidatingKubernetes)

	n := h.node()
	n.Status.NodeInfo.KubeletVersion = "v1.34.0"
	if err := h.r.Status().Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	h.wantPhase(h.step(), patchv1alpha1.PatchJobPhaseCompleted)
	if got := h.job().Status.CurrentKubernetesVersion; got != "v1.34.0" {
		t.Fatalf("CurrentKubernetesVersion = %q, want v1.34.0", got)
	}
}

func TestDrainTimeoutDefaultsWhenUnset(t *testing.T) {
	plan := &patchv1alpha1.PatchPlan{}
	if got := drainTimeout(plan); got != drain.DefaultTimeout {
		t.Fatalf("got %s, want default %s", got, drain.DefaultTimeout)
	}
	plan.Spec.DrainTimeout = metav1.Duration{Duration: time.Minute}
	if got := drainTimeout(plan); got != time.Minute {
		t.Fatalf("got %s, want 1m", got)
	}
}
