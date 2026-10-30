package trusttask

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/ic3software/vtafarm-api/internal/didresolver"
	"github.com/santhosh-tekuri/jsonschema/v6"
	prooflib "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/proof"
	"github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/trusttasks"
)

type Signer struct {
	did                string
	verificationMethod string
	privateKey         ed25519.PrivateKey
}

func NewSigner(did, encodedSeed string) (*Signer, error) {
	did = strings.TrimSpace(did)
	if !strings.HasPrefix(did, "did:webvh:") || strings.ContainsAny(did, "# \t\r\n") {
		return nil, errors.New("FARM_DID must be a did:webvh DID without a fragment")
	}
	seed, err := decodeSeed(strings.TrimSpace(encodedSeed))
	if err != nil {
		return nil, err
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	multikey := "z" + didresolver.EncodeBase58BTC(append([]byte{0xed, 0x01}, publicKey...))
	return &Signer{
		did:                did,
		verificationMethod: did + "#" + multikey,
		privateKey:         privateKey,
	}, nil
}

func (s *Signer) VerificationMethod() string {
	if s == nil {
		return ""
	}
	return s.verificationMethod
}

func (s *Signer) PublicKey() ed25519.PublicKey {
	if s == nil {
		return nil
	}
	key := s.privateKey.Public().(ed25519.PublicKey)
	return slices.Clone(key)
}

// ValidateFarmDIDDocument proves that the configured signing key and HTTPS
// binding base are both advertised by the resolved Farm DID document.
func ValidateFarmDIDDocument(document []byte, signer *Signer, trustTaskBase string) error {
	if signer == nil {
		return errors.New("Farm Trust Task signer is not configured")
	}
	base, err := url.Parse(strings.TrimSpace(trustTaskBase))
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.RawPath != "" || strings.TrimSuffix(base.Path, "/") != "/trust-task" {
		return errors.New("TRUST_TASK_BASE must be an HTTPS URL ending in /trust-task without credentials, query, or fragment")
	}
	var parsed struct {
		ID      string `json:"id"`
		Service []struct {
			Type            json.RawMessage `json:"type"`
			ServiceEndpoint json.RawMessage `json:"serviceEndpoint"`
		} `json:"service"`
	}
	if err := json.Unmarshal(document, &parsed); err != nil {
		return fmt.Errorf("decode Farm DID document: %w", err)
	}
	if parsed.ID != signer.did {
		return errors.New("Farm DID document id does not match FARM_DID")
	}
	key, err := didresolver.AssertionMethodKeyFromDocument(document, signer.did, signer.verificationMethod)
	if err != nil {
		return fmt.Errorf("Farm assertionMethod: %w", err)
	}
	if !key.Equal(signer.PublicKey()) {
		return errors.New("FARM_SIGNING_KEY does not match the published assertionMethod")
	}

	wantedBase := strings.TrimSuffix(base.String(), "/")
	for _, service := range parsed.Service {
		if !serviceTypeContains(service.Type, "TrustTaskHTTPS") {
			continue
		}
		var endpoint string
		if json.Unmarshal(service.ServiceEndpoint, &endpoint) == nil && strings.TrimSuffix(endpoint, "/") == wantedBase {
			return nil
		}
	}
	return errors.New("Farm DID document does not advertise the configured TrustTaskHTTPS base")
}

func ValidateSignInPortal(document []byte, farmDID, portalURL string) error {
	portal, err := url.Parse(strings.TrimSpace(portalURL))
	if err != nil || portal.Scheme != "https" || portal.Host == "" || portal.User != nil || portal.RawQuery != "" || portal.Fragment != "" || portal.RawPath != "" {
		return errors.New("SIGN_IN_PORTAL_URL must be an HTTPS URL without credentials, query, or fragment")
	}
	var parsed struct {
		ID      string `json:"id"`
		Service []struct {
			Type            json.RawMessage `json:"type"`
			ServiceEndpoint json.RawMessage `json:"serviceEndpoint"`
		} `json:"service"`
	}
	if err := json.Unmarshal(document, &parsed); err != nil {
		return fmt.Errorf("decode Farm DID document: %w", err)
	}
	if parsed.ID != farmDID {
		return errors.New("Farm DID document id does not match FARM_DID")
	}
	wanted := portal.String()
	for _, service := range parsed.Service {
		if !serviceTypeContains(service.Type, "SignInPortal") {
			continue
		}
		var endpoint string
		if json.Unmarshal(service.ServiceEndpoint, &endpoint) == nil && endpoint == wanted {
			return nil
		}
	}
	return errors.New("Farm DID document does not advertise the configured SignInPortal")
}

func serviceTypeContains(raw json.RawMessage, expected string) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == expected
	}
	var multiple []string
	return json.Unmarshal(raw, &multiple) == nil && slices.Contains(multiple, expected)
}

