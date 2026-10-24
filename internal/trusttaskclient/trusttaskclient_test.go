package trusttaskclient

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/didresolver"
)

func keyPair(t *testing.T, fill byte) (did, kid string, private ed25519.PrivateKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = fill
	}
	private = ed25519.NewKeyFromSeed(seed)
	did, err := didkey.FromPublicKey(private.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return did, did + "#" + strings.TrimPrefix(did, "did:key:"), private
}

func TestSignedReplyRoundTrip(t *testing.T) {
	clientDid, _, _ := keyPair(t, 1)
	serviceDid, serviceKid, servicePrivate := keyPair(t, 2)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	created := now.Format(time.RFC3339)

	request := Document{ID: "urn:uuid:req", Type: "https://example.org/task/0.1", Issuer: clientDid, Recipient: serviceDid, IssuedAt: created, Payload: json.RawMessage(`{"a":1}`)}
	reply := Document{ID: "urn:uuid:rep", Type: request.Type + "#response", ThreadID: request.ID, Issuer: serviceDid, Recipient: clientDid, IssuedAt: created, Payload: json.RawMessage(`{"ok":true}`)}
	if err := Sign(&reply, serviceKid, servicePrivate, created); err != nil {
		t.Fatal(err)
	}
	if reply.Proof.ProofPurpose != "authentication" || reply.Proof.Cryptosuite != "eddsa-jcs-2022" || !strings.HasPrefix(reply.Proof.ProofValue, "z") {
		t.Fatalf("proof %+v", reply.Proof)
	}

	// The proof survives a trip through JSON, which is how it is received.
	encoded, _ := json.Marshal(reply)
	var received Document
	if err := json.Unmarshal(encoded, &received); err != nil {
		t.Fatal(err)
	}
	expect := Reply{Service: serviceDid, Client: clientDid, Now: func() time.Time { return now }}
	if err := VerifyReply(context.Background(), didresolver.DIDKeyResolver{}, &received, &request, expect); err != nil {
		t.Fatal(err)
	}
	if received.Proof == nil {
		t.Fatal("verification stripped the proof")
	}

	// One byte of payload changed after signing fails, and so does a reply
	// threaded to another request, addressed elsewhere, or stale.
	tampered := received
	tampered.Payload = json.RawMessage(`{"ok":false}`)
	if err := VerifyReply(context.Background(), didresolver.DIDKeyResolver{}, &tampered, &request, expect); err == nil || !strings.Contains(err.Error(), "signature is invalid") {
		t.Fatalf("tampered payload: %v", err)
	}
	other := request
	other.ID = "urn:uuid:other"
	if err := VerifyReply(context.Background(), didresolver.DIDKeyResolver{}, &received, &other, expect); err == nil || !strings.Contains(err.Error(), "threaded") {
		t.Fatalf("wrong thread: %v", err)
	}
	if err := VerifyReply(context.Background(), didresolver.DIDKeyResolver{}, &received, &request, Reply{Service: serviceDid, Client: "did:key:z6MkSomeoneElse", Now: expect.Now}); err == nil || !strings.Contains(err.Error(), "recipient") {
		t.Fatalf("wrong recipient: %v", err)
	}
	late := Reply{Service: serviceDid, Client: clientDid, Now: func() time.Time { return now.Add(6 * time.Minute) }}
	if err := VerifyReply(context.Background(), didresolver.DIDKeyResolver{}, &received, &request, late); err == nil || !strings.Contains(err.Error(), "freshness") {
		t.Fatalf("stale reply: %v", err)
	}
}

func TestDecodeProofValueWantsCanonicalMultibase(t *testing.T) {
	if _, err := DecodeProofValue("abc"); err == nil {
		t.Fatal("a value without the z prefix decoded")
	}
	if _, err := DecodeProofValue("z0OIl"); err == nil {
		t.Fatal("non-alphabet characters decoded")
	}
	signature := make([]byte, ed25519.SignatureSize)
	signature[0] = 7
	encoded := "z" + didresolver.EncodeBase58BTC(signature)
	decoded, err := DecodeProofValue(encoded)
	if err != nil || len(decoded) != ed25519.SignatureSize || decoded[0] != 7 {
		t.Fatalf("round trip %v %d", err, len(decoded))
	}
}
