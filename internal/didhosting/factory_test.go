package didhosting

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/didkey"
)

func testKeypair(t *testing.T, fill byte) (did, privKeyB64 string) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = fill
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	did, err := didkey.FromPublicKey(privateKey.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return did, base64.StdEncoding.EncodeToString(seed)
}

func TestForUsesRequiredServiceDID(t *testing.T) {
	did, key := testKeypair(t, 1)
	c, err := NewFactory(did, key).For("https://dids.example", "did:webvh:dids.example")
	if err != nil {
		t.Fatal(err)
	}
	if c.ServerDid() != "did:webvh:dids.example" {
		t.Fatalf("ServerDid = %q", c.ServerDid())
	}
}

func TestForRefusesMismatchedCachedServiceDID(t *testing.T) {
	did, key := testKeypair(t, 2)
	factory := NewFactory(did, key)
	if _, err := factory.For("https://dids.example", "did:webvh:dids.example"); err != nil {
		t.Fatal(err)
	}
	_, err := factory.For("https://dids.example", "did:webvh:other.example")
	if err == nil || !strings.Contains(err.Error(), "requested") {
		t.Fatalf("expected a service DID mismatch, got %v", err)
	}
}

func TestForRejectsMissingServiceDID(t *testing.T) {
	did, key := testKeypair(t, 3)
	if _, err := NewFactory(did, key).For("https://dids.example", ""); err == nil {
		t.Fatal("missing service DID was accepted")
	}
}

func TestNilFactoryFails(t *testing.T) {
	var factory *Factory
	if _, err := factory.For("https://dids.example", "did:webvh:dids.example"); err == nil {
		t.Fatal("expected an error from a nil factory")
	}
}

func TestForRejectsEmptyURL(t *testing.T) {
	did, key := testKeypair(t, 4)
	if _, err := NewFactory(did, key).For("", "did:webvh:dids.example"); err == nil {
		t.Fatal("empty URL was accepted")
	}
}
