package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseImage(t *testing.T) {
	host, repo, tag, err := parseImage("factory.talos.dev/metal-installer/abc123:v1.14.1")
	if err != nil {
		t.Fatal(err)
	}
	if host != "factory.talos.dev" || repo != "metal-installer/abc123" || tag != "v1.14.1" {
		t.Fatalf("got %q %q %q", host, repo, tag)
	}

	for _, bad := range []string{"noslash", "host/repo"} {
		if _, _, _, err := parseImage(bad); err == nil {
			t.Errorf("parseImage(%q) expected error", bad)
		}
	}
}

func TestExists(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = fmt.Fprint(w, `{"token":"t0k"}`)
		case "/v2/org/img/manifests/v1":
			if r.Header.Get("Authorization") != "Bearer t0k" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="svc",scope="repository:org/img:pull"`, srv.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Checker{HTTPClient: srv.Client()}
	host := srv.Listener.Addr().String()

	if err := c.Exists(context.Background(), host+"/org/img:v1"); err != nil {
		t.Fatalf("existing image: %v", err)
	}
	if err := c.Exists(context.Background(), host+"/org/img:v2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing image: got %v, want ErrNotFound", err)
	}
}
