package didhosting

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	authenticateType = "https://trusttasks.org/spec/auth/authenticate/0.1"
	idTokenTTL       = uint64(300) // seconds; server enforces iat <= now <= exp
)

// Client uploads DID logs to a did-hosting-control service using the
// SIOPv2 challenge-response auth flow defined in
// did-hosting-client/src/auth/message.rs.
type Client struct {
	baseURL   string
	clientDid string // did:key: of this service (iss/sub in id_token)
	serverDid string // server's DID fetched from GET /api/server-info (aud in id_token)
	kid       string // clientDid + "#" + multibase (the z6Mk... part)
	privKey   ed25519.PrivateKey
	hc        *http.Client

	authMu               sync.Mutex
	cachedAccessToken    string
	accessTokenExpiresAt time.Time
}

// ServerDid returns the DID of the did-hosting-control server, fetched from
// /api/server-info at construction time.
func (c *Client) ServerDid() string { return c.serverDid }

// New constructs a Client. privKeyB64 is the base64 (StdEncoding) of the
// 32-byte Ed25519 seed produced by `make gen-keypair`. clientDid is the
// corresponding did:key. Fetches the server DID from /api/server-info at
// construction time.
func New(baseURL, clientDid, privKeyB64 string) (*Client, error) {
	seed, err := base64.StdEncoding.DecodeString(privKeyB64)
	if err != nil {
		return nil, fmt.Errorf("decode DID_HOSTING_PRIVATE_KEY: %w", err)
	}
	if len(seed) != 32 {
		return nil, fmt.Errorf("DID_HOSTING_PRIVATE_KEY: expected 32-byte seed, got %d", len(seed))
	}

	// kid = "<did:key>#<multibase>" where multibase is the z6Mk… part.
	// Matches did-hosting-client/src/auth/message.rs ed25519_did_key().
	multibase := strings.TrimPrefix(clientDid, "did:key:")
	kid := clientDid + "#" + multibase

	hc := &http.Client{Timeout: 30 * time.Second}

	serverDid, err := fetchServerDid(strings.TrimRight(baseURL, "/"), hc)
	if err != nil {
		return nil, fmt.Errorf("fetch server DID from /api/server-info: %w", err)
	}

	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		clientDid: clientDid,
		serverDid: serverDid,
		kid:       kid,
		privKey:   ed25519.NewKeyFromSeed(seed),
		hc:        hc,
	}, nil
}

