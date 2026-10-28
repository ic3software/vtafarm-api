// Package vta is the farm's client for a running VTA, over HTTPS, as a
// short-lived did:key the farm minted for one operation.
//
// A VTA has one authenticated surface: Trust Task documents POSTed to
// /trust-tasks. Logging in is two of them, `auth/challenge/0.1` (unsigned)
// and `auth/authenticate/0.3` (signed by the did:key), which yields a bearer
// token. Every later document carries that token and is signed by the same
// key, because the VTA checks that the proof's signer, the document's issuer
// and the session's subject are one DID.
//
// The farm uses this where an operation has to happen inside the running VTA
// rather than against its stopped store: a did:webvh update is one. The
// offline `vta did-mgmt dids edit` holds no mediator session, so it can
// publish only to a DID host that advertises an HTTPS endpoint; a host that
// advertises TSP or DIDComm alone, which is what a host provisioned through a
// VTA with a mediator gets, is reached by the running VTA alone.
package vta

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/didresolver"
	"github.com/ic3software/vtafarm-api/internal/trusttaskclient"
	"github.com/ic3software/vtafarm-api/internal/webvh"
)

const (
	trustTasksPath = "/trust-tasks"

	challengeType    = "https://trusttasks.org/spec/auth/challenge/0.1"
	authenticateType = "https://trusttasks.org/spec/auth/authenticate/0.3"
	didGetType       = "https://trusttasks.org/spec/vta/webvh/dids/get/1.0"
	didUpdateType    = "https://trusttasks.org/spec/vta/webvh/dids/update/1.0"
	domainsType      = "https://trusttasks.org/spec/vta/webvh/servers/domains/0.1"
	errorType        = "https://trusttasks.org/spec/trust-task-error/0.6"

	// ReasonConflict is `details.reason` when `expectedVersionId` no longer
	// names the DID's local head.
	ReasonConflict = "conflict"
	// ReasonNotFound is `details.reason` for a DID the VTA does not hold, or
	// one the caller may not update; the VTA does not say which.
	ReasonNotFound = "not_found"
)

// Client is one did:key's session with one VTA.
type Client struct {
	baseURL  string
	vtaDid   string
	did      string
	kid      string
	key      ed25519.PrivateKey
	hc       *http.Client
	resolver didresolver.AuthenticationKeyResolver
	now      func() time.Time
	token    string
	// probeInterval paces WaitUntilHostReachable.
	probeInterval time.Duration
}

// NewEphemeral mints a fresh Ed25519 did:key, held in memory only, for a
// VTA reached at baseURL whose DID is vtaDid. The key has no standing until
// the VTA's ACL names it; the caller grants that, scoped and expiring, and
// discards the client when the operation is done.
func NewEphemeral(baseURL, vtaDid string) (*Client, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mint ephemeral VTA client key: %w", err)
	}
	return newClient(baseURL, vtaDid, key)
}

func newClient(baseURL, vtaDid string, key ed25519.PrivateKey) (*Client, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil, errors.New("VTA URL is required")
	}
	if vtaDid == "" {
		return nil, errors.New("VTA DID is required")
	}
	did, err := didkey.FromPublicKey(key.Public().(ed25519.PublicKey))
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL: baseURL,
		vtaDid:  vtaDid,
		did:     did,
		kid:     did + "#" + strings.TrimPrefix(did, "did:key:"),
		key:     key,
		hc: &http.Client{
			Timeout: 60 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		resolver:      didresolver.MethodResolver{WebVH: webvh.NewResolver(5 * time.Second)},
		now:           time.Now,
		probeInterval: 5 * time.Second,
	}, nil
}

// DID is the client's did:key, the subject of the ACL entry it needs.
func (c *Client) DID() string { return c.did }

