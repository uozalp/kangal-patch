package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

// resolveTalosConfig returns the PatchPlan's Talos connection config, loading the certificates
// from the referenced Secret if one is set.
func resolveTalosConfig(ctx context.Context, reader client.Reader, patchPlan *patchv1alpha1.PatchPlan) (*patchv1alpha1.TalosConfig, error) {
	talosConfig := &patchPlan.Spec.TalosConfig
	if talosConfig.SecretRef == nil {
		return talosConfig, nil
	}

	secretRef := talosConfig.SecretRef
	if secretRef.Namespace == "" {
		return nil, fmt.Errorf("secretRef.namespace must be specified")
	}

	var secret corev1.Secret
	secretKey := types.NamespacedName{
		Name:      secretRef.Name,
		Namespace: secretRef.Namespace,
	}
	if err := reader.Get(ctx, secretKey, &secret); err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", secretRef.Namespace, secretRef.Name, err)
	}

	// Validate required keys
	requiredKeys := []string{"ca.crt", "tls.crt", "tls.key"}
	for _, key := range requiredKeys {
		if _, ok := secret.Data[key]; !ok {
			return nil, fmt.Errorf("secret %s/%s missing required key: %s", secretRef.Namespace, secretRef.Name, key)
		}
	}

	return &patchv1alpha1.TalosConfig{
		Endpoints:  talosConfig.Endpoints,
		CACert:     string(secret.Data["ca.crt"]),
		ClientCert: string(secret.Data["tls.crt"]),
		ClientKey:  string(secret.Data["tls.key"]),
	}, nil
}
