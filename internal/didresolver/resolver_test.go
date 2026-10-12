package didresolver

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"testing"
)

func testKey(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	publicKey := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	encoded := append([]byte{0xed, 0x01}, publicKey...)
	return publicKey, "z" + EncodeBase58BTC(encoded)
}

func TestDIDKeyResolverRejectsNonCanonicalKidAndCodec(t *testing.T) {
	publicKey, multibase := testKey(t)
	did := "did:key:" + multibase
	resolved, err := (DIDKeyResolver{}).ResolveAuthenticationKey(context.Background(), did, did+"#"+multibase)
	if err != nil {
		t.Fatalf("ResolveAuthenticationKey() error = %v", err)
	}
	if !resolved.Equal(publicKey) {
		t.Fatal("resolved key does not match")
	}
	if _, err := (DIDKeyResolver{}).ResolveAuthenticationKey(context.Background(), did, did+"#other"); err == nil {
		t.Fatal("non-canonical kid was accepted")
	}

	x25519 := append([]byte{0xec, 0x01}, make([]byte, ed25519.PublicKeySize)...)
	x25519DID := "did:key:z" + EncodeBase58BTC(x25519)
	if _, err := (DIDKeyResolver{}).ResolveAuthenticationKey(
		context.Background(), x25519DID, x25519DID+"#"+x25519DID[len("did:key:"):],
	); err == nil {
		t.Fatal("X25519 did:key was accepted")
	}
}

func TestAuthenticationKeyFromDocument(t *testing.T) {
	publicKey, multibase := testKey(t)
	did := "did:webvh:QmScid:persona.example"
	kid := did + "#key-0"
	document := []byte(fmt.Sprintf(`{
		"id":%q,
		"verificationMethod":[
			{"id":"#key-0","type":"Multikey","controller":%q,"publicKeyMultibase":%q},
			{"id":"#assertion-only","type":"Multikey","controller":%q,"publicKeyMultibase":%q}
		],
		"authentication":["#key-0"]
	}`, did, did, multibase, did, multibase))

	resolved, err := AuthenticationKeyFromDocument(document, did, kid)
	if err != nil {
		t.Fatalf("AuthenticationKeyFromDocument() error = %v", err)
	}
	if !resolved.Equal(publicKey) {
		t.Fatal("resolved key does not match")
	}
	if _, err := AuthenticationKeyFromDocument(document, did, did+"#assertion-only"); err == nil {
		t.Fatal("key outside authentication was accepted")
	}
}

func TestAuthenticationKeyFromDocumentAcceptsEd25519JWK(t *testing.T) {
	publicKey, _ := testKey(t)
	did := "did:webvh:QmScid:persona.example"
	kid := did + "#key-0"
	x := base64.RawURLEncoding.EncodeToString(publicKey)
	document := []byte(fmt.Sprintf(`{
		"id":%q,
		"verificationMethod":[{"id":"#key-0","type":"JsonWebKey2020","controller":%q,"publicKeyJwk":{"kty":"OKP","crv":"Ed25519","x":%q}}],
		"authentication":["#key-0"]
	}`, did, did, x))

	resolved, err := AuthenticationKeyFromDocument(document, did, kid)
	if err != nil {
		t.Fatalf("AuthenticationKeyFromDocument() error = %v", err)
	}
	if !resolved.Equal(publicKey) {
		t.Fatal("resolved JWK does not match")
	}
}
