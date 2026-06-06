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
	"time"

	"github.com/google/uuid"
)

const authenticateType = "https://trusttasks.org/spec/auth/authenticate/0.1"

// Client uploads DID logs to a webvh hosting service using the DIDComm
// challenge-response auth flow (JWS EdDSA signed).
type Client struct {
	baseURL    string
	clientDid  string // our did:key — goes in JWS `from:` and the challenge request
	signingKid string // clientDid + "#key-0"
	privKey    ed25519.PrivateKey
	hc         *http.Client
}

// New constructs a Client. privKeyB64 is the base64 (StdEncoding) of the
// 32-byte Ed25519 seed produced by `make gen-keypair`.
func New(baseURL, clientDid, privKeyB64 string) (*Client, error) {
	seed, err := base64.StdEncoding.DecodeString(privKeyB64)
	if err != nil {
		return nil, fmt.Errorf("decode DID_HOSTING_PRIVATE_KEY: %w", err)
	}
	if len(seed) != 32 {
		return nil, fmt.Errorf("DID_HOSTING_PRIVATE_KEY: expected 32-byte seed, got %d", len(seed))
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		clientDid:  clientDid,
		signingKid: clientDid + "#key-0",
		privKey:    ed25519.NewKeyFromSeed(seed),
		hc:         &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// RegisterDid authenticates with the hosting service and atomically
// claims + publishes the DID log at the given path.
// path is e.g. "user-nd5y4gpn/pvta" (everything after baseURL/).
func (c *Client) RegisterDid(ctx context.Context, path, didLog string) error {
	token, err := c.authenticate(ctx)
	if err != nil {
		return fmt.Errorf("did-hosting authenticate: %w", err)
	}
	return c.registerAtomic(ctx, token, path, didLog)
}

// authenticate runs the DIDComm challenge-response flow and returns an access token.
func (c *Client) authenticate(ctx context.Context) (string, error) {
	challengeBody, _ := json.Marshal(map[string]string{"did": c.clientDid})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/auth/challenge", bytes.NewReader(challengeBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("challenge request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("challenge response %d: %s", resp.StatusCode, body)
	}

	var challengeResp struct {
		SessionId string `json:"sessionId"`
		Data      struct {
			Challenge string `json:"challenge"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&challengeResp); err != nil {
		return "", fmt.Errorf("decode challenge response: %w", err)
	}

	jws, err := c.buildAuthJWS(challengeResp.SessionId, challengeResp.Data.Challenge)
	if err != nil {
		return "", fmt.Errorf("build jws: %w", err)
	}

	req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/auth/", bytes.NewBufferString(jws))
	req2.Header.Set("Content-Type", "application/didcomm-signed+json")
	resp2, err := c.hc.Do(req2)
	if err != nil {
		return "", fmt.Errorf("authenticate request: %w", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		return "", fmt.Errorf("authenticate response %d: %s", resp2.StatusCode, body)
	}

	var authResp struct {
		Data struct {
			AccessToken string `json:"accessToken"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&authResp); err != nil {
		return "", fmt.Errorf("decode auth response: %w", err)
	}
	return authResp.Data.AccessToken, nil
}

// buildAuthJWS constructs the JWS General JSON signed DIDComm message for POST /api/auth/.
// Wire format matches affinidi-messaging-didcomm sign_ed25519:
//
//	{"payload": base64url(msg), "signatures": [{"protected": base64url(header), "signature": base64url(sig)}]}
//
// Signing input: base64url(header) + "." + base64url(msg)
func (c *Client) buildAuthJWS(sessionId, challenge string) (string, error) {
	now := uint64(time.Now().Unix())

	msg := map[string]any{
		"id":           uuid.New().String(),
		"type":         authenticateType,
		"from":         c.clientDid,
		"created_time": now,
		"body": map[string]string{
			"session_id": sessionId,
			"challenge":  challenge,
		},
	}
	payloadBytes, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}

	header := map[string]any{
		"typ": "application/didcomm-signed+json",
		"alg": "EdDSA",
		"kid": c.signingKid,
	}
	headerBytes, err := json.Marshal(header)
	if err != nil {
		return "", err
	}

	headerB64 := base64.RawURLEncoding.EncodeToString(headerBytes)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadBytes)
	signingInput := headerB64 + "." + payloadB64
	sig := ed25519.Sign(c.privKey, []byte(signingInput))

	jws := map[string]any{
		"payload": payloadB64,
		"signatures": []map[string]string{{
			"protected": headerB64,
			"signature": base64.RawURLEncoding.EncodeToString(sig),
		}},
	}
	result, err := json.Marshal(jws)
	return string(result), err
}

// registerAtomic calls POST /api/dids/register — a single round-trip that
// atomically claims the path and publishes the DID log.
func (c *Client) registerAtomic(ctx context.Context, token, path, didLog string) error {
	body, _ := json.Marshal(map[string]any{
		"path":    path,
		"method":  "webvh",
		"didData": didLog,
		"force":   false,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/dids/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.hc.Do(req)
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
