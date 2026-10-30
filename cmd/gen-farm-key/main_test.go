package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/trusttask"
)

func TestBuildDIDDocumentPreservesExistingDocument(t *testing.T) {
	farmDID := "did:webvh:QmFarm:dev-dids.example.com:vtafarm-auth"
	trustTaskBase := "https://dev-vtafarm-api.example.com/trust-task"
	signInPortal := "https://dev-vtafarm.example.com/login"
	signer, err := trusttask.NewSigner(farmDID, base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	current := []byte(`{
  "@context": ["https://www.w3.org/ns/did/v1"],
  "id": "` + farmDID + `",
  "verificationMethod": [{"id":"` + farmDID + `#key-0","type":"Multikey","controller":"` + farmDID + `","publicKeyMultibase":"z6MkExisting"}],
  "assertionMethod": ["` + farmDID + `#key-0"],
  "authentication": ["` + farmDID + `#key-0"],
  "keyAgreement": ["` + farmDID + `#key-1"],
  "service": [{"id":"` + farmDID + `#tsp","type":"TSPTransport","serviceEndpoint":"did:webvh:mediator"}]
}`)

	document, err := buildDIDDocument(current, farmDID, trustTaskBase, signInPortal, signer)
	if err != nil {
		t.Fatal(err)
	}
	if err := trusttask.ValidateFarmDIDDocument(document, signer, trustTaskBase); err != nil {
		t.Fatalf("generated DID document does not validate: %v", err)
	}
	if err := trusttask.ValidateSignInPortal(document, farmDID, signInPortal); err != nil {
		t.Fatalf("generated SignInPortal does not validate: %v", err)
	}

	var parsed struct {
		Context            []string             `json:"@context"`
		ID                 string               `json:"id"`
		VerificationMethod []verificationMethod `json:"verificationMethod"`
		AssertionMethod    []string             `json:"assertionMethod"`
		Authentication     []string             `json:"authentication"`
		KeyAgreement       []string             `json:"keyAgreement"`
		Service            []service            `json:"service"`
	}
	if err := json.Unmarshal(document, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.ID != farmDID || len(parsed.Context) != 1 {
		t.Fatalf("identity fields were not preserved: %#v", parsed)
	}
	if len(parsed.VerificationMethod) != 2 || parsed.VerificationMethod[0].ID != farmDID+"#key-0" || parsed.VerificationMethod[1].ID != signer.VerificationMethod() {
		t.Fatalf("verificationMethod = %#v", parsed.VerificationMethod)
	}
	if len(parsed.AssertionMethod) != 2 || parsed.AssertionMethod[0] != farmDID+"#key-0" || parsed.AssertionMethod[1] != signer.VerificationMethod() {
		t.Fatalf("assertionMethod = %#v", parsed.AssertionMethod)
	}
	if len(parsed.Authentication) != 1 || len(parsed.KeyAgreement) != 1 {
		t.Fatalf("DID relationships were not preserved: authentication=%#v keyAgreement=%#v", parsed.Authentication, parsed.KeyAgreement)
	}
	if len(parsed.Service) != 3 || parsed.Service[0].ID != farmDID+"#tsp" || parsed.Service[1].ID != farmDID+"#trust-tasks" || parsed.Service[2].ID != farmDID+"#sign-in-portal" {
		t.Fatalf("service = %#v", parsed.Service)
	}
}

func TestBuildDIDDocumentIsIdempotent(t *testing.T) {
	farmDID := "did:webvh:QmFarm:dev-dids.example.com:vtafarm-auth"
	trustTaskBase := "https://dev-vtafarm-api.example.com/trust-task"
	signInPortal := "https://dev-vtafarm.example.com/login"
	signer, err := trusttask.NewSigner(farmDID, base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	current := []byte(`{"id":"` + farmDID + `"}`)

	first, err := buildDIDDocument(current, farmDID, trustTaskBase, signInPortal, signer)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildDIDDocument(first, farmDID, trustTaskBase, signInPortal, signer)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("second merge changed the document:\nfirst: %s\nsecond: %s", first, second)
	}
}

func TestBuildDIDDocumentRejectsInvalidInputs(t *testing.T) {
	farmDID := "did:webvh:QmFarm:dev-dids.example.com:vtafarm-auth"
	signer, err := trusttask.NewSigner(farmDID, base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	current := []byte(`{"id":"` + farmDID + `"}`)

	if _, err := buildDIDDocument(current, farmDID, "http://localhost:8080/trust-task", "https://farm.example.com/login", signer); err == nil {
		t.Fatal("accepted an HTTP TRUST_TASK_BASE")
	}
	if _, err := buildDIDDocument([]byte(`{"id":"did:webvh:other"}`), farmDID, "https://api.example.com/trust-task", "https://farm.example.com/login", signer); err == nil {
		t.Fatal("accepted a current document with a different id")
	}
	if _, err := buildDIDDocument(current, farmDID, "https://api.example.com/trust-task", "http://farm.example.com/login", signer); err == nil {
		t.Fatal("accepted an HTTP SignInPortal URL")
	}
}
