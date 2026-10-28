package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	prooflib "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/proof"
	provisionvtaclaimv0_1 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/provision/vta/claim/v0_1"
	"github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/trusttasks"
)

func TestSignedClaim(t *testing.T) {
	t.Parallel()
	handle := "YYZKxe9NXuYJQca1NZgK7g"
	farmDID := "did:webvh:example:dev-vtafarm"
	now := time.Date(2026, 10, 27, 20, 0, 0, 0, time.UTC)
	body, walletDID, claimCheck, err := signedClaim(handle, farmDID, now, bytes.NewReader(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if walletDID == "" || len(claimCheck) != 6 {
		t.Fatalf("wallet DID=%q claim check=%q", walletDID, claimCheck)
	}
	if err := prooflib.NewVerifier().Verify(context.Background(), body); err != nil {
		t.Fatalf("verify signed claim: %v", err)
	}
	var document trusttasks.Document[provisionvtaclaimv0_1.Payload]
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if document.ParentThreadID == nil || *document.ParentThreadID != handle || document.Payload.Handle != handle {
		t.Fatalf("claim handle fields do not match: %+v", document)
	}
	if document.Issuer == nil || *document.Issuer != walletDID || document.Recipient == nil || *document.Recipient != farmDID {
		t.Fatalf("claim parties do not match: %+v", document)
	}
	if document.Proof == nil || document.Proof.ProofPurpose != "authentication" {
		t.Fatalf("claim proof is missing or invalid: %+v", document.Proof)
	}
}

func TestClaimEndpointRejectsNonLocalAPI(t *testing.T) {
	t.Parallel()
	_, err := claimEndpoint(config{
		apiBase: "https://api.example.com",
		handle:  "YYZKxe9NXuYJQca1NZgK7g",
		farmDID: "did:webvh:example:dev-vtafarm",
	})
	if err == nil {
		t.Fatal("expected non-local API base to be rejected")
	}
}

func TestRunPostsSignedClaim(t *testing.T) {
	t.Parallel()
	handle := "YYZKxe9NXuYJQca1NZgK7g"
	farmDID := "did:webvh:example:dev-vtafarm"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/trust-task/trust-tasks" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if err := prooflib.NewVerifier().Verify(r.Context(), body); err != nil {
			t.Errorf("verify request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	var output bytes.Buffer
	err := run(config{apiBase: server.URL, handle: handle, farmDID: farmDID},
		func() time.Time { return time.Date(2026, 10, 27, 20, 0, 0, 0, time.UTC) },
		bytes.NewReader(make([]byte, 32)), server.Client(), &output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Claim started.") || !strings.Contains(output.String(), "Claim check:") {
		t.Fatalf("output = %q", output.String())
	}
}
