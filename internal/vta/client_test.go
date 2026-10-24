package vta

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/didresolver"
	"github.com/ic3software/vtafarm-api/internal/trusttaskclient"
)

type identity struct {
	did     string
	kid     string
	private ed25519.PrivateKey
}

func newIdentity(t *testing.T, fill byte) identity {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = fill
	}
	private := ed25519.NewKeyFromSeed(seed)
	did, err := didkey.FromPublicKey(private.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return identity{did: did, kid: did + "#" + strings.TrimPrefix(did, "did:key:"), private: private}
}

// fakeVTA answers the four tasks the client sends the way a VTA does: a
// signed, threaded `#response` from the VTA's own DID (a did:key here, so the
// client's resolver needs no network), the login tasks without a bearer, and
// everything else only with the token the authenticate step handed out.
type fakeVTA struct {
	t       *testing.T
	self    identity
	client  identity
	token   string
	log     string
	updates []map[string]json.RawMessage
	refuse  *TaskError
	// domainsRefusals is how many domains probes to refuse before answering,
	// the shape of a VTA whose mediator session is not up yet.
	domainsRefusals int
	domainsProbes   int
}

func (f *fakeVTA) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != trustTasksPath || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var doc trusttaskclient.Document
		if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
			f.t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if doc.Recipient != f.self.did || doc.Issuer != f.client.did || !strings.HasPrefix(doc.ID, "urn:uuid:") {
			f.t.Errorf("unexpected envelope: %+v", doc)
		}
		if r.Header.Get("Trust-Task") != doc.Type {
			f.t.Errorf("Trust-Task header %q does not name the document type %q", r.Header.Get("Trust-Task"), doc.Type)
		}
		bearer := r.Header.Get("Authorization")
		login := doc.Type == challengeType || doc.Type == authenticateType
		if login && bearer != "" {
			f.t.Errorf("%s carried a bearer token", doc.Type)
		}
		if !login && bearer != "Bearer "+f.token {
			f.t.Errorf("%s carried authorization %q, want the session token", doc.Type, bearer)
		}
		if doc.Type != challengeType {
			f.verifyProof(&doc)
		}
		if f.refuse != nil && doc.Type == didUpdateType {
			f.writeRefusal(w, &doc, f.refuse)
			return
		}
		if doc.Type == domainsType {
			f.domainsProbes++
			if f.domainsRefusals > 0 {
				f.domainsRefusals--
				f.writeRefusal(w, &doc, &TaskError{Status: http.StatusInternalServerError, Code: "internalError", Message: "no transport in common with `did:webvh:QmHost:dids.example`: it advertises [tsp, didcomm] and this agent can initiate [rest]"})
				return
			}
			f.writeReply(w, &doc, doc.Type+"#response", map[string]any{"domains": []any{}, "default": ""})
			return
		}
		var payload any
		switch doc.Type {
		case challengeType:
			var req struct {
				Subject string `json:"subject"`
			}
			_ = json.Unmarshal(doc.Payload, &req)
			if req.Subject != f.client.did {
				f.t.Errorf("challenge subject %q", req.Subject)
			}
			payload = map[string]any{"challenge": "6f1a" + strings.Repeat("ab", 30), "sessionId": "sess-1", "expiresAt": "2026-10-23T12:05:00Z"}
		case authenticateType:
			var req struct {
				Challenge string `json:"challenge"`
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(doc.Payload, &req)
			if req.SessionID != "sess-1" || !strings.HasPrefix(req.Challenge, "6f1a") {
				f.t.Errorf("authenticate payload %s", doc.Payload)
			}
			f.token = "jwt-for-" + f.client.did
			payload = map[string]any{"session": map[string]any{"subject": f.client.did}, "tokens": map[string]any{"accessToken": f.token, "tokenType": "Bearer", "expiresIn": 900}}
		case didGetType:
			payload = map[string]any{"record": map[string]any{"serverId": "dids"}, "log": f.log}
		case didUpdateType:
			var req map[string]json.RawMessage
			_ = json.Unmarshal(doc.Payload, &req)
			f.updates = append(f.updates, req)
			payload = map[string]any{"did": "did:webvh:QmVtc:dids.example:vtc", "newVersionId": "3-QmNew", "serverless": false}
		default:
			f.t.Errorf("unexpected task %s", doc.Type)
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		f.writeReply(w, &doc, doc.Type+"#response", payload)
	}
}

