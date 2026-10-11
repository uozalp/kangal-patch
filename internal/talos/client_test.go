package talos

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
)

func TestDecodePEM(t *testing.T) {
	const pem = "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"

	for name, in := range map[string]string{
		"raw":    pem,
		"base64": base64.StdEncoding.EncodeToString([]byte(pem)),
	} {
		got, err := decodePEM(in)
		if err != nil || string(got) != pem {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
	}

	if _, err := decodePEM("not base64!"); err == nil {
		t.Error("expected error for invalid input")
	}
}

const sampleConfig = `version: v1alpha1
machine:
  type: worker
  kubelet:
    image: ghcr.io/siderolabs/kubelet:v1.32.0
cluster:
  apiServer:
    image: registry.k8s.io/kube-apiserver:v1.32.0
  controllerManager:
    image: registry.k8s.io/kube-controller-manager:v1.32.0
  scheduler:
    image: registry.k8s.io/kube-scheduler:v1.32.0
`

func TestKubernetesPatchesApplyToMachineConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		patch string
		want  []string
	}{
		"kubelet":       {kubeletPatch("ghcr.io/siderolabs/kubelet:v1.33.0"), []string{"kubelet:v1.33.0"}},
		"control plane": {controlPlanePatch("a:v1.33.0", "cm:v1.33.0", "s:v1.33.0"), []string{"a:v1.33.0", "cm:v1.33.0", "s:v1.33.0"}},
	} {
		t.Run(name, func(t *testing.T) {
			patches, err := configpatcher.LoadPatches([]string{tc.patch})
			if err != nil {
				t.Fatal(err)
			}
			out, err := configpatcher.Apply(configpatcher.WithBytes([]byte(sampleConfig)), patches)
			if err != nil {
				t.Fatal(err)
			}
			b, err := out.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(b), w) {
					t.Errorf("patched config lacks %q:\n%s", w, b)
				}
			}
		})
	}
}
