// Package supportmatrix holds the Talos/Kubernetes version support matrix.
package supportmatrix

import (
	"errors"
	"fmt"

	utilversion "k8s.io/apimachinery/pkg/util/version"
)

// ErrUnknownTalosVersion means the Talos minor version isn't in the matrix, so compatibility
// can't be verified.
var ErrUnknownTalosVersion = errors.New("talos version not in compatibility matrix")

type minor struct{ major, minor uint }

type minorRange struct{ min, max uint }

// kubernetesSupport maps a Talos minor version to the supported Kubernetes 1.x minor range, taken
// from the "Kubernetes" row of https://docs.siderolabs.com/talos/<version>/getting-started/support-matrix.
// Not exposed by the Talos API; add a row for every new Talos minor.
var kubernetesSupport = map[minor]minorRange{
	{1, 10}: {28, 33},
	{1, 11}: {29, 34},
	{1, 12}: {30, 35},
	{1, 13}: {31, 36},
	{1, 14}: {33, 37},
}

// ValidateKubernetesVersion checks that kubernetesVersion is supported by talosVersion. It returns
// an error wrapping ErrUnknownTalosVersion if the Talos minor isn't in the matrix.
func ValidateKubernetesVersion(talosVersion, kubernetesVersion string) error {
	tv, err := utilversion.ParseGeneric(talosVersion)
	if err != nil {
		return fmt.Errorf("invalid talos version %q: %w", talosVersion, err)
	}
	k8s, err := utilversion.ParseGeneric(kubernetesVersion)
	if err != nil {
		return fmt.Errorf("invalid kubernetes version %q: %w", kubernetesVersion, err)
	}

	supported, ok := kubernetesSupport[minor{tv.Major(), tv.Minor()}]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTalosVersion, talosVersion)
	}

	if k8s.Major() != 1 || k8s.Minor() < supported.min || k8s.Minor() > supported.max {
		return fmt.Errorf("kubernetes %s is not supported by talos %s (supported: 1.%d-1.%d)",
			kubernetesVersion, talosVersion, supported.min, supported.max)
	}
	return nil
}
