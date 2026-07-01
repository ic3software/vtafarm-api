// Package vault talks to the farm's HashiCorp Vault to provision the per-user
// secret isolation the VTA pods rely on. It is the runtime counterpart to
// helm/vtafarm-vault/bootstrap.sh: the bootstrap grants this service (via an
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
	Addr              string // e.g. https://vault.vault.svc:8200
	RoleID            string // AppRole role_id for vtafarm-api
	SecretID          string // AppRole secret_id for vtafarm-api
	KVMount           string // KV v2 mount, default "secret"
	K8sAuthMount      string // kubernetes auth mount, default "kubernetes"
	AppRoleMount      string // approle auth mount, default "approle"
	MediatorTokenRole string // token role for minting mediator VAULT_TOKENs, default "vtafarm-mediator-token"
	SkipVerify        bool   // skip TLS verification (self-signed in-cluster CA)
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
	if cfg.MediatorTokenRole == "" {
		cfg.MediatorTokenRole = "vtafarm-mediator-token"
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

// UserName is the shared name for a user's Vault policy AND kubernetes-auth role.
// full_stack also mints the mediator's VAULT_TOKEN against this same policy
// (extended below to also cover the mediator's KV prefix).
func UserName(userID uint) string {
	return fmt.Sprintf("vta-user-%d", userID)
}

// MediatorPrefix is the KV v2 path (under the mount) where a full_stack
// session's mediator secrets live. The per-user policy written by
// EnsureUserAccess globs over secret/{data,metadata}/mediator/user-<id>/*.
func MediatorPrefix(userID, sessionID uint) string {
	return fmt.Sprintf("mediator/user-%d/session-%d", userID, sessionID)
}

// EnsureUserAccess (idempotently) creates the per-user Vault policy and the
// kubernetes-auth role bound to ServiceAccount `saName` in `namespace`. After
// this, a VTA pod running as that SA can read/write only its own seed paths.
//
// The policy also grants the user's mediator KV prefix (full_stack mode) —
// the mediator's secrets-vault backend probes write→read→delete at setup and
// startup, so it needs create/update/read/delete on data and read/list/delete
// on metadata, mirroring the VTA seed grant above it.
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
			`path "%[1]s/metadata/mediator/user-%[2]d/*" { capabilities = ["read", "list", "delete"] }`,
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

// MintMediatorToken creates a periodic token scoped to the user's policy
// (which EnsureUserAccess has already extended to cover the mediator's KV
// prefix) for the mediator binary's VAULT_TOKEN (token auth, secrets-vault
// feature). Minting a child token whose policies aren't a subset of the
// caller's own policies requires a Vault token role with
// allowed_policies_glob — see helm/vtafarm-vault/bootstrap.sh
// (vtafarm-mediator-token). That role already bakes in period=720h and
// orphan=true, so the token comes out periodic/orphan automatically —
// requesting "period"/"no_parent" explicitly here would make Vault demand
// root/sudo on the caller (a non-privileged AppRole token can only inherit
// those from the role, not set them itself).
func (c *Client) MintMediatorToken(ctx context.Context, userID, sessionID uint) (string, error) {
	token, err := c.login(ctx)
	if err != nil {
		return "", err
	}
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	body := map[string]any{
		"policies":     []string{UserName(userID)},
		"display_name": fmt.Sprintf("mediator-user-%d-session-%d", userID, sessionID),
	}
	path := fmt.Sprintf("/v1/auth/token/create/%s", c.cfg.MediatorTokenRole)
	if err := c.do(ctx, http.MethodPost, path, token, body, &out); err != nil {
		return "", fmt.Errorf("mint mediator token: %w", err)
	}
	if out.Auth.ClientToken == "" {
		return "", fmt.Errorf("mint mediator token: empty client_token in response")
	}
	return out.Auth.ClientToken, nil
}

// RevokeToken revokes a token minted by MintMediatorToken. Best-effort —
// called during full_stack teardown.
func (c *Client) RevokeToken(ctx context.Context, mediatorToken string) error {
	token, err := c.login(ctx)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/v1/auth/token/revoke", token,
		map[string]string{"token": mediatorToken}, nil)
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
