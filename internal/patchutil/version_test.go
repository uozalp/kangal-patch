package patchutil

import (
	"testing"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

func TestBuildInstallerImage(t *testing.T) {
	tests := []struct {
		name      string
		target    patchv1alpha1.TargetSpec
		nodeSchem string
		want      string
		wantErr   bool
	}{
		{"ghcr explicit", patchv1alpha1.TargetSpec{Source: "ghcr", TalosVersion: "v1.11.0"}, "", "ghcr.io/siderolabs/installer:v1.11.0", false},
		{"source defaults to ghcr", patchv1alpha1.TargetSpec{TalosVersion: "v1.11.0"}, "", "ghcr.io/siderolabs/installer:v1.11.0", false},
		{"factory with target schematic", patchv1alpha1.TargetSpec{Source: "factory", Installer: "metal", SchematicID: "abc", TalosVersion: "v1.11.0"}, "node", "factory.talos.dev/metal-installer/abc:v1.11.0", false},
		{"factory falls back to node schematic", patchv1alpha1.TargetSpec{Source: "factory", Installer: "metal", TalosVersion: "v1.11.0"}, "node", "factory.talos.dev/metal-installer/node:v1.11.0", false},
		{"factory secureboot", patchv1alpha1.TargetSpec{Source: "factory", Installer: "metal", SchematicID: "abc", SecureBoot: true, TalosVersion: "v1.11.0"}, "", "factory.talos.dev/metal-installer-secureboot/abc:v1.11.0", false},
		{"factory missing schematic", patchv1alpha1.TargetSpec{Source: "factory", Installer: "metal", TalosVersion: "v1.11.0"}, "", "", true},
		{"factory missing installer", patchv1alpha1.TargetSpec{Source: "factory", SchematicID: "abc", TalosVersion: "v1.11.0"}, "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildInstallerImage(tc.target, tc.nodeSchem)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
