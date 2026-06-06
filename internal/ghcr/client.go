package ghcr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Client lists container tags via the OCI Distribution (Docker Registry v2) API.
// Public images work with no token; pass a GitHub PAT for private packages.
type Client struct {
	token       string // GitHub PAT — optional for public packages
	owner       string // e.g. "ic3software"
	packageName string // e.g. "vta"
	http        *http.Client
}

// ImageTag is one entry in the version dropdown.
type ImageTag struct {
	Tag   string `json:"tag"`
	Image string `json:"image"`
}

func New(token, owner, packageName string) *Client {
	return &Client{
		token:       token,
		owner:       owner,
		packageName: packageName,
		http:        &http.Client{},
	}
}

// FullImage returns the base image path without a tag, e.g. "ghcr.io/ic3software/vta".
func (c *Client) FullImage() string {
	return fmt.Sprintf("ghcr.io/%s/%s", c.owner, c.packageName)
}

// ListTags fetches all tags for the package (excluding "latest"), sorted newest-first by semver.
func (c *Client) ListTags(ctx context.Context) ([]ImageTag, error) {
	registryToken, err := c.fetchRegistryToken(ctx)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("https://ghcr.io/v2/%s/%s/tags/list", c.owner, c.packageName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+registryToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tags/list: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tags/list: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode tags: %w", err)
	}

	// Filter out "latest" and sort newest-first.
	var tags []string
	for _, t := range body.Tags {
		if t != "latest" {
			tags = append(tags, t)
		}
	}
	sortSemverDesc(tags)

	base := c.FullImage()
	result := make([]ImageTag, len(tags))
	for i, t := range tags {
		result[i] = ImageTag{Tag: t, Image: base + ":" + t}
	}
	return result, nil
}

// fetchRegistryToken exchanges credentials (or performs anonymous exchange) for a
// short-lived registry bearer token scoped to pull on this package.
func (c *Client) fetchRegistryToken(ctx context.Context) (string, error) {
	url := fmt.Sprintf(
		"https://ghcr.io/token?scope=repository:%s/%s:pull&service=ghcr.io",
		c.owner, c.packageName,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if c.token != "" {
		// GitHub PAT: use Basic auth with "token:<PAT>"
		creds := base64.StdEncoding.EncodeToString([]byte("token:" + c.token))
		req.Header.Set("Authorization", "Basic "+creds)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token exchange: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	return body.Token, nil
}

// sortSemverDesc sorts version strings like "0.5.1", "0.10.0", "v1.2.3" newest-first.
// Non-semver strings fall to the end, sorted lexicographically.
func sortSemverDesc(tags []string) {
	sort.Slice(tags, func(i, j int) bool {
		a := parseSemver(tags[i])
		b := parseSemver(tags[j])
		for k := range 3 {
			if a[k] != b[k] {
				return a[k] > b[k]
			}
		}
		return tags[i] > tags[j]
	})
}

func parseSemver(s string) [3]int {
	s = strings.TrimPrefix(s, "v")
	parts := strings.SplitN(s, ".", 3)
	var v [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		v[i], _ = strconv.Atoi(parts[i])
	}
	return v
}
