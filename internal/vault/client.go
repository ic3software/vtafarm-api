// Package vault talks to the farm's HashiCorp Vault to provision the per-user
// secret isolation the VTA pods rely on. It is the runtime counterpart to
// vtafarm-k8s' scripts/vault-bootstrap.sh: the bootstrap grants this service (via an
// AppRole) the *ability* to manage `vta-user-*` policies and Kubernetes-auth
// roles; this client exercises that ability when a VTA is created or torn down.
//
// It deliberately uses net/http (like the cloudflare/ghcr/didhosting clients)
// rather than pulling in the full hashicorp/vault SDK — the surface is small:
// AppRole login + a handful of policy/role/secret writes and deletes.
package vault

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config holds everything needed to authenticate and address Vault. It mirrors
// the VAULT_* env vars (see internal/config) but is its own type so this
// package doesn't depend on internal/config.
type Config struct {
	Addr         string // e.g. https://vault.vault.svc:8200
	RoleID       string // AppRole role_id for vtafarm-api
	SecretID     string // AppRole secret_id for vtafarm-api
	KVMount      string // KV v2 mount, default "secret"
	K8sAuthMount string // kubernetes auth mount, default "kubernetes"
	AppRoleMount string // approle auth mount, default "approle"
	SkipVerify   bool   // skip TLS verification (self-signed in-cluster CA)
}

type Client struct {
	cfg  Config
	http *http.Client
}

// New returns a Vault client, or an error if the required fields are missing.
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" || cfg.RoleID == "" || cfg.SecretID == "" {
		return nil, fmt.Errorf("vault: VAULT_ADDR, VAULT_ROLE_ID and VAULT_SECRET_ID are all required")
	}
	if cfg.KVMount == "" {
		cfg.KVMount = "secret"
	}
	if cfg.K8sAuthMount == "" {
		cfg.K8sAuthMount = "kubernetes"
	}
	if cfg.AppRoleMount == "" {
		cfg.AppRoleMount = "approle"
	}
	tr := &http.Transport{}
	if cfg.SkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: 15 * time.Second, Transport: tr}}, nil
}

// KVMount returns the configured KV v2 mount (e.g. "secret").
func (c *Client) KVMount() string { return c.cfg.KVMount }

// SeedPath is the KV v2 path (under the mount) where a session's master seed
// lives. The farm Vault policy globs over secret/{data,metadata}/vta/user-<id>/*.
func SeedPath(userID, sessionID uint) string {
	return fmt.Sprintf("vta/user-%d/session-%d/master-seed", userID, sessionID)
}

// UserName is the shared name for a user's Vault policy AND kubernetes-auth
// role. full_stack's mediator and dids daemon both authenticate as the same
// SA against this same role (extended below to also cover their KV prefixes).
func UserName(userID uint) string {
	return fmt.Sprintf("vta-user-%d", userID)
}

// MediatorPrefix is the KV v2 path (under the mount) where a full_stack
// session's mediator secrets live. The per-user policy written by
// EnsureUserAccess globs over secret/{data,metadata}/mediator/user-<id>/*.
func MediatorPrefix(userID, sessionID uint) string {
	return fmt.Sprintf("mediator/user-%d/session-%d", userID, sessionID)
}

// DidsPrefix is the KV v2 path (under the mount) where a full_stack session's
// DID-hosting daemon secrets live. The per-user policy written by
// EnsureUserAccess globs over secret/{data,metadata}/dids/user-<id>/*.
func DidsPrefix(userID, sessionID uint) string {
	return fmt.Sprintf("dids/user-%d/session-%d", userID, sessionID)
}

// VtcPrefix is the KV v2 path (under the mount) where a full_stack
// session's VTC key bundle lives. EnsureUserAccess globs over
// secret/{data,metadata}/vtc/user-<id>/*, mirroring MediatorPrefix/DidsPrefix.
func VtcPrefix(userID, sessionID uint) string {
	return fmt.Sprintf("vtc/user-%d/session-%d", userID, sessionID)
}

