package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	authoobclaimv0_2 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/claim/v0_2"
	authoobgrantv0_3 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/grant/v0_3"
	authoobidentifyv0_1 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/identify/v0_1"
	authoobprovev0_3 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/prove/v0_3"
	authoobredeemv0_2 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/redeem/v0_2"
	authoobrequestv0_2 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/request/v0_2"
	authoobrespondv0_3 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/respond/v0_3"
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
	services := []map[string]any{{
		"id": cfg.FarmDID + "#trust-tasks", "type": "TrustTaskHTTPS", "serviceEndpoint": cfg.TrustTaskBase,
	}}
	if cfg.SignInPortal != "" {
		services = append(services, map[string]any{"id": cfg.FarmDID + "#sign-in-portal", "type": "SignInPortal", "serviceEndpoint": cfg.SignInPortal})
	}
	document, err := json.Marshal(map[string]any{
		"id": cfg.FarmDID,
		"verificationMethod": []map[string]any{{
			"id": verificationMethod, "type": "Multikey", "controller": cfg.FarmDID, "publicKeyMultibase": multikey,
		}},
		"assertionMethod": []string{verificationMethod},
		"service":         services,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := newTrustTaskHandler(db, cfg, staticFarmDocumentResolver{document: document})
	handler.redeemHold = 0
	return handler
}

func signWalletDocument[P any](t *testing.T, signer *prooflib.Signer, document *trusttasks.Document[P]) []byte {
	return signWalletDocumentAs(t, signer, document, "authentication")
}

func signWalletDocumentAs[P any](t *testing.T, signer *prooflib.Signer, document *trusttasks.Document[P], purpose string) []byte {
	t.Helper()
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(encoded, prooflib.SignOptions{ProofPurpose: purpose})
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func postTrustTaskFrom(router http.Handler, body []byte, origin string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/trust-task/trust-tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	router.ServeHTTP(w, req)
	return w
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
	type pendingResponse struct {
		Payload struct {
			Details struct {
				State     string `json:"state"`
				ConfirmBy *int64 `json:"confirmBy"`
			} `json:"details"`
		} `json:"payload"`
	}
	var pending pendingResponse
	if err := json.Unmarshal(response.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Payload.Details.State != "claimed" || pending.Payload.Details.ConfirmBy == nil || claimed.ConfirmBy == nil ||
		*pending.Payload.Details.ConfirmBy != claimed.ConfirmBy.Unix() {
		t.Fatalf("pending details are not as the schema declares: %s", response.Body)
	}

	if _, err := connection.Confirm(t.Context(), db, session.ID, reservation.ID); err != nil {
		t.Fatal(err)
	}
	response = postTrustTask(router, newRedeem())
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("confirmed redeem=%d %s", response.Code, response.Body)
	}
	pending = pendingResponse{}
	if err := json.Unmarshal(response.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Payload.Details.State != "confirmed" || pending.Payload.Details.ConfirmBy != nil {
		t.Fatalf("confirmed details include a confirmation deadline: %s", response.Body)
	}
	started, err := connection.StartProvisioning(t.Context(), db, reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	response = postTrustTask(router, newRedeem())
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("provisioning redeem=%d %s", response.Code, response.Body)
	}
	pending = pendingResponse{}
	if err := json.Unmarshal(response.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Payload.Details.State != "provisioning" || pending.Payload.Details.ConfirmBy != nil {
		t.Fatalf("provisioning details include a confirmation deadline: %s", response.Body)
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

func TestKeyringOOBSignInIssuesRevocableBoundedSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.Postgres(t)
	ctx := t.Context()
	session := testutil.SetupSession(t, db, model.ModeVtaOnly)
	admin := model.Admin{UniqueId: "keyringadm"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	batch := model.PoolBatch{CreatedBy: admin.ID, Label: "Keyring pool", RequestedCount: 1, IdempotencyKey: "keyring-pool", RequestDigest: "keyring-pool"}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.PoolSession{SessionID: session.ID, BatchID: batch.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&session).Update("status", model.StatusPoolWaiting).Error; err != nil {
		t.Fatal(err)
	}
	session.Status = model.StatusPoolWaiting

	identitySeed := make([]byte, 32)
	identitySeed[len(identitySeed)-1] = 21
	identity, err := prooflib.SignerFromPrivateKey(prooflib.Ed25519, identitySeed)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := connection.ChangePool(ctx, db, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Claim(ctx, db, *reservation.ClaimHandle, identity.DID); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Confirm(ctx, db, session.ID, reservation.ID); err != nil {
		t.Fatal(err)
	}
	started, err := connection.StartProvisioning(ctx, db, reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SetupSession{}).Where("id = ?", session.ID).Updates(map[string]any{"status": "running"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&session, session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := connection.Reconcile(ctx, db, started, &session); err != nil {
		t.Fatal(err)
	}
	var firstBinding model.WalletLoginBinding
	if err := db.First(&firstBinding, "login_did = ?", identity.DID).Error; err != nil {
		t.Fatal(err)
	}
	secondSession := testutil.SetupSession(t, db, model.ModeVtaOnly)
	if err := db.Create(&model.PoolSession{SessionID: secondSession.ID, BatchID: batch.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&secondSession).Update("status", model.StatusPoolWaiting).Error; err != nil {
		t.Fatal(err)
	}
	secondReservation, err := connection.ChangePool(ctx, db, secondSession.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Claim(ctx, db, *secondReservation.ClaimHandle, identity.DID); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Confirm(ctx, db, secondSession.ID, secondReservation.ID); err != nil {
		t.Fatal(err)
	}
	secondStarted, err := connection.StartProvisioning(ctx, db, secondReservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&secondSession).Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&secondSession, secondSession.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := connection.Reconcile(ctx, db, secondStarted, &secondSession); err != nil {
		t.Fatal(err)
	}

	cfg := validVTAClaimConfig()
	cfg.SignInPortal = "https://farm.example.com/login"
	cfg.FarmName = "VTA Farm"
	handler := newTestTrustTaskHandler(t, db, cfg)
	handler.ConfigureWalletSessions("test-secret", true)
	handler.redeemHold = 0
	if !handler.WalletSignInEnabled() {
		t.Fatal("wallet sign-in was not enabled")
	}
	router := gin.New()
	router.POST("/trust-task/trust-tasks", handler.Consume)

	browserSeed := make([]byte, 32)
	browserSeed[len(browserSeed)-1] = 22
	browser, err := prooflib.SignerFromPrivateKey(prooflib.Ed25519, browserSeed)
	if err != nil {
		t.Fatal(err)
	}
	lockSeed := make([]byte, 32)
	lockSeed[len(lockSeed)-1] = 23
	lock, err := prooflib.SignerFromPrivateKey(prooflib.Ed25519, lockSeed)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	issuedAt, expiresAt := now.Format(time.RFC3339), now.Add(2*time.Minute).Format(time.RFC3339)
	newDocument := func(typeURI string, issuer string, payload any) *trusttasks.Document[any] {
		return &trusttasks.Document[any]{ID: uuid.NewString(), Type: typeURI, Issuer: &issuer, Recipient: &cfg.FarmDID, IssuedAt: &issuedAt, ExpiresAt: &expiresAt, Payload: payload}
	}

	open := newDocument(authoobrequestv0_2.TypeURI, browser.DID, authoobrequestv0_2.Payload{Purpose: authoobrequestv0_2.PurposeLogin, Mode: authoobrequestv0_2.ModeScan})
	response := postTrustTaskFrom(router, signWalletDocument(t, browser, open), "https://farm.example.com")
	if response.Code != http.StatusOK {
		t.Fatalf("request=%d %s", response.Code, response.Body)
	}
	var opened trusttasks.Document[authoobrequestv0_2.Response]
	if err := json.Unmarshal(response.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}

	claim := newDocument(authoobclaimv0_2.TypeURI, lock.DID, authoobclaimv0_2.Payload{RequestID: opened.Payload.RequestID})
	claim.ParentThreadID = &opened.Payload.RequestID
	response = postTrustTask(router, signWalletDocument(t, lock, claim))
	if response.Code != http.StatusOK {
		t.Fatalf("claim=%d %s", response.Code, response.Body)
	}
	var login model.WalletLoginRequest
	if err := db.First(&login, "request_id = ?", opened.Payload.RequestID).Error; err != nil || login.MatchNumber == nil {
		t.Fatalf("claimed request: %+v %v", login, err)
	}

	identify := newDocument(authoobidentifyv0_1.TypeURI, identity.DID, authoobidentifyv0_1.Payload{RequestID: login.RequestID, ApproverKey: lock.DID, EnteredNumber: *login.MatchNumber})
	identifyWire := signWalletDocument(t, identity, identify)
	var identifyObject map[string]json.RawMessage
	if err := json.Unmarshal(identifyWire, &identifyObject); err != nil {
		t.Fatal(err)
	}
	prove := newDocument(authoobprovev0_3.TypeURI, lock.DID, authoobprovev0_3.Payload{Identify: identifyObject})
	response = postTrustTask(router, signWalletDocument(t, lock, prove))
	if response.Code != http.StatusOK {
		t.Fatalf("prove=%d %s", response.Code, response.Body)
	}
	if err := db.First(&login, "request_id = ?", login.RequestID).Error; err != nil || login.Step2Digest == nil {
		t.Fatalf("identified request: %+v %v", login, err)
	}
	if login.BindingID != nil {
		t.Fatalf("multiple accounts were selected implicitly: %+v", login)
	}
	selection := authoobredeemv0_2.Ext{"com.ic3software.vtafarm": mustJSON(gin.H{"bindingId": firstBinding.ID})}
	chooseAccount := newDocument(authoobredeemv0_2.TypeURI, browser.DID, authoobredeemv0_2.Payload{RequestID: login.RequestID, Ext: &selection})
	response = postTrustTask(router, signWalletDocument(t, browser, chooseAccount))
	if response.Code == http.StatusOK {
		t.Fatalf("account selection completed before wallet approval: %s", response.Body)
	}
	if err := db.First(&login, "request_id = ?", login.RequestID).Error; err != nil || login.BindingID == nil || *login.BindingID != firstBinding.ID {
		t.Fatalf("explicit account selection was not bound: %+v %v", login, err)
	}

	grantPayload := authoobgrantv0_3.Payload{
		RequestID: login.RequestID, Decision: authoobgrantv0_3.PayloadDecisionApprove,
		SessionKey: browser.DID, ApproverKey: lock.DID, Origin: "https://farm.example.com",
		ContextDigest: *login.Step2Digest, SessionEndsAt: now.Add(2 * time.Hour).Format(time.RFC3339),
	}
	grant := newDocument(authoobgrantv0_3.TypeURI, identity.DID, grantPayload)
	grantWire := signWalletDocumentAs(t, identity, grant, "assertionMethod")
	var grantObject map[string]json.RawMessage
	if err := json.Unmarshal(grantWire, &grantObject); err != nil {
		t.Fatal(err)
	}
	respond := newDocument(authoobrespondv0_3.TypeURI, lock.DID, authoobrespondv0_3.Payload{Grant: grantObject})
	response = postTrustTask(router, signWalletDocument(t, lock, respond))
	if response.Code != http.StatusOK {
		t.Fatalf("respond=%d %s", response.Code, response.Body)
	}

	redeem := newDocument(authoobredeemv0_2.TypeURI, browser.DID, authoobredeemv0_2.Payload{RequestID: login.RequestID})
	response = postTrustTask(router, signWalletDocument(t, browser, redeem))
	if response.Code != http.StatusOK || len(response.Result().Cookies()) != 1 {
		t.Fatalf("redeem=%d cookies=%v %s", response.Code, response.Result().Cookies(), response.Body)
	}
	var walletSessions int64
	if err := db.Model(&model.WalletLoginSession{}).Count(&walletSessions).Error; err != nil || walletSessions != 1 {
		t.Fatalf("wallet sessions=%d err=%v", walletSessions, err)
	}
	if err := db.Model(&firstBinding).Update("revoked_at", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	retry := newDocument(authoobredeemv0_2.TypeURI, browser.DID, authoobredeemv0_2.Payload{RequestID: login.RequestID})
	response = postTrustTask(router, signWalletDocument(t, browser, retry))
	if response.Code == http.StatusOK {
		t.Fatalf("revoked binding redeemed again: %s", response.Body)
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