// Login runs the challenge and authenticate tasks and keeps the access token
// for the calls that follow. The VTA hands a challenge to any DID and refuses
// the authenticate step for one its ACL does not name, so a missing or
// expired grant surfaces here as a `permissionDenied` TaskError.
func (c *Client) Login(ctx context.Context) error {
	var challenge struct {
		Challenge string `json:"challenge"`
		SessionID string `json:"sessionId"`
	}
	reply, err := c.send(ctx, challengeType, map[string]any{"subject": c.did}, false, false)
	if err != nil {
		return fmt.Errorf("VTA challenge: %w", err)
	}
	if err := json.Unmarshal(reply, &challenge); err != nil || challenge.Challenge == "" || challenge.SessionID == "" {
		return errors.New("VTA challenge response is missing the challenge or session id")
	}

	var tokens struct {
		Tokens struct {
			AccessToken string `json:"accessToken"`
		} `json:"tokens"`
	}
	reply, err = c.send(ctx, authenticateType, map[string]any{"challenge": challenge.Challenge, "sessionId": challenge.SessionID}, true, false)
	if err != nil {
		return fmt.Errorf("VTA authenticate: %w", err)
	}
	if err := json.Unmarshal(reply, &tokens); err != nil || tokens.Tokens.AccessToken == "" {
		return errors.New("VTA authenticate response carries no access token")
	}
	c.token = tokens.Tokens.AccessToken
	return nil
}

// DIDLog returns the did.jsonl the VTA holds for did, one log entry per
// line. The last line is the local head, which may be ahead of what the DID
// host serves if an earlier publish failed; the VTA re-publishes it before
// its next update.
func (c *Client) DIDLog(ctx context.Context, did string) (string, error) {
	reply, err := c.send(ctx, didGetType, map[string]any{"did": did, "includeLog": true}, true, true)
	if err != nil {
		return "", fmt.Errorf("read the DID log from the VTA: %w", err)
	}
	var got struct {
		Log string `json:"log"`
	}
	if err := json.Unmarshal(reply, &got); err != nil || strings.TrimSpace(got.Log) == "" {
		return "", errors.New("the VTA returned no DID log")
	}
	return got.Log, nil
}

// UpdateDIDDocument replaces did's document and has the VTA publish the new
// log entry to the DID host over whichever transport the two share. The VTA
// rotates the DID's update key as part of any document change. With
// expectedVersionID set, the VTA refuses (ReasonConflict) if its head has
// moved since the caller read it. It returns the new versionId.
func (c *Client) UpdateDIDDocument(ctx context.Context, did string, document json.RawMessage, expectedVersionID string) (string, error) {
	payload := map[string]any{"did": did, "document": document}
	if expectedVersionID != "" {
		payload["expectedVersionId"] = expectedVersionID
	}
	reply, err := c.send(ctx, didUpdateType, payload, true, true)
	if err != nil {
		return "", fmt.Errorf("update the DID document at the VTA: %w", err)
	}
	var got struct {
		NewVersionID string `json:"newVersionId"`
		Serverless   bool   `json:"serverless"`
	}
	if err := json.Unmarshal(reply, &got); err != nil || got.NewVersionID == "" {
		return "", errors.New("the VTA reported no new version id")
	}
	if got.Serverless {
		return "", fmt.Errorf("the VTA holds %s as a serverless DID; nothing published it", did)
	}
	return got.NewVersionID, nil
}

// HostReachable asks the VTA for the hosting domains it may mint into on the
// DID host registered as serverID (`vta/webvh/servers/domains/0.1`), a
// read-only task the VTA can answer only by reaching that host over a
// transport the two share. It is the probe for the one state a caller cannot
// see from outside: right after a restart the VTA answers HTTPS before its
// mediator session is up, and in that window it can initiate REST alone, so a
// host that advertises TSP or DIDComm only is out of its reach and a document
// update would commit locally and then fail to publish.
func (c *Client) HostReachable(ctx context.Context, serverID string) error {
	_, err := c.send(ctx, domainsType, map[string]any{"serverId": serverID}, true, true)
	return err
}

// WaitUntilHostReachable polls HostReachable until it succeeds, the timeout
// passes, or ctx ends. The last refusal is returned on timeout.
func (c *Client) WaitUntilHostReachable(ctx context.Context, serverID string, timeout time.Duration) error {
	deadline := c.now().Add(timeout)
	for {
		err := c.HostReachable(ctx, serverID)
		if err == nil {
			return nil
		}
		if !c.now().Before(deadline) {
			return fmt.Errorf("the VTA could not reach DID host %q within %s: %w", serverID, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.probeInterval):
		}
	}
}

