package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ic3software/vtafarm-api/internal/trusttask"
	"github.com/ic3software/vtafarm-api/internal/webvh"
)

func main() {
	if len(os.Args) != 4 {
		log.Fatal("usage: gen-farm-key <did:webvh Farm DID> <HTTPS Trust Task base ending in /trust-task> <HTTPS SignInPortal URL>")
	}
	farmDID := os.Args[1]
	trustTaskBase := os.Args[2]
	signInPortal := os.Args[3]
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		log.Fatal(err)
	}
	encodedSeed := base64.StdEncoding.EncodeToString(seed)
	signer, err := trusttask.NewSigner(farmDID, encodedSeed)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), webvh.DefaultTimeout)
	defer cancel()
	currentDocument, err := webvh.NewResolver(webvh.DefaultTimeout).ResolveDocument(ctx, farmDID)
	if err != nil {
		log.Fatalf("resolve current Farm DID document: %v", err)
	}
	document, err := buildDIDDocument(currentDocument, farmDID, trustTaskBase, signInPortal, signer)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("FARM_SIGNING_KEY=%s\n", encodedSeed)
	fmt.Printf("FARM_VERIFICATION_METHOD=%s\n", signer.VerificationMethod())
	fmt.Println("\nUpdated DID document (publish with `pnm did-mgmt dids edit`):")
	fmt.Println(string(document))
}

type verificationMethod struct {
	ID                 string `json:"id"`
	Type               string `json:"type"`
	Controller         string `json:"controller"`
	PublicKeyMultibase string `json:"publicKeyMultibase"`
}

type service struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	ServiceEndpoint string `json:"serviceEndpoint"`
}

func buildDIDDocument(currentDocument []byte, farmDID, trustTaskBase, signInPortal string, signer *trusttask.Signer) ([]byte, error) {
	verificationMethodID := signer.VerificationMethod()
	publicKeyMultibase := strings.TrimPrefix(verificationMethodID, farmDID+"#")
	if publicKeyMultibase == verificationMethodID || publicKeyMultibase == "" {
		return nil, fmt.Errorf("Farm verification method is not controlled by FARM_DID")
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(currentDocument, &document); err != nil {
		return nil, fmt.Errorf("decode current Farm DID document: %w", err)
	}
	var documentID string
	if err := json.Unmarshal(document["id"], &documentID); err != nil || documentID != farmDID {
		return nil, fmt.Errorf("current Farm DID document id does not match FARM_DID")
	}

	method := verificationMethod{
		ID:                 verificationMethodID,
		Type:               "Multikey",
		Controller:         farmDID,
		PublicKeyMultibase: publicKeyMultibase,
	}
	methods, err := upsertObjectByID(document["verificationMethod"], verificationMethodID, method)
	if err != nil {
		return nil, fmt.Errorf("merge verificationMethod: %w", err)
	}
	document["verificationMethod"] = methods

	assertionMethods, err := appendUniqueString(document["assertionMethod"], verificationMethodID)
	if err != nil {
		return nil, fmt.Errorf("merge assertionMethod: %w", err)
	}
	document["assertionMethod"] = assertionMethods

	trustTaskService := service{
		ID:              farmDID + "#trust-tasks",
		Type:            "TrustTaskHTTPS",
		ServiceEndpoint: trustTaskBase,
	}
	services, err := upsertObjectByID(document["service"], trustTaskService.ID, trustTaskService)
	if err != nil {
		return nil, fmt.Errorf("merge service: %w", err)
	}
	signInService := service{
		ID:              farmDID + "#sign-in-portal",
		Type:            "SignInPortal",
		ServiceEndpoint: signInPortal,
	}
	services, err = upsertObjectByID(services, signInService.ID, signInService)
	if err != nil {
		return nil, fmt.Errorf("merge SignInPortal service: %w", err)
	}
	document["service"] = services

	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Farm DID document: %w", err)
	}
	if err := trusttask.ValidateFarmDIDDocument(encoded, signer, trustTaskBase); err != nil {
		return nil, err
	}
	if err := trusttask.ValidateSignInPortal(encoded, farmDID, signInPortal); err != nil {
		return nil, err
	}
	return encoded, nil
}

func upsertObjectByID(existing json.RawMessage, id string, replacement any) (json.RawMessage, error) {
	items := make([]json.RawMessage, 0)
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &items); err != nil {
			return nil, err
		}
	}
	replacementJSON, err := json.Marshal(replacement)
	if err != nil {
		return nil, err
	}
	for index, item := range items {
		var objectID struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(item, &objectID) == nil && objectID.ID == id {
			items[index] = replacementJSON
			return json.Marshal(items)
		}
	}
	items = append(items, replacementJSON)
	return json.Marshal(items)
}

func appendUniqueString(existing json.RawMessage, value string) (json.RawMessage, error) {
	items := make([]json.RawMessage, 0)
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &items); err != nil {
			return nil, err
		}
	}
	for _, item := range items {
		var current string
		if json.Unmarshal(item, &current) == nil && current == value {
			return json.Marshal(items)
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	items = append(items, encoded)
	return json.Marshal(items)
}
