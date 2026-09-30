package patchutil

import (
	"fmt"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

// BuildInstallerImage constructs the full Talos installer image reference from a TargetSpec.
// nodeSchematicID is used when the target does not specify a schematicID.
func BuildInstallerImage(t patchv1alpha1.TargetSpec, nodeSchematicID string) (string, error) {
	if t.Source == "ghcr" || t.Source == "" {
		return fmt.Sprintf(
			"ghcr.io/siderolabs/installer:%s",
			t.TalosVersion,
		), nil
	}

	schematicID := t.SchematicID
	if schematicID == "" {
		schematicID = nodeSchematicID
	}
	if schematicID == "" {
		return "", fmt.Errorf("schematicID is required when source=factory and the node's current schematic is unknown")
	}

	if t.Installer == "" {
		return "", fmt.Errorf("installer is required when source=factory")
	}

	suffix := ""
	if t.SecureBoot {
		suffix = "-secureboot"
	}

	return fmt.Sprintf(
		"factory.talos.dev/%s-installer%s/%s:%s",
		t.Installer,
		suffix,
		schematicID,
		t.TalosVersion,
	), nil
}

// BuildKubeletImage constructs the kubelet image reference for a given Kubernetes version.
// Matches the default used by `talosctl upgrade-k8s --kubelet-image`.
func BuildKubeletImage(kubernetesVersion string) string {
	return fmt.Sprintf("ghcr.io/siderolabs/kubelet:%s", kubernetesVersion)
}

// BuildAPIServerImage constructs the kube-apiserver image reference for a given Kubernetes version.
func BuildAPIServerImage(kubernetesVersion string) string {
	return fmt.Sprintf("registry.k8s.io/kube-apiserver:%s", kubernetesVersion)
}

// BuildControllerManagerImage constructs the kube-controller-manager image reference for a given
// Kubernetes version.
func BuildControllerManagerImage(kubernetesVersion string) string {
	return fmt.Sprintf("registry.k8s.io/kube-controller-manager:%s", kubernetesVersion)
}

// BuildSchedulerImage constructs the kube-scheduler image reference for a given Kubernetes version.
func BuildSchedulerImage(kubernetesVersion string) string {
	return fmt.Sprintf("registry.k8s.io/kube-scheduler:%s", kubernetesVersion)
}

// BuildKubeProxyImage constructs the kube-proxy image reference for a given Kubernetes version.
func BuildKubeProxyImage(kubernetesVersion string) string {
	return fmt.Sprintf("registry.k8s.io/kube-proxy:%s", kubernetesVersion)
}