// EnsureUserAccess (idempotently) creates the per-user Vault policy and the
// kubernetes-auth role bound to ServiceAccount `saName` in `namespace`. After
// this, a VTA pod running as that SA can read/write only its own seed paths.
//
// The policy also grants the user's mediator, dids, and vtc KV prefixes
// (full_stack mode) — those components all
// authenticate to Vault the same way the VTA does (kubernetes auth, same SA,
// same role), so one shared policy covers all four. Each needs
// create/update/read/delete on data plus read/delete (and list, where the
// component enumerates its keys) on metadata, mirroring the VTA seed grant.
func (c *Client) EnsureUserAccess(ctx context.Context, userID uint, namespace, saName string) error {
	token, err := c.login(ctx)
	if err != nil {
		return err
	}
	name := UserName(userID)

	policy := fmt.Sprintf(
		`path "%[1]s/data/vta/user-%[2]d/*" { capabilities = ["read", "create", "update", "delete"] }`+"\n"+
			`path "%[1]s/metadata/vta/user-%[2]d/*" { capabilities = ["read", "delete"] }`+"\n"+
			`path "%[1]s/data/mediator/user-%[2]d/*" { capabilities = ["create", "update", "read", "delete"] }`+"\n"+
			`path "%[1]s/metadata/mediator/user-%[2]d/*" { capabilities = ["read", "list", "delete"] }`+"\n"+
			`path "%[1]s/data/dids/user-%[2]d/*" { capabilities = ["create", "update", "read", "delete"] }`+"\n"+
			`path "%[1]s/metadata/dids/user-%[2]d/*" { capabilities = ["read", "list", "delete"] }`+"\n"+
			`path "%[1]s/data/vtc/user-%[2]d/*" { capabilities = ["read", "create", "update", "delete"] }`+"\n"+
			`path "%[1]s/metadata/vtc/user-%[2]d/*" { capabilities = ["read", "delete"] }`,
		c.cfg.KVMount, userID,
	)
	if err := c.do(ctx, http.MethodPut, "/v1/sys/policies/acl/"+name, token,
		map[string]string{"policy": policy}, nil); err != nil {
		return fmt.Errorf("write policy %s: %w", name, err)
	}

	role := map[string]any{
		"bound_service_account_names":      []string{saName},
		"bound_service_account_namespaces": []string{namespace},
		"token_policies":                   []string{name},
		"ttl":                              "1h",
	}
	if err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/v1/auth/%s/role/%s", c.cfg.K8sAuthMount, name), token, role, nil); err != nil {
		return fmt.Errorf("write k8s role %s: %w", name, err)
	}
	return nil
}

// DeleteUserAccess removes the per-user policy + kubernetes-auth role.
// Best-effort: call this only when a user has no remaining sessions.
func (c *Client) DeleteUserAccess(ctx context.Context, userID uint) error {
	token, err := c.login(ctx)
	if err != nil {
		return err
	}
	name := UserName(userID)
	_ = c.do(ctx, http.MethodDelete, fmt.Sprintf("/v1/auth/%s/role/%s", c.cfg.K8sAuthMount, name), token, nil, nil)
	_ = c.do(ctx, http.MethodDelete, "/v1/sys/policies/acl/"+name, token, nil, nil)
	return nil
}

// DeleteSeed destroys all versions of a session's seed (KV v2 metadata delete).
func (c *Client) DeleteSeed(ctx context.Context, secretPath string) error {
	token, err := c.login(ctx)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/v1/%s/metadata/%s", c.cfg.KVMount, secretPath), token, nil, nil)
}

// DeleteMediatorSecrets destroys all versions of a full_stack session's
// mediator secrets (KV v2 metadata delete) — mirrors DeleteSeed.
func (c *Client) DeleteMediatorSecrets(ctx context.Context, userID, sessionID uint) error {
	token, err := c.login(ctx)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/v1/%s/metadata/%s", c.cfg.KVMount, MediatorPrefix(userID, sessionID)), token, nil, nil)
}

// DeleteDidsSecrets destroys all versions of a full_stack session's dids
// daemon secrets (KV v2 metadata delete) — mirrors DeleteMediatorSecrets.
func (c *Client) DeleteDidsSecrets(ctx context.Context, userID, sessionID uint) error {
	token, err := c.login(ctx)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/v1/%s/metadata/%s", c.cfg.KVMount, DidsPrefix(userID, sessionID)), token, nil, nil)
}

// DeleteVtcSecrets destroys all versions of a full_stack session's
// VTC key bundle (KV v2 metadata delete) — mirrors DeleteMediatorSecrets.
func (c *Client) DeleteVtcSecrets(ctx context.Context, userID, sessionID uint) error {
	token, err := c.login(ctx)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/v1/%s/metadata/%s", c.cfg.KVMount, VtcPrefix(userID, sessionID)), token, nil, nil)
}

// login exchanges the AppRole role_id/secret_id for a short-lived token. The
// provisioning operations are infrequent (per VTA create/delete), so we log in
// fresh each time rather than maintaining a renewed token in the background.
func (c *Client) login(ctx context.Context) (string, error) {
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	body := map[string]string{"role_id": c.cfg.RoleID, "secret_id": c.cfg.SecretID}
	if err := c.do(ctx, http.MethodPost, "/v1/auth/"+c.cfg.AppRoleMount+"/login", "", body, &out); err != nil {
		return "", fmt.Errorf("approle login: %w", err)
	}
	if out.Auth.ClientToken == "" {
		return "", fmt.Errorf("approle login: empty client_token in response")
	}
	return out.Auth.ClientToken, nil
}

func (c *Client) do(ctx context.Context, method, path, token string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.cfg.Addr, "/")+path, rdr)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}