func (f *fakeVTA) verifyProof(doc *trusttaskclient.Document) {
	f.t.Helper()
	if doc.Proof == nil {
		f.t.Errorf("%s carried no proof", doc.Type)
		return
	}
	p := *doc.Proof
	if p.ProofPurpose != "authentication" || p.VerificationMethod != f.client.kid {
		f.t.Errorf("proof %+v", p)
	}
	signature, err := trusttaskclient.DecodeProofValue(p.ProofValue)
	if err != nil {
		f.t.Error(err)
		return
	}
	doc.Proof = nil
	message, err := trusttaskclient.ProofMessage(doc, p)
	doc.Proof = &p
	if err != nil {
		f.t.Error(err)
		return
	}
	key, err := (didresolver.DIDKeyResolver{}).ResolveAuthenticationKey(context.Background(), f.client.did, f.client.kid)
	if err != nil {
		f.t.Error(err)
		return
	}
	if !ed25519.Verify(key, message, signature) {
		f.t.Errorf("%s proof did not verify", doc.Type)
	}
}

func (f *fakeVTA) writeReply(w http.ResponseWriter, request *trusttaskclient.Document, typeURI string, payload any) {
	body, _ := json.Marshal(payload)
	now := time.Now().UTC().Format(time.RFC3339)
	reply := trusttaskclient.Document{
		ID:        "urn:uuid:reply",
		Type:      typeURI,
		ThreadID:  request.ID,
		Issuer:    f.self.did,
		Recipient: f.client.did,
		IssuedAt:  now,
		Payload:   body,
	}
	if err := trusttaskclient.Sign(&reply, f.self.kid, f.self.private, now); err != nil {
		f.t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

func (f *fakeVTA) writeRefusal(w http.ResponseWriter, request *trusttaskclient.Document, refusal *TaskError) {
	details := map[string]any{}
	if refusal.Reason != "" {
		details["reason"] = refusal.Reason
	}
	body, _ := json.Marshal(map[string]any{
		"code":         refusal.Code,
		"message":      refusal.Message,
		"retryable":    false,
		"inResponseTo": map[string]any{"id": request.ID, "typeUri": request.Type},
		"details":      details,
	})
	reply := trusttaskclient.Document{ID: "urn:uuid:err", Type: errorType, ThreadID: request.ID, Issuer: f.self.did, Recipient: f.client.did, IssuedAt: time.Now().UTC().Format(time.RFC3339), Payload: body}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(refusal.Status)
	_ = json.NewEncoder(w).Encode(reply)
}

const testLog = `{"versionId":"1-QmOne","state":{"id":"did:webvh:QmVtc:dids.example:vtc","service":[]}}
{"versionId":"2-QmTwo","state":{"id":"did:webvh:QmVtc:dids.example:vtc","service":[{"id":"did:webvh:QmVtc:dids.example:vtc#tsp","type":"TSPTransport","serviceEndpoint":"did:webvh:QmMed:dids.example:mediator"}]}}
`

func newTestClient(t *testing.T, fake *fakeVTA) *Client {
	t.Helper()
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	client, err := newClient(server.URL+"/", fake.self.did, fake.client.private)
	if err != nil {
		t.Fatal(err)
	}
	client.resolver = didresolver.DIDKeyResolver{}
	return client
}

func TestLoginReadAndUpdateOverOneEndpoint(t *testing.T) {
	fake := &fakeVTA{t: t, self: newIdentity(t, 1), client: newIdentity(t, 2), log: testLog}
	client := newTestClient(t, fake)
	ctx := context.Background()

	if client.DID() != fake.client.did {
		t.Fatalf("client DID %q", client.DID())
	}
	if _, err := client.DIDLog(ctx, "did:webvh:QmVtc:dids.example:vtc"); err == nil {
		t.Fatal("an authenticated task went out before login")
	}
	if err := client.Login(ctx); err != nil {
		t.Fatal(err)
	}
	if client.token != "jwt-for-"+fake.client.did {
		t.Fatalf("token %q", client.token)
	}

	didLog, err := client.DIDLog(ctx, "did:webvh:QmVtc:dids.example:vtc")
	if err != nil {
		t.Fatal(err)
	}
	version, state, err := LatestLogEntry(didLog)
	if err != nil {
		t.Fatal(err)
	}
	if version != "2-QmTwo" || !strings.Contains(string(state), "TSPTransport") {
		t.Fatalf("head %s %s", version, state)
	}

	newVersion, err := client.UpdateDIDDocument(ctx, "did:webvh:QmVtc:dids.example:vtc", state, version)
	if err != nil {
		t.Fatal(err)
	}
	if newVersion != "3-QmNew" {
		t.Fatalf("new version %q", newVersion)
	}
	if len(fake.updates) != 1 {
		t.Fatalf("%d updates sent", len(fake.updates))
	}
	sent := fake.updates[0]
	if string(sent["did"]) != `"did:webvh:QmVtc:dids.example:vtc"` || string(sent["expectedVersionId"]) != `"2-QmTwo"` {
		t.Fatalf("update payload %v", sent)
	}
	if !json.Valid(sent["document"]) || !strings.Contains(string(sent["document"]), "TSPTransport") {
		t.Fatalf("update document %s", sent["document"])
	}
	if _, has := sent["preRotationCount"]; has {
		t.Fatal("update payload carried a field the caller did not set")
	}
}

func TestARefusalBecomesATaskError(t *testing.T) {
	fake := &fakeVTA{t: t, self: newIdentity(t, 3), client: newIdentity(t, 4), log: testLog,
		refuse: &TaskError{Status: http.StatusUnprocessableEntity, Code: "taskFailed", Reason: ReasonConflict, Message: "expectedVersionId does not match"}}
	client := newTestClient(t, fake)
	ctx := context.Background()
	if err := client.Login(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := client.UpdateDIDDocument(ctx, "did:webvh:QmVtc:dids.example:vtc", json.RawMessage(`{"id":"did:webvh:QmVtc:dids.example:vtc"}`), "1-QmOne")
	var refusal *TaskError
	if !errors.As(err, &refusal) {
		t.Fatalf("error %v is not a TaskError", err)
	}
	if !refusal.Conflict() || refusal.Status != http.StatusUnprocessableEntity || refusal.Code != "taskFailed" {
		t.Fatalf("refusal %+v", refusal)
	}
	if !strings.Contains(err.Error(), "taskFailed (conflict): expectedVersionId does not match") {
		t.Fatalf("message %q", err.Error())
	}
}

func TestAReplyFromAnotherDIDIsRefused(t *testing.T) {
	fake := &fakeVTA{t: t, self: newIdentity(t, 5), client: newIdentity(t, 6), log: testLog}
	client := newTestClient(t, fake)
	// The client expects the VTA to be someone else: every reply now fails
	// verification before its payload is read.
	client.vtaDid = newIdentity(t, 7).did
	fake.self = identity{did: client.vtaDid, kid: fake.self.kid, private: fake.self.private}
	err := client.Login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "proof") {
		t.Fatalf("login error %v", err)
	}
}

func TestWaitUntilHostReachableOutlastsTheMediatorGap(t *testing.T) {
	fake := &fakeVTA{t: t, self: newIdentity(t, 8), client: newIdentity(t, 9), log: testLog, domainsRefusals: 2}
	client := newTestClient(t, fake)
	client.probeInterval = time.Millisecond
	ctx := context.Background()
	if err := client.Login(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.WaitUntilHostReachable(ctx, "dids", time.Second); err != nil {
		t.Fatal(err)
	}
	if fake.domainsProbes != 3 {
		t.Fatalf("%d probes", fake.domainsProbes)
	}

	fake.domainsRefusals = 1 << 20
	err := client.WaitUntilHostReachable(ctx, "dids", 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no transport in common") {
		t.Fatalf("timeout error %v", err)
	}
}

func TestLatestLogEntry(t *testing.T) {
	if _, _, err := LatestLogEntry("\n\n"); err == nil {
		t.Fatal("an empty log parsed")
	}
	if _, _, err := LatestLogEntry(`{"versionId":"1-x"}`); err == nil {
		t.Fatal("an entry without state parsed")
	}
	version, state, err := LatestLogEntry("{\"versionId\":\"1-a\",\"state\":{\"id\":\"did:webvh:a\"}}\n{\"versionId\":\"2-b\",\"state\":{\"id\":\"did:webvh:a\",\"service\":[]}}\n\n")
	if err != nil || version != "2-b" || string(state) != `{"id":"did:webvh:a","service":[]}` {
		t.Fatalf("%s %s %v", version, state, err)
	}
}
