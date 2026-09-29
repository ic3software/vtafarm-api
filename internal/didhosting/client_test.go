package didhosting

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/siop"
)

type testIdentity struct {
	did     string
	kid     string
	seedB64 string
	private ed25519.PrivateKey
}

func identity(t *testing.T, fill byte) testIdentity {
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
	multibase := did[len("did:key:"):]
	return testIdentity{
		did:     did,
		kid:     did + "#" + multibase,
		seedB64: base64.StdEncoding.EncodeToString(seed),
		private: private,
	}
}

func verifyTask(t *testing.T, doc *trustTask, signer testIdentity) {
	t.Helper()
	if doc.Proof == nil {
		t.Fatal("request has no proof")
	}
	p := *doc.Proof
	signature, err := decodeProofValue(p.ProofValue)
	if err != nil {
		t.Fatal(err)
	}
	doc.Proof = nil
	message, err := proofMessage(doc, p)
	doc.Proof = &p
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := (siop.DIDKeyResolver{}).ResolveAuthenticationKey(context.Background(), signer.did, signer.kid)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(publicKey, message, signature) {
		t.Fatal("request proof did not verify")
	}
}

func TestRegisterDidUsesSignedTrustTask(t *testing.T) {
	clientIdentity := identity(t, 1)
	serverIdentity := identity(t, 2)
	var received trustTask

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != trustTaskEndpoint || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("legacy bearer authorization was sent: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		verifyTask(t, &received, clientIdentity)

		payload, _ := json.Marshal(map[string]any{"record": map[string]any{"mnemonic": "agent-vta"}})
		reply := trustTask{
			ID:        "reply-id",
			Type:      received.Type + "#response",
			ThreadID:  received.ID,
			Issuer:    serverIdentity.did,
			Recipient: clientIdentity.did,
			IssuedAt:  time.Now().UTC().Format(time.RFC3339),
			Payload:   payload,
		}
		if err := signTrustTask(&reply, serverIdentity.kid, serverIdentity.private, reply.IssuedAt); err != nil {
			t.Errorf("sign response: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	defer server.Close()

	client, err := New(server.URL, clientIdentity.did, clientIdentity.seedB64, serverIdentity.did)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RegisterDid(context.Background(), "agent-vta", "did log"); err != nil {
		t.Fatalf("RegisterDid: %v", err)
	}
	if received.Type != didRegisterType || received.Issuer != clientIdentity.did || received.Recipient != serverIdentity.did {
		t.Fatalf("unexpected Trust Task envelope: %+v", received)
	}
	var payload map[string]any
	if err := json.Unmarshal(received.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["path"] != "agent-vta" || payload["method"] != "webvh" || payload["didData"] != "did log" || payload["force"] != false {
		t.Fatalf("unexpected register payload: %#v", payload)
	}
}

func TestRegisterDidRejectsUnsignedResponse(t *testing.T) {
	clientIdentity := identity(t, 3)
	serverIdentity := identity(t, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request trustTask
		_ = json.NewDecoder(r.Body).Decode(&request)
		_ = json.NewEncoder(w).Encode(trustTask{
			ID:        "reply-id",
			Type:      request.Type + "#response",
			ThreadID:  request.ID,
			Issuer:    serverIdentity.did,
			Recipient: clientIdentity.did,
			Payload:   json.RawMessage(`{}`),
		})
	}))
	defer server.Close()

	client, err := New(server.URL, clientIdentity.did, clientIdentity.seedB64, serverIdentity.did)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RegisterDid(context.Background(), "agent-vta", "did log"); err == nil {
		t.Fatal("unsigned response was accepted")
	}
}
