package handler

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	poolstore "github.com/ic3software/vtafarm-api/internal/pool"
	"github.com/ic3software/vtafarm-api/internal/testutil"
	"gorm.io/gorm"
)

func TestCleanupPoolBatchDeletesDedicatedOwnerAndBatch(t *testing.T) {
	db := testutil.Postgres(t)
	admin := model.Admin{UniqueId: "pool-cleanup"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	admitted, err := poolstore.Admit(context.Background(), db, poolstore.Admission{
		AdminID: admin.ID, Key: "cleanup", Digest: "cleanup", Label: "Cleanup", Count: 1,
		Capacity: 1,
		Session: model.SetupSession{
			Mode: model.ModeVtaOnly, Domain: "example.com", Subdomain: "vta-",
			VtaImage: "example/vta:test", DidHostingServerURL: "https://dids.example.com",
		},
		MemoryRequest: "32Mi", MemoryLimit: "64Mi",
	})
	if err != nil {
		t.Fatal(err)
	}
	batch := admitted.Batch
	var membership model.PoolSession
	if err := db.First(&membership, "batch_id = ?", batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	var session model.SetupSession
	if err := db.First(&session, membership.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&session).Update("status", model.StatusPoolWaiting).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := poolstore.QueueBatchCleanup(context.Background(), db, batch.ID); err != nil {
		t.Fatal(err)
	}
	handler := &PoolHandler{db: db, setup: &SetupHandler{db: db}}
	if err := handler.cleanupSession(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&model.SetupSession{}, session.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("setup session lookup error = %v, want not found", err)
	}
	if err := db.First(&model.User{}, session.UserID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("dedicated user lookup error = %v, want not found", err)
	}
	if err := db.First(&model.PoolBatch{}, batch.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("pool batch lookup error = %v, want not found", err)
	}
}

func TestPoolBatchViewTreatsExpiredClaimAsLapsedWithoutWriting(t *testing.T) {
	db := testutil.Postgres(t)
	admin := model.Admin{UniqueId: "pool-read"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	admitted, err := poolstore.Admit(context.Background(), db, poolstore.Admission{
		AdminID: admin.ID, Key: "read", Digest: "read", Label: "Read", Count: 1, Capacity: 1,
		Session: model.SetupSession{
			Mode: model.ModeVtaOnly, Domain: "example.com", Subdomain: "vta-",
			VtaImage: "example/vta:test", DidHostingServerURL: "https://dids.example.com",
		},
		MemoryRequest: "32Mi", MemoryLimit: "64Mi",
	})
	if err != nil {
		t.Fatal(err)
	}
	session := admitted.Sessions[0]
	if err := db.Model(&session).Update("status", model.StatusPoolWaiting).Error; err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	claim := model.MobileConnection{
		ID: uuid.NewString(), SessionID: session.ID, VtaDid: "did:webvh:test",
		Operation: "provision_vta", Status: "open", ExpiresAt: past,
	}
	if err := db.Create(&claim).Error; err != nil {
		t.Fatal(err)
	}
	handler := &PoolHandler{db: db}
	view, err := handler.batchView(context.Background(), admitted.Batch.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Sessions) != 1 || view.Sessions[0].ClaimState != "lapsed" || !view.Sessions[0].Cleanable {
		t.Fatalf("expired claim view = %+v", view.Sessions)
	}
	var stored model.MobileConnection
	if err := db.First(&stored, "id = ?", claim.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "open" {
		t.Fatalf("GET changed claim status to %q, want open", stored.Status)
	}
}

func TestPoolBatchViewDoesNotOfferCleanupWhilePreparing(t *testing.T) {
	preparing := []string{
		model.StatusPoolCreating,
		"vta_setup_running",
		model.StatusPoolPublishing,
		model.StatusPoolPrepared,
	}
	for _, status := range preparing {
		view := buildPoolBatchView(
			model.PoolBatch{ID: 1, RequestedCount: 1},
			[]model.SetupSession{{ID: 1, UserID: 1, Status: status}},
			nil,
			true,
		)
		if len(view.Sessions) != 1 || view.Sessions[0].Cleanable {
			t.Fatalf("status %q cleanable view = %+v, want false", status, view.Sessions)
		}
	}
}

func TestPoolBatchViewPublicInventoryCountersAreExclusive(t *testing.T) {
	now := time.Now()
	sessions := []model.SetupSession{
		{ID: 1, Status: model.StatusPoolCreating},
		{ID: 2, Status: model.StatusPoolWaiting},
		{ID: 3, Status: model.StatusPoolWaiting},
		{ID: 4, Status: "provisioning", AdminDid: "did:key:confirmed"},
		{ID: 5, Status: "running", AdminDid: "did:key:released"},
		{ID: 6, Status: "failed"},
		{ID: 7, Status: "failed", AdminDid: "did:key:confirmed-failure"},
		{ID: 8, Status: "deleting"},
	}
	claims := map[uint]poolClaimSummary{
		3: {Status: "open"},
		4: {Status: "provisioning", ConfirmedAt: &now},
		5: {Status: "released", ConfirmedAt: &now},
		7: {Status: "failed", ConfirmedAt: &now},
	}
	view := buildPoolBatchView(model.PoolBatch{ID: 1, RequestedCount: len(sessions)}, sessions, claims, true)
	counts := view.Counts
	if counts.Creating != 1 || counts.Available != 1 || counts.Reserved != 1 ||
		counts.HandedOut != 3 || counts.FailedUnclaimed != 1 || counts.Deleting != 1 {
		t.Fatalf("public inventory counters = %+v", counts)
	}
	classified := counts.Creating + counts.Available + counts.Reserved + counts.HandedOut + counts.FailedUnclaimed + counts.Deleting
	if classified != counts.Total {
		t.Fatalf("classified = %d, total = %d", classified, counts.Total)
	}
	if counts.Activating != 1 || counts.Running != 1 || counts.Failed != 2 {
		t.Fatalf("legacy operational counters = %+v", counts)
	}
}

func TestPublicPoolClaimStoresTokenDigestAndHidesInternalFields(t *testing.T) {
	db := testutil.Postgres(t)
	platformOwner := model.User{UniqueId: systemAccountUniqueID}
	if err := db.Create(&platformOwner).Error; err != nil {
		t.Fatal(err)
	}
	verifiedAt := time.Now()
	platformDomain := model.Domain{
		UserID: platformOwner.ID, Domain: "platform.example.com", Kind: model.DomainKindPlatform, VerifiedAt: &verifiedAt,
	}
	if err := db.Create(&platformDomain).Error; err != nil {
		t.Fatal(err)
	}
	platformSession := model.SetupSession{
		UserID: platformOwner.ID, Status: "running", Mode: model.ModeFullStack,
		Domain: platformDomain.Domain, DomainType: model.DomainPlatform, DomainID: &platformDomain.ID,
		Subdomain: "vta", VtaName: "platform", VtcSubdomain: "vtc", VtcName: "fpdev",
		VtcDid: "did:webvh:platform.example.com:fpdev-vtc",
	}
	if err := db.Create(&platformSession).Error; err != nil {
		t.Fatal(err)
	}
	admin := model.Admin{UniqueId: "public-hdlr"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	admitted, err := poolstore.Admit(context.Background(), db, poolstore.Admission{
		AdminID: admin.ID, Key: "public-handler", Digest: "public-handler", Label: "Public", Purpose: model.PoolPurposePublicSignup,
		Count: 1, Capacity: 1,
		Session: model.SetupSession{
			Mode: model.ModeVtaOnly, Domain: "example.com", Subdomain: "vta-",
			VtaImage: "example/vta:test", DidHostingServerURL: "https://dids.example.com",
		},
		MemoryRequest: "32Mi", MemoryLimit: "64Mi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SetupSession{}).Where("id = ?", admitted.Sessions[0].ID).
		Updates(map[string]any{"status": model.StatusPoolWaiting, "vta_did": "did:webvh:example.com:public"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := poolstore.SetPublicSignup(context.Background(), db, admitted.Batch.ID, true); err != nil {
		t.Fatal(err)
	}
	linkHost, err := url.Parse("https://links.example.com")
	if err != nil {
		t.Fatal(err)
	}
	handler := &PoolHandler{db: db, mobile: &MobileConnectionHandler{
		db: db, enabled: true, farmDID: "did:webvh:example.com:farm", linkHost: linkHost,
	}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	public := router.Group("/api/v1/public/pool-claims", middleware.NoStore())
	public.POST("", handler.CreatePublicClaim)
	public.GET("/:request_id", handler.GetPublicClaim)

	rawToken := make([]byte, 32)
	for i := range rawToken {
		rawToken[i] = byte(i + 1)
	}
	token := base64.RawURLEncoding.EncodeToString(rawToken)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/public/pool-claims", nil)
	request.Header.Set("X-VTA-Claim-Token", token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	for _, forbidden := range []string{"batch_id", "session_id", "user_id", "vta_name", "candidate_did"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("public response leaked %q: %s", forbidden, response.Body.String())
		}
	}
	var body publicPoolClaimView
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Connection == nil || body.Connection.RequestID == "" || body.Connection.TriggerLink == "" {
		t.Fatalf("public response = %+v", body)
	}
	var stored model.MobileConnection
	if err := db.First(&stored, "id = ?", body.Connection.RequestID).Error; err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(rawToken)
	if string(stored.PublicAccessDigest) != string(wantDigest[:]) {
		t.Fatalf("stored digest = %x, want %x", stored.PublicAccessDigest, wantDigest)
	}

	wrongToken := make([]byte, 32)
	wrongToken[0] = 99
	request = httptest.NewRequest(http.MethodGet, "/api/v1/public/pool-claims/"+body.Connection.RequestID, nil)
	request.Header.Set("X-VTA-Claim-Token", base64.RawURLEncoding.EncodeToString(wrongToken))
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("mismatched token status = %d body=%s", response.Code, response.Body.String())
	}

	releasedAt := time.Now().UTC()
	if err := db.Model(&stored).Updates(map[string]any{"status": "released", "released_at": releasedAt}).Error; err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/public/pool-claims/"+body.Connection.RequestID, nil)
	request.Header.Set("X-VTA-Claim-Token", token)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("released claim status = %d body=%s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.PlatformVTC == nil || body.PlatformVTC.DID != platformSession.VtcDid || body.PlatformVTC.URL != "https://vtc.platform.example.com" {
		t.Fatalf("platform VTC = %+v", body.PlatformVTC)
	}
}
