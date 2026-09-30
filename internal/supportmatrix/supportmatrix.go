// Package supportmatrix holds the Talos/Kubernetes version support matrix.
package supportmatrix

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrUnknownTalosVersion means the Talos minor version isn't in the matrix, so compatibility
// can't be verified.
var ErrUnknownTalosVersion = errors.New("talos version not in compatibility matrix")

type minor struct{ major, minor int }

type minorRange struct{ min, max int }

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
	talos, err := parseMajorMinor(talosVersion)
	if err != nil {
		return fmt.Errorf("invalid talos version %q: %w", talosVersion, err)
	}
	k8s, err := parseMajorMinor(kubernetesVersion)
	if err != nil {
		return fmt.Errorf("invalid kubernetes version %q: %w", kubernetesVersion, err)
	}

	supported, ok := kubernetesSupport[talos]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTalosVersion, talosVersion)
	}

	if k8s.major != 1 || k8s.minor < supported.min || k8s.minor > supported.max {
		return fmt.Errorf("kubernetes %s is not supported by talos %s (supported: 1.%d-1.%d)",
			kubernetesVersion, talosVersion, supported.min, supported.max)
	}
	return nil
}

// parseMajorMinor extracts major and minor from versions like "v1.14.1" or "1.14.0-beta.0".
func parseMajorMinor(v string) (minor, error) {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) < 2 {
		return minor{}, errors.New("expected MAJOR.MINOR[.PATCH]")
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return minor{}, fmt.Errorf("invalid major: %w", err)
	}
	mnr, err := strconv.Atoi(parts[1])
	if err != nil {
		return minor{}, fmt.Errorf("invalid minor: %w", err)
	}
	return minor{major, mnr}, nil
}