func fetchServerDid(baseURL string, hc *http.Client) (string, error) {
	resp, err := hc.Get(baseURL + "/api/server-info")
	if err != nil {
		return "", fmt.Errorf("GET /api/server-info: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("server-info %d: %s", resp.StatusCode, body)
	}
	var info struct {
		ServerDid *string `json:"server_did"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("decode server-info: %w", err)
	}
	if info.ServerDid == nil || *info.ServerDid == "" {
		return "", fmt.Errorf("server has no server_did configured")
	}
	return *info.ServerDid, nil
}

// RegisterDid authenticates with the hosting service and atomically
// claims + publishes the DID log at the given path.
// path is e.g. "nd5y4gpn/pvta" (everything after baseURL/).
func (c *Client) RegisterDid(ctx context.Context, path, didLog string) error {
	resp, err := c.doAuthenticated(ctx, func(token string) (*http.Request, error) {
		body, _ := json.Marshal(map[string]any{
			"path":    path,
			"did_log": didLog,
			"force":   false,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/dids/register", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req, err
	})
	if err != nil {
		return fmt.Errorf("register request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("register response %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

type accessToken struct {
	value     string
	expiresAt time.Time
}

// authenticate runs the SIOPv2 challenge-response flow and returns a cacheable
// access token. A fresh login revokes every older token for the same caller DID,
// so callers must go through getAccessToken rather than invoking this directly.
func (c *Client) authenticate(ctx context.Context) (accessToken, error) {
	// ── Step 1: request a challenge nonce. ────────────────────────────────────
	// Server accepts "subject" (canonical) or "did" (legacy alias).
	challengeBody, _ := json.Marshal(map[string]string{"subject": c.clientDid})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/auth/challenge", bytes.NewReader(challengeBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return accessToken{}, fmt.Errorf("challenge request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return accessToken{}, fmt.Errorf("challenge response %d: %s", resp.StatusCode, body)
	}

	// Wire shape: {"challenge":"…","sessionId":"…","expiresAt":"…"} (flat camelCase).
	var challengeResp struct {
		Challenge string `json:"challenge"`
		SessionId string `json:"sessionId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&challengeResp); err != nil {
		return accessToken{}, fmt.Errorf("decode challenge response: %w", err)
	}

	// ── Step 2: self-issue a SIOPv2 id_token and wrap in Trust-Task envelope. ─
	envelope, err := c.buildAuthEnvelope(challengeResp.SessionId, challengeResp.Challenge)
	if err != nil {
		return accessToken{}, fmt.Errorf("build auth envelope: %w", err)
	}

	req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/auth/", bytes.NewBufferString(envelope))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := c.hc.Do(req2)
	if err != nil {
		return accessToken{}, fmt.Errorf("authenticate request: %w", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		return accessToken{}, fmt.Errorf("authenticate response %d: %s", resp2.StatusCode, body)
	}

	// Wire shape: {"session":{…},"tokens":{"accessToken":"…","tokenType":"…",…}}
	var authResp struct {
		Tokens struct {
			AccessToken string `json:"accessToken"`
			ExpiresIn   uint64 `json:"expiresIn"`
		} `json:"tokens"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&authResp); err != nil {
		return accessToken{}, fmt.Errorf("decode auth response: %w", err)
	}
	if authResp.Tokens.AccessToken == "" {
		return accessToken{}, fmt.Errorf("auth response missing access token")
	}
	return accessToken{
		value:     authResp.Tokens.AccessToken,
		expiresAt: tokenExpiry(authResp.Tokens.AccessToken, authResp.Tokens.ExpiresIn),
	}, nil
}

// getAccessToken serialises only cache refresh/authentication. Ordinary API
// requests run after the lock is released and remain fully concurrent.
func (c *Client) getAccessToken(ctx context.Context) (string, error) {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.cachedAccessToken != "" && time.Now().Add(30*time.Second).Before(c.accessTokenExpiresAt) {
		return c.cachedAccessToken, nil
	}
	token, err := c.authenticate(ctx)
	if err != nil {
		return "", fmt.Errorf("did-hosting authenticate: %w", err)
	}
	c.cachedAccessToken = token.value
	c.accessTokenExpiresAt = token.expiresAt
	return token.value, nil
}

func (c *Client) invalidateAccessToken(token string) {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	// A concurrent request may already have replaced the rejected token. Do not
	// discard that newer credential when handling a late 401 for the old one.
	if c.cachedAccessToken == token {
		c.cachedAccessToken = ""
		c.accessTokenExpiresAt = time.Time{}
	}
}

// doAuthenticated retries one 401 with a newly authenticated token. This also
// recovers when another API replica or operator rotates this DID's token.
func (c *Client) doAuthenticated(
	ctx context.Context,
	buildRequest func(token string) (*http.Request, error),
) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.getAccessToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := buildRequest(token)
		if err != nil {
			return nil, err
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt == 1 {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		c.invalidateAccessToken(token)
	}
	panic("unreachable")
}

func tokenExpiry(token string, expiresIn uint64) time.Time {
	if expiresIn > 0 {
		return time.Now().Add(time.Duration(expiresIn) * time.Second)
	}
	parts := strings.Split(token, ".")
	if len(parts) == 3 {
		if payload, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				ExpiresAt int64 `json:"exp"`
			}
			if json.Unmarshal(payload, &claims) == nil && claims.ExpiresAt > 0 {
				return time.Unix(claims.ExpiresAt, 0)
			}
		}
	}
	// Current servers always provide expiresIn and JWT exp. Avoid treating an
	// unfamiliar response as indefinitely cacheable.
	return time.Now()
}

// buildAuthEnvelope builds the Trust-Task authenticate envelope.
// Wire format per did-hosting-client/src/auth/message.rs::build_authenticate_body:
//
//	{"id":uuid,"type":"…/authenticate/0.1","payload":{"id_token":"<compact JWT>","session_id":"…"}}
//
// The id_token is a compact EdDSA JWS: base64url(header).base64url(payload).base64url(sig)
//
//	header:  {"alg":"EdDSA","typ":"JWT","kid":"<did:key>#<multibase>"}
//	payload: {"iss":<did:key>,"sub":<did:key>,"aud":<serverDid>,"nonce":<challenge>,"iat":<now>,"exp":<now+300>}
//	signing: Ed25519 over UTF-8 bytes of "<header_b64>.<payload_b64>"
func (c *Client) buildAuthEnvelope(sessionId, challenge string) (string, error) {
	now := uint64(time.Now().Unix())
	enc := base64.RawURLEncoding

	header := map[string]any{
		"alg": "EdDSA",
		"typ": "JWT",
		"kid": c.kid,
	}
	jwtPayload := map[string]any{
		"iss":   c.clientDid,
		"sub":   c.clientDid,
		"aud":   c.serverDid,
		"nonce": challenge,
		"iat":   now,
		"exp":   now + idTokenTTL,
	}

	headerBytes, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	payloadBytes, err := json.Marshal(jwtPayload)
	if err != nil {
		return "", err
	}

	headerB64 := enc.EncodeToString(headerBytes)
	payloadB64 := enc.EncodeToString(payloadBytes)
	signingInput := headerB64 + "." + payloadB64
	sig := ed25519.Sign(c.privKey, []byte(signingInput))
	idToken := signingInput + "." + enc.EncodeToString(sig)

	envelope := map[string]any{
		"id":   uuid.New().String(),
		"type": authenticateType,
		"payload": map[string]any{
			"id_token":   idToken,
			"session_id": sessionId,
		},
	}
	result, err := json.Marshal(envelope)
	return string(result), err
}

// CreateAcl adds did to the hosting service's ACL with the given role and label.
// Returns nil if the entry already exists (409 Conflict is treated as success).
func (c *Client) CreateAcl(ctx context.Context, did, role, label string) error {
	body, _ := json.Marshal(map[string]any{
		"did":   did,
		"role":  role,
		"label": label,
	})
	resp, err := c.doAuthenticated(ctx, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/acl", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req, err
	})
	if err != nil {
		return fmt.Errorf("acl create request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("acl create response %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// DeleteAcl removes did from the hosting service's ACL.
// Returns nil if the entry does not exist (404 is treated as success).
func (c *Client) DeleteAcl(ctx context.Context, did string) error {
	resp, err := c.doAuthenticated(ctx, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/api/acl/"+did, nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req, err
	})
	if err != nil {
		return fmt.Errorf("acl delete request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("acl delete response %d: %s", resp.StatusCode, respBody)
}

// DeleteDid deletes the DID at path from the hosting service.
// path is e.g. "nd5y4gpn/pvta".
func (c *Client) DeleteDid(ctx context.Context, path string) error {
	resp, err := c.doAuthenticated(ctx, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/api/dids/"+path, nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req, err
	})
	if err != nil {
		return fmt.Errorf("delete request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete response %d: %s", resp.StatusCode, body)
	}
	return nil
}
