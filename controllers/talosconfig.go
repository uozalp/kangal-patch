package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/talos"
)

// openTalos resolves the PatchPlan's Talos config and connects. The returned func closes the client.
func openTalos(ctx context.Context, reader client.Reader, operatorNamespace string, patchPlan *patchv1alpha1.PatchPlan) (*talos.Client, func(), error) {
	talosConfig, err := resolveTalosConfig(ctx, reader, operatorNamespace, patchPlan)
	if err != nil {
		return nil, nil, err
	}

	talosClient, err := talos.NewClient(talosConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create Talos client: %w", err)
	}

	return talosClient, func() {
		if cerr := talosClient.Close(); cerr != nil {
			log.FromContext(ctx).Error(cerr, "failed to close Talos client")
		}
	}, nil
}

// resolveTalosConfig returns the PatchPlan's Talos connection config, loading the certificates
// from the referenced Secret, which must live in the operator namespace.
func resolveTalosConfig(ctx context.Context, reader client.Reader, operatorNamespace string, patchPlan *patchv1alpha1.PatchPlan) (*talos.Config, error) {
	talosConfig := &patchPlan.Spec.TalosConfig
	secretRef := talosConfig.SecretRef
	if secretRef == nil {
		return nil, fmt.Errorf("talosConfig.secretRef must be specified")
	}
	if secretRef.Namespace == "" {
		return nil, fmt.Errorf("secretRef.namespace must be specified")
	}
	// Rejected before any Get so the error does not reveal whether a foreign Secret exists.
	if secretRef.Namespace != operatorNamespace {
		return nil, fmt.Errorf("secretRef.namespace must be the operator namespace %q", operatorNamespace)
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

	return &talos.Config{
		Endpoints:  talosConfig.Endpoints,
		CACert:     string(secret.Data["ca.crt"]),
		ClientCert: string(secret.Data["tls.crt"]),
		ClientKey:  string(secret.Data["tls.key"]),
	}, nil
}
