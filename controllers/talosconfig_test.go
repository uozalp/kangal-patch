package controllers

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

func TestResolveTalosConfigRejectsForeignNamespace(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "talos", Namespace: "other"},
		Data:       map[string][]byte{"ca.crt": nil, "tls.crt": nil, "tls.key": nil},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
	plan := &patchv1alpha1.PatchPlan{Spec: patchv1alpha1.PatchPlanSpec{TalosConfig: patchv1alpha1.TalosConfig{
		SecretRef: &patchv1alpha1.SecretReference{Name: "talos", Namespace: "other"},
	}}}

	if _, err := resolveTalosConfig(context.Background(), c, "ns", plan); err == nil || !strings.Contains(err.Error(), "operator namespace") {
		t.Fatalf("want operator-namespace rejection, got %v", err)
	}

	plan.Spec.TalosConfig.SecretRef.Namespace = "other"
	if _, err := resolveTalosConfig(context.Background(), c, "other", plan); err != nil {
		t.Fatalf("operator namespace should be accepted: %v", err)
	}
}
