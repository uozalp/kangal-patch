package supportmatrix

import (
	"errors"
	"testing"
)

func TestValidateKubernetesVersion(t *testing.T) {
	tests := []struct {
		name    string
		talos   string
		k8s     string
		wantErr bool
		unknown bool
	}{
		{name: "in range", talos: "v1.14.1", k8s: "v1.36.2"},
		{name: "lower bound", talos: "v1.14.0", k8s: "v1.33.0"},
		{name: "upper bound", talos: "v1.14.0", k8s: "v1.37.9"},
		{name: "prerelease talos", talos: "v1.14.0-beta.0", k8s: "v1.35.0"},
		{name: "too new", talos: "v1.13.2", k8s: "v1.37.0", wantErr: true},
		{name: "too old", talos: "v1.14.0", k8s: "v1.32.4", wantErr: true},
		{name: "unknown talos", talos: "v1.99.0", k8s: "v1.35.0", wantErr: true, unknown: true},
		{name: "bad talos", talos: "latest", k8s: "v1.35.0", wantErr: true},
		{name: "bad k8s", talos: "v1.14.0", k8s: "v1", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateKubernetesVersion(tt.talos, tt.k8s)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrUnknownTalosVersion); got != tt.unknown {
				t.Fatalf("errors.Is(ErrUnknownTalosVersion) = %v, want %v", got, tt.unknown)
			}
		})
	}
}
