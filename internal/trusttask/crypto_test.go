package trusttask

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/didresolver"
)

func farmDocument(t *testing.T, signer *Signer, endpoint string) []byte {
	t.Helper()
	method := signer.VerificationMethod()
	multikey := method[strings.LastIndex(method, "#")+1:]
	document, err := json.Marshal(map[string]any{
		"id": signer.did,
		"verificationMethod": []map[string]any{{
			"id": method, "type": "Multikey", "controller": signer.did, "publicKeyMultibase": multikey,
		}},
		"assertionMethod": []string{method},
		"service": []map[string]any{{
			"id": signer.did + "#trust-tasks", "type": "TrustTaskHTTPS", "serviceEndpoint": endpoint,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestValidateFarmDIDDocument(t *testing.T) {
	did := "did:webvh:QmFarm:example.com:vtafarm-auth"
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	signer, err := NewSigner(did, key)
	if err != nil {
		t.Fatal(err)
	}
	document := farmDocument(t, signer, "https://api.example.com/trust-task/")
	if err := ValidateFarmDIDDocument(document, signer, "https://api.example.com/trust-task"); err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(document, &parsed); err != nil {
		t.Fatal(err)
	}
	delete(parsed, "service")
	withoutService, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateFarmDIDDocument(withoutService, signer, "https://api.example.com/trust-task"); err == nil {
		t.Fatal("accepted Farm DID document without TrustTaskHTTPS")
	}

	otherKey := make([]byte, 32)
	otherKey[len(otherKey)-1] = 1
	otherSigner, err := NewSigner(did, base64.StdEncoding.EncodeToString(otherKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateFarmDIDDocument(document, otherSigner, "https://api.example.com/trust-task"); err == nil {
		t.Fatal("accepted FARM_SIGNING_KEY absent from assertionMethod")
	}
}

func TestSignerProducesVerifiableEddsaJCSProof(t *testing.T) {
	did := "did:webvh:QmFarm:example.com:vtafarm-auth"
	signer, err := NewSigner(did, base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.SignRaw(json.RawMessage(`{"id":"reply-1","type":"https://example.com/reply","issuer":"` + did + `","payload":{"ok":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(signed, &body); err != nil {
		t.Fatal(err)
	}
	proof, ok := body["proof"].(map[string]any)
	if !ok {
		t.Fatal("signed document has no proof object")
	}
	proofValue, ok := proof["proofValue"].(string)
	if !ok || !strings.HasPrefix(proofValue, "z") {
		t.Fatal("signed document has no base58btc proofValue")
	}
	signature, err := didresolver.DecodeBase58BTC(strings.TrimPrefix(proofValue, "z"))
	if err != nil {
		t.Fatal(err)
	}
	delete(proof, "proofValue")
	delete(body, "proof")
	proofCanonical, err := canonicalJSON(proof)
	if err != nil {
		t.Fatal(err)
	}
	bodyCanonical, err := canonicalJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	proofHash, bodyHash := sha256.Sum256(proofCanonical), sha256.Sum256(bodyCanonical)
	message := append(proofHash[:], bodyHash[:]...)
	if !ed25519.Verify(signer.PublicKey(), message, signature) {
		t.Fatal("Farm signature does not verify")
	}
}
