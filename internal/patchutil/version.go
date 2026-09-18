package patchutil

import (
	"fmt"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

// BuildInstallerImage constructs the full Talos installer image reference from a TargetSpec.
func BuildInstallerImage(t patchv1alpha1.TargetSpec) (string, error) {
	if t.Source == "ghcr" || t.Source == "" {
		return fmt.Sprintf(
			"ghcr.io/siderolabs/installer:%s",
			t.Version,
		), nil
	}

	if t.SchematicID == "" {
		return "", fmt.Errorf("schematicID is required when source=factory")
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
		t.SchematicID,
		t.Version,
	), nil
}
