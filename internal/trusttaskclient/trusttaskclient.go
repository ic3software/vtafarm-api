// Package trusttaskclient holds what an outbound Trust Task client needs and
// nothing a server needs: the document envelope, the eddsa-jcs-2022 proof a
// did:key makes over it, and the checks a signed reply has to pass before its
// payload is believed. The DID hosting client and the VTA client are both
// built on it, so the two never disagree about what a proof covers.
//
// The proof follows the Data Integrity eddsa-jcs-2022 cryptosuite: the signed
// message is sha256(JCS(proof options without proofValue)) followed by
// sha256(JCS(document without proof)), and proofValue is the base58btc
// multibase ("z…") of the Ed25519 signature. Both the DID hosting daemon and
// the VTA verify the JSON exactly as received, so a document is signed after
// its final marshalling shape is fixed and never re-encoded afterwards.
package trusttaskclient

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"

	"github.com/ic3software/vtafarm-api/internal/didresolver"
)

// MaxResponseBytes caps what a reply may carry; anything over it is refused
// unread. Both services cap their own bodies at the same size.
const MaxResponseBytes = 1 << 20

// Document is the Trust Task envelope as sent and received. Fields the spec
// leaves optional are omitted when empty so the signed bytes carry no nulls.
type Document struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	ThreadID  string          `json:"threadId,omitempty"`
	Issuer    string          `json:"issuer,omitempty"`
	Recipient string          `json:"recipient,omitempty"`
	IssuedAt  string          `json:"issuedAt,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	Proof     *Proof          `json:"proof,omitempty"`
}

// Proof is a Data Integrity proof. Only the members the cryptosuite signs are
// carried: a verifier refuses a proof with members outside this set.
type Proof struct {
	Type               string `json:"type"`
	Cryptosuite        string `json:"cryptosuite"`
	VerificationMethod string `json:"verificationMethod"`
	Created            string `json:"created"`
	ProofPurpose       string `json:"proofPurpose"`
	ProofValue         string `json:"proofValue,omitempty"`
}

// Sign attaches an eddsa-jcs-2022 proof with purpose `authentication`, made
// by privateKey, naming kid as the verification method. Any proof already on
// the document is discarded first.
func Sign(doc *Document, kid string, privateKey ed25519.PrivateKey, created string) error {
	doc.Proof = nil
	options := Proof{
		Type:               "DataIntegrityProof",
		Cryptosuite:        "eddsa-jcs-2022",
		VerificationMethod: kid,
		Created:            created,
		ProofPurpose:       "authentication",
	}
	message, err := ProofMessage(doc, options)
	if err != nil {
		return err
	}
	options.ProofValue = "z" + didresolver.EncodeBase58BTC(ed25519.Sign(privateKey, message))
	doc.Proof = &options
	return nil
}

// ProofMessage is the 64 bytes the cryptosuite signs for doc under the proof
// options p (its ProofValue is ignored). doc.Proof must be nil.
func ProofMessage(doc *Document, p Proof) ([]byte, error) {
	p.ProofValue = ""
	proofCanonical, err := CanonicalJSON(p)
	if err != nil {
		return nil, fmt.Errorf("canonicalize proof options: %w", err)
	}
	documentCanonical, err := CanonicalJSON(doc)
	if err != nil {
		return nil, fmt.Errorf("canonicalize document: %w", err)
	}
	proofHash := sha256.Sum256(proofCanonical)
	documentHash := sha256.Sum256(documentCanonical)
	message := make([]byte, 0, len(proofHash)+len(documentHash))
	message = append(message, proofHash[:]...)
	message = append(message, documentHash[:]...)
	return message, nil
}

// CanonicalJSON is RFC 8785 JCS of value's JSON encoding.
func CanonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return jsoncanonicalizer.Transform(encoded)
}

// DecodeProofValue reads a canonical base58btc multibase proofValue.
func DecodeProofValue(value string) ([]byte, error) {
	if !strings.HasPrefix(value, "z") {
		return nil, errors.New("Trust Task proofValue is not base58btc multibase")
	}
	decoded, err := didresolver.DecodeBase58BTC(value[1:])
	if err != nil || didresolver.EncodeBase58BTC(decoded) != value[1:] {
		return nil, errors.New("Trust Task proofValue is not canonical base58btc")
	}
	return decoded, nil
}

// Reply is the identity a signed reply must prove and whom it must address.
type Reply struct {
	// Service is the DID the reply must be issued and signed by.
	Service string
	// Client is the DID the reply must be addressed to.
	Client string
	// Now is the clock the freshness checks use; nil means time.Now.
	Now func() time.Time
}

// VerifyReply checks that reply answers request and was signed by the
// service: threaded to the request id, issued by the service to the client,
// issued within the last five minutes (plus clock skew), carrying an
// eddsa-jcs-2022 `authentication` proof under a key the service's DID
// document lists for authentication, which verifies over the reply as
// received. resolver fetches that key.
func VerifyReply(ctx context.Context, resolver didresolver.AuthenticationKeyResolver, reply, request *Document, expect Reply) error {
	now := time.Now
	if expect.Now != nil {
		now = expect.Now
	}
	if reply.ThreadID != request.ID {
		return errors.New("Trust Task response is not threaded to the request")
	}
	if reply.Issuer != expect.Service {
		return errors.New("Trust Task response issuer does not match the service DID")
	}
	if reply.Recipient != expect.Client {
		return errors.New("Trust Task response recipient does not match the client DID")
	}
	issuedAt, err := time.Parse(time.RFC3339, reply.IssuedAt)
	if err != nil || issuedAt.Before(now().Add(-5*time.Minute)) || issuedAt.After(now().Add(didresolver.DefaultClockSkew)) {
		return errors.New("Trust Task response is outside the freshness window")
	}
	if reply.Proof == nil {
		return errors.New("Trust Task response has no proof")
	}
	p := *reply.Proof
	if p.Type != "DataIntegrityProof" || p.Cryptosuite != "eddsa-jcs-2022" || p.ProofPurpose != "authentication" {
		return errors.New("Trust Task response has an unsupported proof")
	}
	proofDid, _, ok := strings.Cut(p.VerificationMethod, "#")
	if !ok || proofDid != expect.Service {
		return errors.New("Trust Task response proof does not belong to the service DID")
	}
	created, err := time.Parse(time.RFC3339, p.Created)
	if err != nil || created.After(now().Add(didresolver.DefaultClockSkew)) {
		return errors.New("Trust Task response proof has an invalid creation time")
	}
	signature, err := DecodeProofValue(p.ProofValue)
	if err != nil {
		return err
	}
	publicKey, err := resolver.ResolveAuthenticationKey(ctx, expect.Service, p.VerificationMethod)
	if err != nil {
		return fmt.Errorf("resolve Trust Task response key: %w", err)
	}
	reply.Proof = nil
	message, err := ProofMessage(reply, p)
	reply.Proof = &p
	if err != nil {
		return fmt.Errorf("canonicalize Trust Task response: %w", err)
	}
	if len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, message, signature) {
		return errors.New("Trust Task response proof signature is invalid")
	}
	return nil
}
