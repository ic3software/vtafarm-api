package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	farmtrusttask "github.com/ic3software/vtafarm-api/internal/trusttask"
	prooflib "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/proof"
	authoobclaimv0_2 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/claim/v0_2"
	authoobgrantv0_3 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/grant/v0_3"
	authoobidentifyv0_1 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/identify/v0_1"
	authoobprovev0_3 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/prove/v0_3"
	authoobrespondv0_3 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/respond/v0_3"
	"github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/trusttasks"
)

func TestRunCompletesWalletSideSignIn(t *testing.T) {
	const (
		requestID = "abcdefghijklmnopqrstuv"
		farmDID   = "did:webvh:example:dev-vtafarm"
		origin    = "https://farm.example.com"
	)
	now := time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC)
	seed := bytes.Repeat([]byte{7}, 32)
	wallet, err := prooflib.SignerFromPrivateKey(prooflib.Ed25519, seed)
	if err != nil {
		t.Fatal(err)
	}
	seedPath := filepath.Join(t.TempDir(), "wallet.seed")
	if err := os.WriteFile(seedPath, []byte(base64.StdEncoding.EncodeToString(seed)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stage := 0
	var proveResponse []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Error(readErr)
			return
		}
		if verifyErr := prooflib.NewVerifier().Verify(context.Background(), body); verifyErr != nil {
			t.Errorf("verify outer request: %v", verifyErr)
			return
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch envelope.Type {
		case authoobclaimv0_2.TypeURI:
			stage++
			var claim trusttasks.Document[authoobclaimv0_2.Payload]
			if err := json.Unmarshal(body, &claim); err != nil {
				t.Error(err)
				return
			}
			if claim.Issuer == nil || *claim.Issuer != wallet.DID || claim.ParentThreadID == nil || *claim.ParentThreadID != requestID {
				t.Errorf("unexpected claim: %+v", claim)
				return
			}
			writeResponse(t, w, farmDID, authoobclaimv0_2.TypeURI, map[string]any{
				"requestId": requestID,
				"service":   map[string]any{"did": farmDID, "name": "VTA Farm"},
				"origin":    origin, "purpose": "login", "decisionClosesAt": now.Add(2 * time.Minute).Format(time.RFC3339),
			})
		case authoobprovev0_3.TypeURI:
			stage++
			var prove trusttasks.Document[authoobprovev0_3.Payload]
			if err := json.Unmarshal(body, &prove); err != nil {
				t.Error(err)
				return
			}
			identifyBody, err := json.Marshal(prove.Payload.Identify)
			if err != nil {
				t.Error(err)
				return
			}
			if err := prooflib.NewVerifier().Verify(context.Background(), identifyBody); err != nil {
				t.Errorf("verify identify: %v", err)
				return
			}
			var identify trusttasks.Document[authoobidentifyv0_1.Payload]
			if err := json.Unmarshal(identifyBody, &identify); err != nil {
				t.Error(err)
				return
			}
			if identify.Issuer == nil || *identify.Issuer != wallet.DID || identify.Payload.EnteredNumber != "42" {
				t.Errorf("unexpected identify: %+v", identify)
				return
			}
			proveResponse = responseDocument(t, farmDID, authoobprovev0_3.TypeURI, map[string]any{
				"requestId": requestID,
				"service":   map[string]any{"did": farmDID, "name": "VTA Farm"},
				"origin":    origin, "purpose": "login", "decisionClosesAt": now.Add(2 * time.Minute).Format(time.RFC3339),
				"sessionKey": "did:key:z6Mktestbrowser", "requester": map[string]any{}, "identifiedAs": wallet.DID,
			})
			_, _ = w.Write(proveResponse)
		case authoobrespondv0_3.TypeURI:
			stage++
			var respond trusttasks.Document[authoobrespondv0_3.Payload]
			if err := json.Unmarshal(body, &respond); err != nil {
				t.Error(err)
				return
			}
			grantBody, err := json.Marshal(respond.Payload.Grant)
			if err != nil {
				t.Error(err)
				return
			}
			if err := prooflib.NewVerifier().Verify(context.Background(), grantBody); err != nil {
				t.Errorf("verify grant: %v", err)
				return
			}
			var grant trusttasks.Document[authoobgrantv0_3.Payload]
			if err := json.Unmarshal(grantBody, &grant); err != nil {
				t.Error(err)
				return
			}
			expectedDigest, err := farmtrusttask.DigestMultibase(json.RawMessage(proveResponse))
			if err != nil {
				t.Error(err)
				return
			}
			if grant.Issuer == nil || *grant.Issuer != wallet.DID || grant.Payload.ContextDigest != expectedDigest || grant.Payload.Decision != authoobgrantv0_3.PayloadDecisionApprove {
				t.Errorf("unexpected grant: %+v", grant)
				return
			}
			writeResponse(t, w, farmDID, authoobrespondv0_3.TypeURI, map[string]any{"status": "approved"})
		default:
			t.Errorf("unexpected type %q", envelope.Type)
			http.Error(w, "unexpected type", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	var output bytes.Buffer
	err = run(config{apiBase: server.URL, requestID: requestID, farmDID: farmDID, walletSeedFile: seedPath},
		func() time.Time { return now }, server.Client(), strings.NewReader("42\n"), &output)
	if err != nil {
		t.Fatal(err)
	}
	if stage != 3 || !strings.Contains(output.String(), "Approved") || !strings.Contains(output.String(), wallet.DID) {
		t.Fatalf("stage=%d output=%q", stage, output.String())
	}
}

func TestTrustTaskEndpointRejectsNonLocalAPI(t *testing.T) {
	_, err := trustTaskEndpoint(config{apiBase: "https://api.example.com", requestID: "abcdefghijklmnopqrstuv", farmDID: "did:webvh:example:farm"})
	if err == nil {
		t.Fatal("accepted a non-local development API")
	}
}

func writeResponse(t *testing.T, w http.ResponseWriter, farmDID, typeURI string, payload any) {
	t.Helper()
	_, _ = w.Write(responseDocument(t, farmDID, typeURI, payload))
}

func responseDocument(t *testing.T, farmDID, typeURI string, payload any) []byte {
	t.Helper()
	now := "2026-10-29T20:00:00Z"
	document := trusttasks.Document[any]{ID: "urn:uuid:test-response", Type: typeURI, Issuer: &farmDID, IssuedAt: &now, Payload: payload}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
