package siop

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	projectdidkey "github.com/ic3software/vtafarm-api/internal/didkey"
)

const (
	testAudience = "did:webvh:QmSiteScid:rp.example"
	testNonce    = "challenge-value"
)

var testNow = time.Unix(1_700_000_000, 0)

type testIdentity struct {
	did        string
	kid        string
	privateKey ed25519.PrivateKey
}

func newTestIdentity(t *testing.T, seed byte) testIdentity {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed(bytesOf(seed, ed25519.SeedSize))
	did, err := projectdidkey.FromPublicKey(privateKey.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	multibase := did[len("did:key:"):]
	return testIdentity{did: did, kid: did + "#" + multibase, privateKey: privateKey}
}

func bytesOf(value byte, length int) []byte {
	result := make([]byte, length)
	for i := range result {
		result[i] = value
	}
	return result
}

func mintToken(t *testing.T, signer testIdentity, header, payload map[string]any) string {
	t.Helper()
	if header == nil {
		header = map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": signer.kid}
	}
	if payload == nil {
		payload = map[string]any{
			"iss": signer.did, "sub": signer.did, "aud": testAudience,
			"nonce": testNonce, "iat": testNow.Unix(), "exp": testNow.Add(5 * time.Minute).Unix(),
		}
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	headerSegment := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadSegment := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := headerSegment + "." + payloadSegment
	signature := ed25519.Sign(signer.privateKey, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func verifyForTest(token, expectedDID string, resolver AuthenticationKeyResolver) (VerifiedIDToken, error) {
	return VerifyIDTokenAt(
		context.Background(), token, expectedDID, testAudience, testNonce,
		resolver, testNow, DefaultClockSkew,
	)
}

func TestVerifyIDTokenAcceptsRustCompatibleDIDKeyToken(t *testing.T) {
	identity := newTestIdentity(t, 42)
	verified, err := verifyForTest(mintToken(t, identity, nil, nil), identity.did, DIDKeyResolver{})
	if err != nil {
		t.Fatalf("VerifyIDTokenAt() error = %v", err)
	}
	if verified.Subject != identity.did || verified.Kid != identity.kid {
		t.Fatalf("VerifyIDTokenAt() = %#v", verified)
	}
}

func TestVerifyIDTokenRejectsSecurityFailures(t *testing.T) {
	identity := newTestIdentity(t, 42)
	other := newTestIdentity(t, 7)
	basePayload := func() map[string]any {
		return map[string]any{
			"iss": identity.did, "sub": identity.did, "aud": testAudience,
			"nonce": testNonce, "iat": testNow.Unix(), "exp": testNow.Add(5 * time.Minute).Unix(),
		}
	}

	tests := []struct {
		name     string
		token    func() string
		expected ErrorCode
	}{
		{
			name: "alg none",
			token: func() string {
				return mintToken(t, identity, map[string]any{"alg": "none", "kid": identity.kid}, nil)
			},
			expected: ErrorUnsupportedAlgorithm,
		},
		{
			name: "missing kid",
			token: func() string {
				return mintToken(t, identity, map[string]any{"alg": "EdDSA", "typ": "JWT"}, nil)
			},
			expected: ErrorInvalidHeader,
		},
		{
			name: "unknown protected header",
			token: func() string {
				return mintToken(t, identity, map[string]any{"alg": "EdDSA", "kid": identity.kid, "crit": []string{"x"}, "x": true}, nil)
			},
			expected: ErrorInvalidHeader,
		},
		{
			name: "issuer and subject differ",
			token: func() string {
				payload := basePayload()
				payload["sub"] = other.did
				return mintToken(t, identity, nil, payload)
			},
			expected: ErrorSubjectMismatch,
		},
		{
			name: "wrong audience",
			token: func() string {
				payload := basePayload()
				payload["aud"] = "did:webvh:other"
				return mintToken(t, identity, nil, payload)
			},
			expected: ErrorAudienceMismatch,
		},
		{
			name: "wrong nonce",
			token: func() string {
				payload := basePayload()
				payload["nonce"] = "wrong"
				return mintToken(t, identity, nil, payload)
			},
			expected: ErrorNonceMismatch,
		},
		{
			name: "expired",
			token: func() string {
				payload := basePayload()
				payload["iat"] = testNow.Add(-10 * time.Minute).Unix()
				payload["exp"] = testNow.Add(-2 * time.Minute).Unix()
				return mintToken(t, identity, nil, payload)
			},
			expected: ErrorTokenExpired,
		},
		{
			name: "iat in future",
			token: func() string {
				payload := basePayload()
				payload["iat"] = testNow.Add(2 * time.Minute).Unix()
				return mintToken(t, identity, nil, payload)
			},
			expected: ErrorTokenNotYetValid,
		},
		{
			name: "kid from another DID",
			token: func() string {
				return mintToken(t, identity, map[string]any{"alg": "EdDSA", "kid": other.kid}, nil)
			},
			expected: ErrorKIDMismatch,
		},
		{
			name: "altered signature",
			token: func() string {
				token := mintToken(t, identity, nil, nil)
				last := token[len(token)-1]
				replacement := byte('A')
				if last == replacement {
					replacement = 'B'
				}
				return token[:len(token)-1] + string(replacement)
			},
			expected: ErrorSignatureInvalid,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := verifyForTest(test.token(), identity.did, DIDKeyResolver{})
			if ErrorCodeOf(err) != test.expected {
				t.Fatalf("error = %v, code = %q, want %q", err, ErrorCodeOf(err), test.expected)
			}
		})
	}
}

func TestVerifyIDTokenRejectsMalformedCompactJWS(t *testing.T) {
	identity := newTestIdentity(t, 42)
	valid := mintToken(t, identity, nil, nil)
	parts := splitToken(t, valid)

	tests := map[string]string{
		"two segments":        "a.b",
		"empty segment":       "a..b",
		"padded header":       parts[0] + "=." + parts[1] + "." + parts[2],
		"duplicate alg":       rawSignedToken(t, identity, `{"alg":"EdDSA","alg":"none","kid":"`+identity.kid+`"}`, defaultPayloadJSON(identity)),
		"duplicate issuer":    rawSignedToken(t, identity, defaultHeaderJSON(identity), `{"iss":"`+identity.did+`","iss":"`+identity.did+`","sub":"`+identity.did+`","aud":"`+testAudience+`","nonce":"`+testNonce+`","iat":1700000000,"exp":1700000300}`),
		"fractional issuedAt": rawSignedToken(t, identity, defaultHeaderJSON(identity), `{"iss":"`+identity.did+`","sub":"`+identity.did+`","aud":"`+testAudience+`","nonce":"`+testNonce+`","iat":1700000000.5,"exp":1700000300}`),
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := verifyForTest(token, identity.did, DIDKeyResolver{}); err == nil {
				t.Fatal("malformed token was accepted")
			}
		})
	}
}

func TestIssuerMismatchDoesNotCallResolver(t *testing.T) {
	identity := newTestIdentity(t, 42)
	resolver := &countingResolver{}
	_, err := verifyForTest(mintToken(t, identity, nil, nil), "did:key:z6MkExpected", resolver)
	if ErrorCodeOf(err) != ErrorIssuerMismatch {
		t.Fatalf("error = %v", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolver.calls)
	}
}

type countingResolver struct{ calls int }

func (r *countingResolver) ResolveAuthenticationKey(context.Context, string, string) (ed25519.PublicKey, error) {
	r.calls++
	return nil, errors.New("unexpected call")
}

func TestDIDKeyResolverRejectsNonCanonicalKidAndCodec(t *testing.T) {
	identity := newTestIdentity(t, 42)
	resolver := DIDKeyResolver{}
	if _, err := resolver.ResolveAuthenticationKey(context.Background(), identity.did, identity.did+"#other"); err == nil {
		t.Fatal("non-canonical kid was accepted")
	}

	x25519 := append([]byte{0xec, 0x01}, bytesOf(1, ed25519.PublicKeySize)...)
	did := "did:key:z" + EncodeBase58BTC(x25519)
	if _, err := resolver.ResolveAuthenticationKey(context.Background(), did, did+"#"+did[len("did:key:"):]); err == nil {
		t.Fatal("X25519 did:key was accepted")
	}
}

func TestAuthenticationKeyFromDocument(t *testing.T) {
	identity := newTestIdentity(t, 42)
	multibase := identity.did[len("did:key:"):]
	did := "did:webvh:QmScid:persona.example"
	kid := did + "#key-0"
	document := []byte(fmt.Sprintf(`{
		"@context":["https://www.w3.org/ns/did/v1"],
		"id":%q,
		"verificationMethod":[
			{"id":"#key-0","type":"Multikey","controller":%q,"publicKeyMultibase":%q},
			{"id":"#assertion-only","type":"Multikey","controller":%q,"publicKeyMultibase":%q}
		],
		"authentication":["#key-0"]
	}`, did, did, multibase, did, multibase))

	key, err := AuthenticationKeyFromDocument(document, did, kid)
	if err != nil {
		t.Fatalf("AuthenticationKeyFromDocument() error = %v", err)
	}
	if !key.Equal(identity.privateKey.Public()) {
		t.Fatal("resolved key does not match")
	}
	if _, err := AuthenticationKeyFromDocument(document, did, did+"#assertion-only"); err == nil {
		t.Fatal("key outside authentication was accepted")
	}
}

func TestAuthenticationKeyFromDocumentAcceptsEd25519JWK(t *testing.T) {
	identity := newTestIdentity(t, 42)
	did := "did:webvh:QmScid:persona.example"
	kid := did + "#key-0"
	x := base64.RawURLEncoding.EncodeToString(identity.privateKey.Public().(ed25519.PublicKey))
	document := []byte(fmt.Sprintf(`{
		"id":%q,
		"verificationMethod":[{"id":"#key-0","type":"JsonWebKey2020","controller":%q,"publicKeyJwk":{"kty":"OKP","crv":"Ed25519","x":%q}}],
		"authentication":["#key-0"]
	}`, did, did, x))
	key, err := AuthenticationKeyFromDocument(document, did, kid)
	if err != nil {
		t.Fatalf("AuthenticationKeyFromDocument() error = %v", err)
	}
	if !key.Equal(identity.privateKey.Public()) {
		t.Fatal("resolved JWK does not match")
	}
}

func splitToken(t *testing.T, token string) [3]string {
	t.Helper()
	var parts [3]string
	count := 0
	start := 0
	for i := 0; i <= len(token); i++ {
		if i == len(token) || token[i] == '.' {
			if count >= len(parts) {
				t.Fatal("invalid test token")
			}
			parts[count] = token[start:i]
			count++
			start = i + 1
		}
	}
	if count != 3 {
		t.Fatal("invalid test token")
	}
	return parts
}

func rawSignedToken(t *testing.T, identity testIdentity, headerJSON, payloadJSON string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(headerJSON))
	payload := base64.RawURLEncoding.EncodeToString([]byte(payloadJSON))
	input := header + "." + payload
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(identity.privateKey, []byte(input)))
}

func defaultHeaderJSON(identity testIdentity) string {
	return fmt.Sprintf(`{"alg":"EdDSA","typ":"JWT","kid":%q}`, identity.kid)
}

func defaultPayloadJSON(identity testIdentity) string {
	return fmt.Sprintf(`{"iss":%q,"sub":%q,"aud":%q,"nonce":%q,"iat":1700000000,"exp":1700000300}`,
		identity.did, identity.did, testAudience, testNonce)
}
