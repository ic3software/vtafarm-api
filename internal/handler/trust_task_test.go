package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
	farmtrusttask "github.com/ic3software/vtafarm-api/internal/trusttask"
	prooflib "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/proof"
	provisionvtaclaimv0_1 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/provision/vta/claim/v0_1"
	provisionvtaredeemv0_1 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/provision/vta/redeem/v0_1"
	trusttasknextstepv0_1 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/trust-task-next-step/v0_1"
	"github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/trusttasks"
	"gorm.io/gorm"
)

type staticFarmDocumentResolver struct{ document []byte }

func (r staticFarmDocumentResolver) ResolveDocument(context.Context, string) ([]byte, error) {
	return r.document, nil
}

func newTestTrustTaskHandler(t *testing.T, db *gorm.DB, cfg config.VTAClaimConfig) *TrustTaskHandler {
	t.Helper()
	signer, err := farmtrusttask.NewSigner(cfg.FarmDID, cfg.FarmSigningKey)
	if err != nil {
		t.Fatal(err)
	}
	verificationMethod := signer.VerificationMethod()
	multikey := verificationMethod[strings.LastIndex(verificationMethod, "#")+1:]
	document, err := json.Marshal(map[string]any{
		"id": cfg.FarmDID,
		"verificationMethod": []map[string]any{{
			"id": verificationMethod, "type": "Multikey", "controller": cfg.FarmDID, "publicKeyMultibase": multikey,
		}},
		"assertionMethod": []string{verificationMethod},
		"service": []map[string]any{{
			"id": cfg.FarmDID + "#trust-tasks", "type": "TrustTaskHTTPS", "serviceEndpoint": cfg.TrustTaskBase,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := newTrustTaskHandler(db, cfg, staticFarmDocumentResolver{document: document})
	handler.redeemHold = 0
	return handler
}

func signWalletDocument[P any](t *testing.T, signer *prooflib.Signer, document *trusttasks.Document[P]) []byte {
	t.Helper()
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(encoded, prooflib.SignOptions{ProofPurpose: "authentication"})
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func postTrustTask(router http.Handler, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/trust-task/trust-tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

func TestTrustTaskClaimAndRedeemFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.Postgres(t)
	session := testutil.SetupSession(t, db, model.ModeVtaOnly)
	reservation, err := connection.Change(t.Context(), db, session.ID, "create", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := validVTAClaimConfig()
	handler := newTestTrustTaskHandler(t, db, cfg)
	router := gin.New()
	router.POST("/trust-task/trust-tasks", handler.Consume)

	walletSeed := make([]byte, 32)
	walletSeed[len(walletSeed)-1] = 7
	wallet, err := prooflib.SignerFromPrivateKey(prooflib.Ed25519, walletSeed)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	issuedAt, expiresAt := now.Format(time.RFC3339), now.Add(2*time.Minute).Format(time.RFC3339)
	claimID := uuid.NewString()
	claim := &trusttasks.Document[provisionvtaclaimv0_1.Payload]{
		ID:             claimID,
		ParentThreadID: reservation.ClaimHandle,
		Type:           provisionvtaclaimv0_1.TypeURI,
		Issuer:         &wallet.DID,
		Recipient:      &cfg.FarmDID,
		IssuedAt:       &issuedAt,
		ExpiresAt:      &expiresAt,
		Payload:        provisionvtaclaimv0_1.Payload{Handle: *reservation.ClaimHandle},
	}
	claimWire := signWalletDocument(t, wallet, claim)
	response := postTrustTask(router, claimWire)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("claim=%d %s", response.Code, response.Body)
	}
	var next trusttasks.Document[trusttasknextstepv0_1.Payload]
	if err := json.Unmarshal(response.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	if next.Type != trusttasknextstepv0_1.TypeURI || next.Proof == nil || next.Proof.ProofPurpose != "assertionMethod" ||
		len(next.Payload.Expects) != 1 || next.Payload.Expects[0].TypeURI != provisionvtaredeemv0_1.TypeURI {
		t.Fatalf("unexpected next step: %+v", next)
	}
	firstResponse := append([]byte(nil), response.Body.Bytes()...)
	response = postTrustTask(router, claimWire)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), firstResponse) {
		t.Fatalf("claim retry was not byte-identical: %d %s", response.Code, response.Body)
	}
	var claimed model.MobileConnection
	db.First(&claimed, "id = ?", reservation.ID)
	var unmodified model.SetupSession
	db.First(&unmodified, session.ID)
	if claimed.Status != "claimed" || claimed.AdminDid != wallet.DID || unmodified.AdminDid != "" {
		t.Fatalf("claim crossed owner confirmation boundary: %+v %+v", claimed, unmodified)
	}

	newRedeem := func() []byte {
		id := uuid.NewString()
		issued, expires := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339), time.Now().UTC().Add(2*time.Minute).Truncate(time.Second).Format(time.RFC3339)
		document := &trusttasks.Document[provisionvtaredeemv0_1.Payload]{
			ID: id, Type: provisionvtaredeemv0_1.TypeURI, Issuer: &wallet.DID, Recipient: &cfg.FarmDID,
			IssuedAt: &issued, ExpiresAt: &expires, Payload: provisionvtaredeemv0_1.Payload{Handle: *reservation.ClaimHandle},
		}
		return signWalletDocument(t, wallet, document)
	}
	response = postTrustTask(router, newRedeem())
	if response.Code != http.StatusUnprocessableEntity || !bytes.Contains(response.Body.Bytes(), []byte("provision/vta/redeem:pending")) {
		t.Fatalf("pending redeem=%d %s", response.Code, response.Body)
	}
	var pending struct {
		Payload struct {
			Details struct {
				State     string          `json:"state"`
				ConfirmBy json.RawMessage `json:"confirmBy"`
			} `json:"details"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	// The registered detailsSchema declares confirmBy as an integer (epoch seconds), like the link's _exp.
	if pending.Payload.Details.State != "claimed" || !regexp.MustCompile(`^[0-9]+$`).Match(pending.Payload.Details.ConfirmBy) {
		t.Fatalf("pending details are not as the schema declares: %s", response.Body)
	}

	if _, err := connection.Confirm(t.Context(), db, session.ID, reservation.ID); err != nil {
		t.Fatal(err)
	}
	started, err := connection.StartProvisioning(t.Context(), db, reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&session).Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&session, session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := connection.Reconcile(t.Context(), db, started, &session); err != nil {
		t.Fatal(err)
	}
	response = postTrustTask(router, newRedeem())
	if response.Code != http.StatusOK {
		t.Fatalf("released redeem=%d %s", response.Code, response.Body)
	}
	var handover trusttasks.Document[provisionvtaredeemv0_1.Response]
	if err := json.Unmarshal(response.Body.Bytes(), &handover); err != nil {
		t.Fatal(err)
	}
	if handover.Type != provisionvtaredeemv0_1.ResponseTypeURI || handover.Payload.VtaDID != session.VtaDid || handover.Proof == nil {
		t.Fatalf("unexpected hand-over: %+v", handover)
	}
}

func TestTrustTaskRejectsUnknownClaimPayloadMember(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.Postgres(t)
	handler := newTestTrustTaskHandler(t, db, validVTAClaimConfig())
	router := gin.New()
	router.POST("/trust-task/trust-tasks", handler.Consume)
	body := []byte(`{"id":"request-1","type":"https://trusttasks.org/spec/provision/vta/claim/0.1","payload":{"handle":"abcdefghijklmnopqrstuv","unexpected":true}}`)
	response := postTrustTask(router, body)
	if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte("malformedRequest")) {
		t.Fatalf("unknown payload member=%d %s", response.Code, response.Body)
	}
}

func TestRedeemLongPollIsReleasedByStateChangeAndExclusive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.Postgres(t)
	session := testutil.SetupSession(t, db, model.ModeVtaOnly)
	reservation, err := connection.Change(t.Context(), db, session.ID, "create", "")
	if err != nil {
		t.Fatal(err)
	}
	walletSeed := make([]byte, 32)
	walletSeed[len(walletSeed)-1] = 9
	wallet, err := prooflib.SignerFromPrivateKey(prooflib.Ed25519, walletSeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Claim(t.Context(), db, *reservation.ClaimHandle, wallet.DID); err != nil {
		t.Fatal(err)
	}

	cfg := validVTAClaimConfig()
	handler := newTestTrustTaskHandler(t, db, cfg)
	handler.redeemHold = 2 * time.Second
	router := gin.New()
	router.POST("/trust-task/trust-tasks", handler.Consume)
	newRedeem := func() []byte {
		now := time.Now().UTC().Truncate(time.Second)
		issuedAt, expiresAt := now.Format(time.RFC3339), now.Add(2*time.Minute).Format(time.RFC3339)
		document := &trusttasks.Document[provisionvtaredeemv0_1.Payload]{
			ID: uuid.NewString(), Type: provisionvtaredeemv0_1.TypeURI, Issuer: &wallet.DID, Recipient: &cfg.FarmDID,
			IssuedAt: &issuedAt, ExpiresAt: &expiresAt, Payload: provisionvtaredeemv0_1.Payload{Handle: *reservation.ClaimHandle},
		}
		return signWalletDocument(t, wallet, document)
	}

	firstResult := make(chan *httptest.ResponseRecorder, 1)
	firstBody := newRedeem()
	go func() { firstResult <- postTrustTask(router, firstBody) }()
	deadline := time.Now().Add(time.Second)
	for {
		var current model.MobileConnection
		if err := db.First(&current, "id = ?", reservation.ID).Error; err != nil {
			t.Fatal(err)
		}
		if current.RedeemPollID != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first redeem did not acquire its poll lease")
		}
		time.Sleep(20 * time.Millisecond)
	}

	second := postTrustTask(router, newRedeem())
	if second.Code != http.StatusServiceUnavailable || !bytes.Contains(second.Body.Bytes(), []byte("unavailable")) {
		t.Fatalf("concurrent redeem=%d %s", second.Code, second.Body)
	}
	if _, err := connection.Reject(t.Context(), db, session.ID, reservation.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case first := <-firstResult:
		if first.Code != http.StatusUnprocessableEntity || !bytes.Contains(first.Body.Bytes(), []byte("provision/vta/redeem:rejected")) {
			t.Fatalf("released redeem=%d %s", first.Code, first.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("redeem poll did not wake after rejection")
	}
}
