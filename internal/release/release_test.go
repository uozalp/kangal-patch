package release

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) Version {
	t.Helper()
	v, err := ParseVersion(s)
	if err != nil {
		t.Fatalf("ParseVersion(%q): %v", s, err)
	}
	return v
}

func Test_ParseVersion(t *testing.T) {
	tests := []struct {
		in      string
		want    Version
		wantErr bool
	}{
		{in: "v1.13.4", want: Version{1, 13, 4, ""}},
		{in: "1.13.4", want: Version{1, 13, 4, ""}},
		{in: "v1.14.0-beta.0", want: Version{1, 14, 0, "beta.0"}},
		{in: "v1.13", wantErr: true},
		{in: "v1.x.0", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseVersion(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func Test_Version_Compare(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1.13.4", "v1.13.4", 0},
		{"v1.13.4", "v1.13.5", -1},
		{"v1.14.0", "v1.13.9", 1},
		{"v1.14.0-beta.0", "v1.14.0", -1},
		{"v1.14.0", "v1.14.0-rc.1", 1},
	}
	for _, tt := range tests {
		t.Run(tt.a+"_vs_"+tt.b, func(t *testing.T) {
			if got := mustParse(t, tt.a).Compare(mustParse(t, tt.b)); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func Test_Select(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	day := 24 * time.Hour

	releases := func(t *testing.T) []Release {
		return []Release{
			{mustParse(t, "v1.13.0"), ago(60 * day)},
			{mustParse(t, "v1.13.3"), ago(10 * day)},
			{mustParse(t, "v1.13.4"), ago(5 * day)},
			{mustParse(t, "v1.13.5"), ago(1 * day)},
			{mustParse(t, "v1.14.0"), ago(20 * day)},
			{mustParse(t, "v1.14.1"), ago(2 * day)},
			{mustParse(t, "v1.15.0"), ago(3 * day)},
		}
	}

	tests := []struct {
		name        string
		current     string
		allow       string
		minAge      time.Duration
		wantTarget  string
		wantPending string
		wantLatest  string
	}{
		{name: "patch picks newest patch in minor", current: "v1.13.0", allow: AllowPatch, wantTarget: "v1.13.5", wantLatest: "v1.15.0"},
		{name: "patch honours min age", current: "v1.13.0", allow: AllowPatch, minAge: 3 * day, wantTarget: "v1.13.4", wantPending: "v1.13.5", wantLatest: "v1.15.0"},
		{name: "patch up to date", current: "v1.13.5", allow: AllowPatch, wantLatest: "v1.15.0"},
		{name: "patch never crosses minor", current: "v1.13.5", allow: AllowPatch, minAge: 100 * day, wantLatest: "v1.15.0"},
		{name: "minor steps one minor only", current: "v1.13.0", allow: AllowMinor, wantTarget: "v1.14.1", wantLatest: "v1.15.0"},
		{name: "minor falls back to older eligible", current: "v1.13.0", allow: AllowMinor, minAge: 3 * day, wantTarget: "v1.14.0", wantPending: "v1.14.1", wantLatest: "v1.15.0"},
		{name: "minor from previous minor", current: "v1.14.1", allow: AllowMinor, wantTarget: "v1.15.0", wantLatest: "v1.15.0"},
		{name: "current ahead of everything", current: "v1.16.0", allow: AllowMinor, wantLatest: "v1.15.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Select(releases(t), mustParse(t, tt.current), tt.allow, tt.minAge, now)

			check := func(label string, r *Release, want string) {
				t.Helper()
				switch {
				case want == "" && r != nil:
					t.Errorf("%s = %s, want none", label, r.Version)
				case want != "" && r == nil:
					t.Errorf("%s = none, want %s", label, want)
				case want != "" && r.Version.String() != want:
					t.Errorf("%s = %s, want %s", label, r.Version, want)
				}
			}
			check("target", got.Target, tt.wantTarget)
			check("pending", got.Pending, tt.wantPending)
			check("latest", got.Latest, tt.wantLatest)
		})
	}
}

func Test_Fetcher_List(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "missing user agent", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`[
			{"tag_name":"v1.13.4","published_at":"2026-09-01T10:00:00Z"},
			{"tag_name":"v1.14.0-beta.0","prerelease":true,"published_at":"2026-09-02T10:00:00Z"},
			{"tag_name":"v1.14.0-rc.1","published_at":"2026-09-03T10:00:00Z"},
			{"tag_name":"v1.13.5","draft":true,"published_at":"2026-09-04T10:00:00Z"},
			{"tag_name":"not-a-version","published_at":"2026-09-05T10:00:00Z"}
		]`))
	}))
	defer srv.Close()

	f := &Fetcher{HTTPClient: srv.Client(), URL: srv.URL}
	got, err := f.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Version.String() != "v1.13.4" {
		t.Fatalf("got %+v, want only v1.13.4", got)
	}
	if want := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC); !got[0].PublishedAt.Equal(want) {
		t.Errorf("PublishedAt = %s, want %s", got[0].PublishedAt, want)
	}
}

func Test_Fetcher_List_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	f := &Fetcher{HTTPClient: srv.Client(), URL: srv.URL}
	if _, err := f.List(context.Background()); err == nil {
		t.Fatal("expected error for non-200 status")
	}
}
