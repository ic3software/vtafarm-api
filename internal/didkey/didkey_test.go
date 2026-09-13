package didkey

import (
	"crypto/ed25519"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	did, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if !strings.HasPrefix(did, "did:key:z6Mk") {
		t.Fatalf("Generate() = %q, want an Ed25519 did:key", did)
	}
}

func TestFromPublicKeyRejectsInvalidLength(t *testing.T) {
	if _, err := FromPublicKey(ed25519.PublicKey{1, 2, 3}); err == nil {
		t.Fatal("FromPublicKey() accepted an invalid public key")
	}
}
