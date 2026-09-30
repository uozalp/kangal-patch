// Package release discovers Talos releases and picks the one an auto-update should roll out.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultURL lists the Talos GitHub releases, newest first.
	DefaultURL = "https://api.github.com/repos/siderolabs/talos/releases?per_page=50"

	// AllowPatch only permits updates within the current minor version.
	AllowPatch = "patch"
	// AllowMinor additionally permits stepping to the next minor version.
	AllowMinor = "minor"

	// The release notes make 100 releases ~10MB.
	maxResponseBytes = 32 << 20
)

// Release is a published stable Talos release.
type Release struct {
	Version     Version
	PublishedAt time.Time
}

// Fetcher lists releases from the GitHub releases API anonymously.
type Fetcher struct {
	HTTPClient *http.Client
	// URL defaults to DefaultURL.
	URL string
}

type githubRelease struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
}

// List returns the stable releases; drafts, prereleases and unparsable tags are skipped.
func (f *Fetcher) List(ctx context.Context) ([]Release, error) {
	url := f.URL
	if url == "" {
		url = DefaultURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "kangal-patch")

	resp, err := f.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch releases: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("releases endpoint returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read releases: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("releases response exceeds %d bytes", maxResponseBytes)
	}

	var raw []githubRelease
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed to decode releases: %w", err)
	}

	releases := make([]Release, 0, len(raw))
	for _, r := range raw {
		if r.Draft || r.Prerelease {
			continue
		}
		v, err := ParseVersion(r.TagName)
		if err != nil || v.Pre != "" {
			continue
		}
		releases = append(releases, Release{Version: v, PublishedAt: r.PublishedAt})
	}
	return releases, nil
}

// Result is the outcome of Select.
type Result struct {
	// Latest is the newest stable release, regardless of age or allow.
	Latest *Release
	// Target is the release to roll out now, nil if there is nothing to do.
	Target *Release
	// Pending is a newer allowed release that is still younger than minAge.
	Pending *Release
}

// Select picks the newest release that is newer than current, permitted by allow, and at least
// minAge old at now.
func Select(releases []Release, current Version, allow string, minAge time.Duration, now time.Time) Result {
	var res Result

	for i := range releases {
		r := &releases[i]
		if res.Latest == nil || r.Version.Compare(res.Latest.Version) > 0 {
			res.Latest = r
		}

		if !permitted(current, r.Version, allow) {
			continue
		}

		if now.Sub(r.PublishedAt) >= minAge {
			if res.Target == nil || r.Version.Compare(res.Target.Version) > 0 {
				res.Target = r
			}
			continue
		}
		if res.Pending == nil || r.Version.Compare(res.Pending.Version) > 0 {
			res.Pending = r
		}
	}

	if res.Pending != nil && res.Target != nil && res.Pending.Version.Compare(res.Target.Version) <= 0 {
		res.Pending = nil
	}
	return res
}

func permitted(current, candidate Version, allow string) bool {
	if candidate.Compare(current) <= 0 || candidate.Major != current.Major {
		return false
	}
	if allow == AllowMinor {
		return candidate.Minor <= current.Minor+1
	}
	return candidate.Minor == current.Minor
}

// Version is a parsed Talos version such as v1.13.4 or v1.14.0-beta.0.
type Version struct {
	Major, Minor, Patch int
	// Pre is the prerelease suffix without the leading dash, empty for a stable release.
	Pre string
}

// ParseVersion parses "vMAJOR.MINOR.PATCH[-pre]"; the leading "v" is optional.
func ParseVersion(s string) (Version, error) {
	core, pre, _ := strings.Cut(strings.TrimPrefix(s, "v"), "-")

	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("invalid version %q: expected MAJOR.MINOR.PATCH", s)
	}

	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("invalid version %q: bad number %q", s, p)
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2], Pre: pre}, nil
}

// Compare returns -1, 0 or 1. A prerelease sorts before its stable release; two prereleases of the
// same core version are compared lexically.
func (v Version) Compare(o Version) int {
	for _, d := range []int{v.Major - o.Major, v.Minor - o.Minor, v.Patch - o.Patch} {
		if d != 0 {
			if d < 0 {
				return -1
			}
			return 1
		}
	}

	switch {
	case v.Pre == o.Pre:
		return 0
	case v.Pre == "":
		return 1
	case o.Pre == "":
		return -1
	default:
		return strings.Compare(v.Pre, o.Pre)
	}
}

// String formats the version with a leading "v".
func (v Version) String() string {
	s := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}