func decodeSeed(value string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding.Strict(), base64.RawStdEncoding.Strict(), base64.RawURLEncoding.Strict()} {
		decoded, err := encoding.DecodeString(value)
		if err == nil && len(decoded) == ed25519.SeedSize {
			return decoded, nil
		}
	}
	return nil, errors.New("FARM_SIGNING_KEY must be a base64-encoded 32-byte Ed25519 seed")
}

func (s *Signer) SignRaw(document json.RawMessage) (json.RawMessage, error) {
	if s == nil {
		return nil, errors.New("Farm Trust Task signer is not configured")
	}
	var body map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(document)))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	issuer, ok := body["issuer"].(string)
	if !ok || issuer != s.did {
		return nil, errors.New("Trust Task reply issuer does not match FARM_DID")
	}
	delete(body, "proof")
	proof := map[string]any{
		"type":               "DataIntegrityProof",
		"cryptosuite":        "eddsa-jcs-2022",
		"created":            time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		"verificationMethod": s.verificationMethod,
		"proofPurpose":       "assertionMethod",
	}
	proofCanonical, err := canonicalJSON(proof)
	if err != nil {
		return nil, fmt.Errorf("canonicalize proof: %w", err)
	}
	bodyCanonical, err := canonicalJSON(body)
	if err != nil {
		return nil, fmt.Errorf("canonicalize reply: %w", err)
	}
	proofHash := sha256.Sum256(proofCanonical)
	bodyHash := sha256.Sum256(bodyCanonical)
	message := append(proofHash[:], bodyHash[:]...)
	proof["proofValue"] = "z" + didresolver.EncodeBase58BTC(ed25519.Sign(s.privateKey, message))
	body["proof"] = proof
	return json.Marshal(body)
}

func SignDocument[P any](signer *Signer, document *trusttasks.Document[P]) error {
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	signed, err := signer.SignRaw(encoded)
	if err != nil {
		return err
	}
	return json.Unmarshal(signed, document)
}

func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return jsoncanonicalizer.Transform(encoded)
}

type WalletVerifier struct {
	inner *prooflib.Verifier
}

func (v *WalletVerifier) VerifyDIDKey(ctx context.Context, document json.RawMessage, issuer, purpose string) error {
	var envelope struct {
		Issuer *string           `json:"issuer"`
		Proof  *trusttasks.Proof `json:"proof"`
	}
	if err := json.Unmarshal(document, &envelope); err != nil {
		return err
	}
	if envelope.Issuer == nil || *envelope.Issuer != issuer || !strings.HasPrefix(issuer, "did:key:z6Mk") || envelope.Proof == nil {
		return errors.New("proof issuer is not the expected Ed25519 did:key")
	}
	multikey := strings.TrimPrefix(issuer, "did:key:")
	if envelope.Proof.ProofPurpose != purpose || envelope.Proof.VerificationMethod != issuer+"#"+multikey {
		return errors.New("proof is not authorized for the required DID relationship")
	}
	return v.inner.Verify(ctx, document)
}

func NewWalletVerifier() *WalletVerifier {
	return &WalletVerifier{inner: prooflib.NewVerifier(prooflib.WithCryptosuites(prooflib.EddsaJcs2022))}
}

func (v *WalletVerifier) Verify(ctx context.Context, document json.RawMessage) error {
	var envelope struct {
		Issuer *string           `json:"issuer"`
		Proof  *trusttasks.Proof `json:"proof"`
	}
	if err := json.Unmarshal(document, &envelope); err != nil {
		return err
	}
	if envelope.Issuer == nil || !strings.HasPrefix(*envelope.Issuer, "did:key:z6Mk") || envelope.Proof == nil || envelope.Proof.ProofPurpose != "authentication" {
		return errors.New("wallet proof must authenticate an Ed25519 did:key issuer")
	}
	return v.VerifyDIDKey(ctx, document, *envelope.Issuer, "authentication")
}

func DigestMultibase(value any) (string, error) {
	canonical, err := canonicalJSON(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	multihash := append([]byte{0x12, 0x20}, digest[:]...)
	return "z" + didresolver.EncodeBase58BTC(multihash), nil
}

type SchemaValidator struct{}

var compiledSchemas sync.Map

func (SchemaValidator) Validate(schemaJSON string, payload json.RawMessage) error {
	compiled, ok := compiledSchemas.Load(schemaJSON)
	if !ok {
		schemaValue, err := jsonschema.UnmarshalJSON(strings.NewReader(schemaJSON))
		if err != nil {
			return err
		}
		compiler := jsonschema.NewCompiler()
		const location = "urn:vtafarm:trust-task-payload"
		if err := compiler.AddResource(location, schemaValue); err != nil {
			return err
		}
		schema, err := compiler.Compile(location)
		if err != nil {
			return err
		}
		compiled, _ = compiledSchemas.LoadOrStore(schemaJSON, schema)
	}
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	return compiled.(*jsonschema.Schema).Validate(value)
}
