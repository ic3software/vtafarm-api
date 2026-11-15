package setup

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/didresolver"
)

func TestParseMediatorAdminProfile(t *testing.T) {
	profile, key, _ := validMediatorAdminProfile(t)

	got, err := ParseMediatorAdminProfile(profile)
	if err != nil {
		t.Fatalf("ParseMediatorAdminProfile() error = %v", err)
	}
	if got != key {
		t.Fatal("ParseMediatorAdminProfile() returned the wrong key")
	}
}

func TestParseMediatorAdminProfileRejectsInvalidProfilesWithoutLeakingKey(t *testing.T) {
	profile, key, did := validMediatorAdminProfile(t)
	tests := []struct {
		name    string
		profile string
	}{
		{name: "invalid json", profile: `{`},
		{name: "wrong alias", profile: strings.Replace(profile, `"alias":"admin"`, `"alias":"operator"`, 1)},
		{name: "wrong DID", profile: strings.Replace(profile, did, "did:key:z6MkWrong", 1)},
		{name: "missing secret", profile: fmt.Sprintf(`{"alias":"admin","did":%q,"secrets":[]}`, did)},
		{name: "ambiguous secrets", profile: strings.Replace(profile, `"secrets":[`, `"secrets":[{"privateKeyMultibase":"`+key+`"},`, 1)},
		{name: "invalid JWK", profile: strings.Replace(profile, `"crv":"Ed25519"`, `"crv":"X25519"`, 1)},
		{name: "mismatched public key", profile: strings.Replace(profile, `"x":"`, `"x":"AA`, 1)},
		{name: "invalid multibase", profile: fmt.Sprintf(`{"alias":"admin","did":%q,"secrets":[{"privateKeyMultibase":"not-a-private-key"}]}`, did)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMediatorAdminProfile(tt.profile)
			if err == nil {
				t.Fatal("ParseMediatorAdminProfile() error = nil")
			}
			if got != "" {
				t.Fatal("ParseMediatorAdminProfile() returned a key for an invalid profile")
			}
			if strings.Contains(err.Error(), key) {
				t.Fatal("ParseMediatorAdminProfile() leaked the key in its error")
			}
		})
	}
}

func validMediatorAdminProfile(t *testing.T) (profile, privateKey, did string) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	privateKey = "z" + didresolver.EncodeBase58BTC(append([]byte{0x80, 0x26}, seed...))
	did = "did:key:z" + didresolver.EncodeBase58BTC(append([]byte{0xed, 0x01}, public...))
	profile = fmt.Sprintf(
		`{"alias":"admin","did":%q,"mediator":"did:webvh:example","secrets":[{"id":%q,"type":"JsonWebKey2020","privateKeyJwk":{"kty":"OKP","crv":"Ed25519","d":%q,"x":%q}}]}`,
		did,
		did+"#key",
		base64.RawURLEncoding.EncodeToString(seed),
		base64.RawURLEncoding.EncodeToString(public),
	)
	return profile, privateKey, did
}
