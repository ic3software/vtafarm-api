package ghcr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
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
	Tag     string `json:"tag"`
	Image   string `json:"image"`
	Latest  bool   `json:"latest,omitempty"`  // the image "latest" points to (last tagged release)
	Nightly bool   `json:"nightly,omitempty"` // the image "nightly" points to
	Main    bool   `json:"main,omitempty"`    // the image "main" points to
	Created string `json:"created,omitempty"` // image creation time, RFC 3339 UTC; "" when unknown
}

// channelTags are the moving tags the build pipeline repoints on each build of
// its source (devops fpp/build-pipeline.md). They are never rows of their own:
// the versioned tag that shares a channel's digest carries the flag instead,
// so the dropdown only ever offers immutable <version>-<sha> images.
var channelTags = []string{"latest", "nightly", "main"}

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

// ListTags fetches all tags for the package, sorted newest-first by image
// creation time — the same order GitHub shows package versions. Tags whose
// creation time can't be resolved keep a semver-descending fallback order
// after the dated ones. The tags that "latest", "nightly" and "main" point to
// are marked Latest, Nightly and Main; the Latest row is moved to the front.
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

	// Separate versioned tags from the channel tags; semver order is the
	// fallback for tags whose creation time can't be resolved below.
	var tags []string
	present := map[string]bool{}
	for _, t := range body.Tags {
		if slices.Contains(channelTags, t) {
			present[t] = true
		} else {
			tags = append(tags, t)
		}
	}
	sortSemverDesc(tags)

	// Resolve each tag's manifest digest and image creation time, and each
	// present channel tag's digest, best-effort.
	infos := make([]tagInfo, len(tags))
	channelDigest := make([]string, len(channelTags))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(5)
	for i, t := range tags {
		g.Go(func() error {
			infos[i] = c.resolveTag(gctx, registryToken, t)
			return nil
		})
	}
	for i, t := range channelTags {
		if !present[t] {
			continue
		}
		g.Go(func() error {
			channelDigest[i], _ = c.manifestDigest(gctx, registryToken, t)
			return nil
		})
	}
	_ = g.Wait()

	// Newest-first by creation time; the stable sort leaves undated tags in
	// semver order at the end (zero time sorts after everything).
	sort.SliceStable(infos, func(i, j int) bool {
		return infos[i].created.After(infos[j].created)
	})

	// Flag the versioned tag each channel points to by comparing manifest
	// digests. Only the first (newest) match per channel is flagged.
	base := c.FullImage()
	result := make([]ImageTag, len(infos))
	flagged := make([]bool, len(channelTags))
	latestFound := false
	for i, info := range infos {
		r := ImageTag{Tag: info.tag, Image: base + ":" + info.tag}
		if !info.created.IsZero() {
			r.Created = info.created.UTC().Format(time.RFC3339)
		}
		for k, digest := range channelDigest {
			if digest == "" || flagged[k] || info.digest != digest {
				continue
			}
			flagged[k] = true
			switch channelTags[k] {
			case "latest":
				r.Latest = true
				latestFound = true
			case "nightly":
				r.Nightly = true
			case "main":
				r.Main = true
			}
		}
		result[i] = r
	}

	// Ensure the latest-marked tag is first so frontends can default to result[0].
	if latestFound {
		for i, r := range result {
			if r.Latest && i != 0 {
				result[0], result[i] = result[i], result[0]
				break
			}
		}
	}

	return result, nil
}

// tagInfo carries what sorting and latest-matching need per tag.
type tagInfo struct {
	tag     string
	digest  string    // manifest digest, "" when unresolvable
	created time.Time // image creation time, zero when unresolvable
}

const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

// resolveTag fetches a tag's manifest digest and the creation time recorded in
// its image config. Best-effort: fields stay zero on any failure so one bad
// tag can't break the whole listing.
func (c *Client) resolveTag(ctx context.Context, registryToken, tag string) tagInfo {
	info := tagInfo{tag: tag}

	digest, manifest, err := c.fetchManifest(ctx, registryToken, tag)
	if err != nil {
		return info
	}
	info.digest = digest

	// Multi-arch index: descend into the first real platform manifest,
	// skipping buildx attestation entries (platform "unknown").
	if manifest.Config.Digest == "" && len(manifest.Manifests) > 0 {
		for _, m := range manifest.Manifests {
			if m.Platform != nil && m.Platform.OS == "unknown" {
				continue
			}
			_, sub, err := c.fetchManifest(ctx, registryToken, m.Digest)
			if err != nil {
				return info
			}
			manifest = sub
			break
		}
	}
	if manifest.Config.Digest == "" {
		return info
	}

	url := fmt.Sprintf("https://ghcr.io/v2/%s/%s/blobs/%s", c.owner, c.packageName, manifest.Config.Digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return info
	}
	req.Header.Set("Authorization", "Bearer "+registryToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info
	}

	var config struct {
		Created time.Time `json:"created"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&config); err == nil {
		info.created = config.Created
	}
	return info
}

// registryManifest covers both a single image manifest (Config set) and a
// multi-arch index (Manifests set).
type registryManifest struct {
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform *struct {
			OS string `json:"os"`
		} `json:"platform"`
	} `json:"manifests"`
}

// fetchManifest GETs a manifest by tag or digest, returning its content digest
// and decoded body.
func (c *Client) fetchManifest(ctx context.Context, registryToken, ref string) (string, registryManifest, error) {
	var manifest registryManifest
	url := fmt.Sprintf("https://ghcr.io/v2/%s/%s/manifests/%s", c.owner, c.packageName, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", manifest, err
	}
	req.Header.Set("Authorization", "Bearer "+registryToken)
	req.Header.Set("Accept", manifestAccept)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", manifest, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", manifest, fmt.Errorf("manifests/%s: status %d", ref, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return "", manifest, fmt.Errorf("decode manifest %s: %w", ref, err)
	}
	return resp.Header.Get("Docker-Content-Digest"), manifest, nil
}

// manifestDigest returns the Docker-Content-Digest for a given tag.
func (c *Client) manifestDigest(ctx context.Context, registryToken, tag string) (string, error) {
	url := fmt.Sprintf("https://ghcr.io/v2/%s/%s/manifests/%s", c.owner, c.packageName, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+registryToken)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("manifests/%s: status %d", tag, resp.StatusCode)
	}
	return resp.Header.Get("Docker-Content-Digest"), nil
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
	// Drop any suffix like "-1bc81a1" so the last number parses.
	s, _, _ = strings.Cut(s, "-")
	parts := strings.SplitN(s, ".", 3)
	var v [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		v[i], _ = strconv.Atoi(parts[i])
	}
	return v
}
