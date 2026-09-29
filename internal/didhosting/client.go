package didhosting

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/google/uuid"

	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/siop"
	"github.com/ic3software/vtafarm-api/internal/siop/webvh"
)

const (
	trustTaskEndpoint = "/api/trust-tasks"

	didRegisterType = "https://trusttasks.org/spec/did-management/did/register/0.1"
	didDeleteType   = "https://trusttasks.org/spec/did-management/did/delete/0.1"
	aclGrantType    = "https://trusttasks.org/spec/acl/grant/0.1"
	aclRevokeType   = "https://trusttasks.org/spec/acl/revoke/0.1"

	maxTrustTaskResponseBytes = 1 << 20
)

type trustTask struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	ThreadID  string          `json:"threadId,omitempty"`
	Issuer    string          `json:"issuer,omitempty"`
	Recipient string          `json:"recipient,omitempty"`
	IssuedAt  string          `json:"issuedAt,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	Proof     *proof          `json:"proof,omitempty"`
}

type proof struct {
	Type               string `json:"type"`
	Cryptosuite        string `json:"cryptosuite"`
	VerificationMethod string `json:"verificationMethod"`
	Created            string `json:"created"`
	ProofPurpose       string `json:"proofPurpose"`
	ProofValue         string `json:"proofValue,omitempty"`
}

// Client talks to one DID Hosting control plane exclusively through signed
// Trust Tasks. serverDid is supplied by provisioning and is both the request
// recipient and the identity every response proof must authenticate.
type Client struct {
	baseURL   string
	clientDid string
	serverDid string
	kid       string
	privKey   ed25519.PrivateKey
	hc        *http.Client
	resolver  siop.AuthenticationKeyResolver
}

func (c *Client) ServerDid() string { return c.serverDid }

// New constructs a client for the current Trust Task-only management surface.
// Both identities are mandatory: the removed /api/server-info route is not a
// discovery fallback, and requests cannot be signed without a recipient.
func New(baseURL, clientDid, privKeyB64, serverDid string) (*Client, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil, errors.New("DID hosting control URL is required")
	}
	if clientDid == "" {
		return nil, errors.New("DID_HOSTING_DID is required")
	}
	if serverDid == "" {
		return nil, errors.New("DID hosting service DID is required")
	}

	seed, err := base64.StdEncoding.DecodeString(privKeyB64)
	if err != nil {
		return nil, fmt.Errorf("decode DID_HOSTING_PRIVATE_KEY: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("DID_HOSTING_PRIVATE_KEY: expected %d-byte seed, got %d", ed25519.SeedSize, len(seed))
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	derivedDid, err := didkey.FromPublicKey(privateKey.Public().(ed25519.PublicKey))
	if err != nil {
		return nil, fmt.Errorf("derive DID_HOSTING_DID: %w", err)
	}
	if derivedDid != clientDid {
		return nil, errors.New("DID_HOSTING_PRIVATE_KEY does not match DID_HOSTING_DID")
	}

	multibase := strings.TrimPrefix(clientDid, "did:key:")
	if multibase == clientDid || multibase == "" {
		return nil, errors.New("DID_HOSTING_DID must be a did:key")
	}

	return &Client{
		baseURL:   baseURL,
		clientDid: clientDid,
		serverDid: serverDid,
		kid:       clientDid + "#" + multibase,
		privKey:   privateKey,
		hc: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		resolver: siop.MethodResolver{WebVH: webvh.NewResolver(5 * time.Second)},
	}, nil
}

// RegisterDid atomically claims and publishes a signed did:webvh log.
func (c *Client) RegisterDid(ctx context.Context, path, didLog string) error {
	_, err := c.call(ctx, didRegisterType, map[string]any{
		"path":    path,
		"method":  "webvh",
		"didData": didLog,
		"force":   false,
	})
	if err != nil {
		return fmt.Errorf("register DID: %w", err)
	}
	return nil
}

// CreateAcl grants an ACL entry. acl/grant is idempotent when the subject
// already has the same role, so no legacy conflict special case is required.
func (c *Client) CreateAcl(ctx context.Context, did, role, label string) error {
	entry := map[string]any{"subject": did, "role": role}
	if label != "" {
		entry["label"] = label
	}
	_, err := c.call(ctx, aclGrantType, map[string]any{"entry": entry})
	if err != nil {
		return fmt.Errorf("grant ACL entry: %w", err)
	}
	return nil
}

// DeleteAcl revokes the complete ACL entry.
func (c *Client) DeleteAcl(ctx context.Context, did string) error {
	_, err := c.call(ctx, aclRevokeType, map[string]any{"subject": did})
	if err != nil {
		return fmt.Errorf("revoke ACL entry: %w", err)
	}
	return nil
}

// DeleteDid removes the DID slot named by mnemonic.
func (c *Client) DeleteDid(ctx context.Context, mnemonic string) error {
	_, err := c.call(ctx, didDeleteType, map[string]any{"mnemonic": mnemonic})
	if err != nil {
		return fmt.Errorf("delete DID: %w", err)
	}
	return nil
}

func (c *Client) call(ctx context.Context, typeURI string, payload any) (json.RawMessage, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Trust Task payload: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	doc := trustTask{
		ID:        uuid.New().String(),
		Type:      typeURI,
		Issuer:    c.clientDid,
		Recipient: c.serverDid,
		IssuedAt:  now,
		Payload:   payloadJSON,
	}
	if err := signTrustTask(&doc, c.kid, c.privKey, now); err != nil {
		return nil, fmt.Errorf("sign Trust Task: %w", err)
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode Trust Task: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+trustTaskEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Trust Task request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send Trust Task: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxTrustTaskResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Trust Task response: %w", err)
	}
	if len(responseBody) > maxTrustTaskResponseBytes {
		return nil, errors.New("DID hosting Trust Task response exceeds 1 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("DID hosting Trust Task returned HTTP %d: %s", resp.StatusCode, responseBody)
	}

	var reply trustTask
	if err := json.Unmarshal(responseBody, &reply); err != nil {
		return nil, fmt.Errorf("decode Trust Task response: %w", err)
	}
	if err := c.verifyReply(ctx, &reply, &doc); err != nil {
		return nil, err
	}
	if reply.Type != typeURI+"#response" {
		return nil, fmt.Errorf("unexpected Trust Task response type %q", reply.Type)
	}
	return reply.Payload, nil
}

func signTrustTask(doc *trustTask, kid string, privateKey ed25519.PrivateKey, created string) error {
	doc.Proof = nil
	proofConfig := proof{
		Type:               "DataIntegrityProof",
		Cryptosuite:        "eddsa-jcs-2022",
		VerificationMethod: kid,
		Created:            created,
		ProofPurpose:       "authentication",
	}
	message, err := proofMessage(doc, proofConfig)
	if err != nil {
		return err
	}
	proofConfig.ProofValue = "z" + siop.EncodeBase58BTC(ed25519.Sign(privateKey, message))
	doc.Proof = &proofConfig
	return nil
}

func (c *Client) verifyReply(ctx context.Context, reply, request *trustTask) error {
	if reply.ThreadID != request.ID {
		return errors.New("DID hosting Trust Task response is not threaded to the request")
	}
	if reply.Issuer != c.serverDid {
		return errors.New("DID hosting Trust Task response issuer does not match the service DID")
	}
	if reply.Recipient != c.clientDid {
		return errors.New("DID hosting Trust Task response recipient does not match DID_HOSTING_DID")
	}
	issuedAt, err := time.Parse(time.RFC3339, reply.IssuedAt)
	if err != nil || issuedAt.Before(time.Now().Add(-5*time.Minute)) || issuedAt.After(time.Now().Add(siop.DefaultClockSkew)) {
		return errors.New("DID hosting Trust Task response is outside the freshness window")
	}
	if reply.Proof == nil {
		return errors.New("DID hosting Trust Task response has no proof")
	}
	p := *reply.Proof
	if p.Type != "DataIntegrityProof" || p.Cryptosuite != "eddsa-jcs-2022" || p.ProofPurpose != "authentication" {
		return errors.New("DID hosting Trust Task response has an unsupported proof")
	}
	proofDid, _, ok := strings.Cut(p.VerificationMethod, "#")
	if !ok || proofDid != c.serverDid {
		return errors.New("DID hosting Trust Task response proof does not belong to the service DID")
	}
	created, err := time.Parse(time.RFC3339, p.Created)
	if err != nil || created.After(time.Now().Add(siop.DefaultClockSkew)) {
		return errors.New("DID hosting Trust Task response proof has an invalid creation time")
	}
	signature, err := decodeProofValue(p.ProofValue)
	if err != nil {
		return err
	}
	publicKey, err := c.resolver.ResolveAuthenticationKey(ctx, c.serverDid, p.VerificationMethod)
	if err != nil {
		return fmt.Errorf("resolve DID hosting response key: %w", err)
	}
	reply.Proof = nil
	message, err := proofMessage(reply, p)
	reply.Proof = &p
	if err != nil {
		return fmt.Errorf("canonicalize Trust Task response: %w", err)
	}
	if len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, message, signature) {
		return errors.New("DID hosting Trust Task response proof signature is invalid")
	}
	return nil
}

func proofMessage(doc *trustTask, p proof) ([]byte, error) {
	p.ProofValue = ""
	proofCanonical, err := canonicalJSON(p)
	if err != nil {
		return nil, fmt.Errorf("canonicalize proof options: %w", err)
	}
	documentCanonical, err := canonicalJSON(doc)
	if err != nil {
		return nil, fmt.Errorf("canonicalize document: %w", err)
	}
	proofHash := sha256.Sum256(proofCanonical)
	documentHash := sha256.Sum256(documentCanonical)
	message := make([]byte, 0, len(proofHash)+len(documentHash))
	message = append(message, proofHash[:]...)
	message = append(message, documentHash[:]...)
	return message, nil
}

func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return jsoncanonicalizer.Transform(encoded)
}

func decodeProofValue(value string) ([]byte, error) {
	if !strings.HasPrefix(value, "z") {
		return nil, errors.New("DID hosting Trust Task proofValue is not base58btc multibase")
	}
	decoded, err := siop.DecodeBase58BTC(value[1:])
	if err != nil || siop.EncodeBase58BTC(decoded) != value[1:] {
		return nil, errors.New("DID hosting Trust Task proofValue is not canonical base58btc")
	}
	return decoded, nil
}