// TaskError is a Trust Task refusal, as the VTA's `trust-task-error/0.5`
// document carries it.
type TaskError struct {
	Status  int
	Code    string
	Reason  string
	Message string
}

func (e *TaskError) Error() string {
	text := fmt.Sprintf("the VTA refused the task: %s", e.Code)
	if e.Reason != "" {
		text += " (" + e.Reason + ")"
	}
	if e.Message != "" {
		text += ": " + e.Message
	}
	return text
}

// Conflict reports an `expectedVersionId` that no longer named the head.
func (e *TaskError) Conflict() bool { return e.Reason == ReasonConflict }

// send POSTs one Trust Task and returns the verified reply's payload. signed
// attaches the did:key's proof; bearer sends the session token, which the
// login tasks must not carry.
func (c *Client) send(ctx context.Context, typeURI string, payload any, signed, bearer bool) (json.RawMessage, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Trust Task payload: %w", err)
	}
	now := c.now().UTC().Format(time.RFC3339)
	doc := trusttaskclient.Document{
		ID:        "urn:uuid:" + uuid.New().String(),
		Type:      typeURI,
		Issuer:    c.did,
		Recipient: c.vtaDid,
		IssuedAt:  now,
		Payload:   payloadJSON,
	}
	if signed {
		if err := trusttaskclient.Sign(&doc, c.kid, c.key, now); err != nil {
			return nil, fmt.Errorf("sign Trust Task: %w", err)
		}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode Trust Task: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+trustTasksPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Trust Task request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Trust-Task", typeURI)
	if bearer {
		if c.token == "" {
			return nil, errors.New("not logged in to the VTA")
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send Trust Task: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, trusttaskclient.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Trust Task response: %w", err)
	}
	if len(responseBody) > trusttaskclient.MaxResponseBytes {
		return nil, errors.New("VTA Trust Task response exceeds 1 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, refusal(resp.StatusCode, responseBody)
	}

	var reply trusttaskclient.Document
	if err := json.Unmarshal(responseBody, &reply); err != nil {
		return nil, fmt.Errorf("decode Trust Task response: %w", err)
	}
	if err := trusttaskclient.VerifyReply(ctx, c.resolver, &reply, &doc, trusttaskclient.Reply{Service: c.vtaDid, Client: c.did, Now: c.now}); err != nil {
		return nil, fmt.Errorf("VTA %w", err)
	}
	if reply.Type != typeURI+"#response" {
		return nil, fmt.Errorf("unexpected Trust Task response type %q", reply.Type)
	}
	return reply.Payload, nil
}

// refusal reads a non-2xx body: a `trust-task-error/0.6` document becomes a
// TaskError; anything else (a flat `{"error":…}`, a proxy page) is quoted.
func refusal(status int, body []byte) error {
	var doc struct {
		Type    string `json:"type"`
		Payload struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &doc); err == nil && doc.Type == errorType && doc.Payload.Code != "" {
		return &TaskError{Status: status, Code: doc.Payload.Code, Reason: doc.Payload.Details.Reason, Message: doc.Payload.Message}
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 512 {
		text = text[:512] + "…"
	}
	return fmt.Errorf("VTA returned HTTP %d: %s", status, text)
}

// LatestLogEntry reads the head of a did.jsonl: its versionId, and the DID
// document (`state`) it establishes.
func LatestLogEntry(didLog string) (versionID string, state json.RawMessage, err error) {
	lines := strings.Split(didLog, "\n")
	var last string
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = lines[i]
			break
		}
	}
	if last == "" {
		return "", nil, errors.New("the DID log is empty")
	}
	var entry struct {
		VersionID string          `json:"versionId"`
		State     json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal([]byte(last), &entry); err != nil {
		return "", nil, fmt.Errorf("parse the DID log head: %w", err)
	}
	if entry.VersionID == "" || len(entry.State) == 0 {
		return "", nil, errors.New("the DID log head has no versionId or state")
	}
	return entry.VersionID, entry.State, nil
}
