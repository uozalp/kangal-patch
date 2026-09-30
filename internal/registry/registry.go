// Package registry checks whether an image tag exists in an OCI registry.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// ErrNotFound means the registry has no manifest for the requested image.
var ErrNotFound = errors.New("image not found")

const manifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

var authParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// Checker queries registries anonymously over HTTPS.
type Checker struct {
	HTTPClient *http.Client
}

// Exists returns nil if image ("host/repo/path:tag") has a manifest, ErrNotFound if the registry
// reports none, and another error if the registry couldn't be queried.
func (c *Checker) Exists(ctx context.Context, image string) error {
	host, repo, tag, err := parseImage(image)
	if err != nil {
		return err
	}

	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, repo, tag)

	resp, err := c.head(ctx, manifestURL, "")
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		_ = resp.Body.Close()

		token, err := c.fetchToken(ctx, challenge)
		if err != nil {
			return fmt.Errorf("registry %s authentication failed: %w", host, err)
		}
		if resp, err = c.head(ctx, manifestURL, token); err != nil {
			return err
		}
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, image)
	default:
		return fmt.Errorf("registry %s returned status %d for %s", host, resp.StatusCode, image)
	}
}

func (c *Checker) head(ctx context.Context, target, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.HTTPClient.Do(req)
}

// fetchToken resolves an anonymous pull token from a `Bearer realm=...,service=...,scope=...` challenge.
func (c *Checker) fetchToken(ctx context.Context, challenge string) (string, error) {
	if !strings.HasPrefix(challenge, "Bearer ") {
		return "", fmt.Errorf("unsupported auth challenge %q", challenge)
	}

	params := map[string]string{}
	for _, m := range authParam.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}

	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" {
		return "", fmt.Errorf("invalid token realm %q", params["realm"])
	}

	q := realm.Query()
	if v := params["service"]; v != "" {
		q.Set("service", v)
	}
	if v := params["scope"]; v != "" {
		q.Set("scope", v)
	}
	realm.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned status %d", resp.StatusCode)
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("invalid token response: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", errors.New("token response contained no token")
}

// parseImage splits "host/repo/path:tag" into its parts.
func parseImage(image string) (host, repo, tag string, err error) {
	slash := strings.Index(image, "/")
	if slash < 0 {
		return "", "", "", fmt.Errorf("image %q has no registry host", image)
	}
	host, rest := image[:slash], image[slash+1:]

	colon := strings.LastIndex(rest, ":")
	if colon < 0 || strings.Contains(rest[colon:], "/") {
		return "", "", "", fmt.Errorf("image %q has no tag", image)
	}
	return host, rest[:colon], rest[colon+1:], nil
}
