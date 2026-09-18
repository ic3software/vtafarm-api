package siop

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// AuthenticationKeyResolver returns the exact Ed25519 authentication key
// selected by kid from did's verified DID document.
type AuthenticationKeyResolver interface {
	ResolveAuthenticationKey(ctx context.Context, did, kid string) (ed25519.PublicKey, error)
}

// MethodResolver dispatches supported DID methods without providing a fallback
// for unknown methods.
type MethodResolver struct {
	WebVH AuthenticationKeyResolver
	Peer  AuthenticationKeyResolver
}

func (r MethodResolver) ResolveAuthenticationKey(ctx context.Context, did, kid string) (ed25519.PublicKey, error) {
	switch {
	case strings.HasPrefix(did, "did:key:"):
		return (DIDKeyResolver{}).ResolveAuthenticationKey(ctx, did, kid)
	case strings.HasPrefix(did, "did:webvh:") && r.WebVH != nil:
		return r.WebVH.ResolveAuthenticationKey(ctx, did, kid)
	case strings.HasPrefix(did, "did:peer:") && r.Peer != nil:
		return r.Peer.ResolveAuthenticationKey(ctx, did, kid)
	default:
		return nil, errors.New("unsupported DID method")
	}
}

type didDocument struct {
	ID                 string               `json:"id"`
	VerificationMethod []verificationMethod `json:"verificationMethod"`
	Authentication     []json.RawMessage    `json:"authentication"`
}

type verificationMethod struct {
	ID                 string           `json:"id"`
	Type               string           `json:"type"`
	Controller         string           `json:"controller"`
	PublicKeyMultibase string           `json:"publicKeyMultibase"`
	PublicKeyJWK       *json.RawMessage `json:"publicKeyJwk"`
}

// AuthenticationKeyFromDocument selects kid only when the DID document lists
// it in authentication. The document must already have passed method-specific
// history and proof verification.
func AuthenticationKeyFromDocument(document []byte, did, kid string) (ed25519.PublicKey, error) {
	var parsed didDocument
	if err := json.Unmarshal(document, &parsed); err != nil {
		return nil, fmt.Errorf("decode DID document: %w", err)
	}
	if parsed.ID != did {
		return nil, errors.New("resolved DID document id does not match DID")
	}

	declared := make(map[string]verificationMethod, len(parsed.VerificationMethod))
	for _, method := range parsed.VerificationMethod {
		method.ID = absoluteVerificationMethodID(method.ID, did)
		if method.ID == "" {
			return nil, errors.New("verification method has no id")
		}
		if _, exists := declared[method.ID]; exists {
			return nil, errors.New("DID document has duplicate verification method ids")
		}
		declared[method.ID] = method
	}

	for _, raw := range parsed.Authentication {
		var reference string
		if err := json.Unmarshal(raw, &reference); err == nil {
			if absoluteVerificationMethodID(reference, did) != kid {
				continue
			}
			method, ok := declared[kid]
			if !ok {
				return nil, errors.New("authentication references an undeclared verification method")
			}
			return ed25519KeyFromVerificationMethod(method, did)
		}

		var embedded verificationMethod
		if err := json.Unmarshal(raw, &embedded); err != nil {
			return nil, errors.New("authentication entry is neither a reference nor a verification method")
		}
		embedded.ID = absoluteVerificationMethodID(embedded.ID, did)
		if embedded.ID == kid {
			return ed25519KeyFromVerificationMethod(embedded, did)
		}
	}
	return nil, errors.New("kid is not an authentication verification method")
}

func absoluteVerificationMethodID(id, did string) string {
	if strings.HasPrefix(id, "#") {
		return did + id
	}
	return id
}

func ed25519KeyFromVerificationMethod(method verificationMethod, did string) (ed25519.PublicKey, error) {
	if method.Controller != did {
		return nil, errors.New("verification method controller does not match DID")
	}
	hasMultibase := method.PublicKeyMultibase != ""
	hasJWK := method.PublicKeyJWK != nil
	if hasMultibase == hasJWK {
		return nil, errors.New("verification method must contain exactly one supported key representation")
	}
	if hasMultibase {
		if method.Type != "Multikey" && method.Type != "Ed25519VerificationKey2020" {
			return nil, errors.New("verification method type is not compatible with an Ed25519 multikey")
		}
		return decodeEd25519Multikey(method.PublicKeyMultibase)
	}
	if method.Type != "JsonWebKey2020" {
		return nil, errors.New("verification method type is not JsonWebKey2020")
	}

	var jwk struct {
		KeyType string `json:"kty"`
		Curve   string `json:"crv"`
		X       string `json:"x"`
		D       string `json:"d"`
	}
	if err := json.Unmarshal(*method.PublicKeyJWK, &jwk); err != nil {
		return nil, fmt.Errorf("decode public key JWK: %w", err)
	}
	if jwk.KeyType != "OKP" || jwk.Curve != "Ed25519" || jwk.X == "" || jwk.D != "" {
		return nil, errors.New("JWK is not an Ed25519 public key")
	}
	decoded, err := decodeCanonicalBase64URL(jwk.X)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("JWK x is not a canonical 32-byte Ed25519 key")
	}
	return ed25519.PublicKey(decoded), nil
}
