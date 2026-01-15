/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
