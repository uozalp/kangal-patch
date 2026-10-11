package talos

import (
	"encoding/base64"
	"testing"
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
