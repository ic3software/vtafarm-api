package siop

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"strings"
	"time"
)

const DefaultClockSkew = 60 * time.Second

// VerifiedIDToken contains only identity data that passed key, signature, and
// claim verification.
type VerifiedIDToken struct {
	Subject string
	Kid     string
}

// VerifyIDToken verifies a compact VTA SIOPv2 id_token using the default
// 60-second clock skew. Applications with configured policy should call
// VerifyIDTokenAt and pass their clock and skew explicitly.
func VerifyIDToken(
	ctx context.Context,
	compact string,
	expectedDID string,
	audience string,
	nonce string,
	resolver AuthenticationKeyResolver,
) (VerifiedIDToken, error) {
	return VerifyIDTokenAt(ctx, compact, expectedDID, audience, nonce, resolver, time.Now(), DefaultClockSkew)
}

// VerifyIDTokenAt is the deterministic verification entry point. now and
// clockSkew are explicit policy inputs so tests and handlers do not depend on a
// package-global clock.
func VerifyIDTokenAt(
	ctx context.Context,
	compact string,
	expectedDID string,
	audience string,
	nonce string,
	resolver AuthenticationKeyResolver,
	now time.Time,
	clockSkew time.Duration,
) (VerifiedIDToken, error) {
	if resolver == nil {
		return VerifiedIDToken{}, verificationError(ErrorResolverFailed, "authentication key resolver is not configured", nil)
	}
	if expectedDID == "" || audience == "" || nonce == "" || clockSkew < 0 {
		return VerifiedIDToken{}, verificationError(ErrorInvalidClaims, "verification policy is incomplete", nil)
	}

	token, err := parseCompactToken(compact)
	if err != nil {
		return VerifiedIDToken{}, err
	}

	// This check deliberately precedes resolver work. The value is still
	// untrusted; it is used only to stop callers from turning resolution into an
	// arbitrary DID-fetch proxy.
	if token.claims.Issuer != expectedDID {
		return VerifiedIDToken{}, verificationError(ErrorIssuerMismatch, "issuer does not match the expected DID", nil)
	}
	if !kidBelongsToDID(token.header.KID, token.claims.Issuer) {
		return VerifiedIDToken{}, verificationError(ErrorKIDMismatch, "kid does not belong to issuer or has no fragment", nil)
	}

	publicKey, err := resolver.ResolveAuthenticationKey(ctx, token.claims.Issuer, token.header.KID)
	if err != nil {
		return VerifiedIDToken{}, verificationError(ErrorResolverFailed, "authentication key resolution failed", err)
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return VerifiedIDToken{}, verificationError(ErrorResolverFailed, "resolver returned an invalid Ed25519 key", nil)
	}
	if strings.HasPrefix(token.claims.Issuer, "did:key:") {
		pinned, _, err := ed25519KeyFromDIDKey(token.claims.Issuer)
		if err != nil || subtle.ConstantTimeCompare(pinned, publicKey) != 1 {
			return VerifiedIDToken{}, verificationError(ErrorResolverFailed, "resolved key does not match did:key", err)
		}
	}

	if len(token.signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, []byte(token.signingInput), token.signature) {
		return VerifiedIDToken{}, verificationError(ErrorSignatureInvalid, "Ed25519 signature verification failed", nil)
	}
	if err := token.claims.validate(expectedDID, audience, nonce, now, clockSkew); err != nil {
		return VerifiedIDToken{}, err
	}

	return VerifiedIDToken{Subject: token.claims.Subject, Kid: token.header.KID}, nil
}

func kidBelongsToDID(kid, did string) bool {
	fragmentAt := strings.IndexByte(kid, '#')
	return fragmentAt == len(did) && fragmentAt >= 0 && kid[:fragmentAt] == did && fragmentAt+1 < len(kid)
}
