package handler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	poolstore "github.com/ic3software/vtafarm-api/internal/pool"
	"github.com/ic3software/vtafarm-api/internal/resourceprofile"
	"github.com/ic3software/vtafarm-api/internal/setup"
)

type PoolHandler struct {
	db        *gorm.DB
	setup     *SetupHandler
	dashboard *DashboardHandler
	mobile    *MobileConnectionHandler
	runner    *poolstore.Runner
}

func NewPoolHandler(
	db *gorm.DB,
	setupHandler *SetupHandler,
	dashboard *DashboardHandler,
	mobile *MobileConnectionHandler,
) *PoolHandler {
	h := &PoolHandler{db: db, setup: setupHandler, dashboard: dashboard, mobile: mobile}
	var build func(context.Context, uint) error
	if setupHandler != nil && setupHandler.orch != nil {
		build = setupHandler.orch.BuildPool
	}
	h.runner = poolstore.NewRunner(db, h.prepareSession, build, h.cleanupSession)
	return h
}

func (h *PoolHandler) Start(ctx context.Context, reconcileExisting bool) {
	h.runner.Start(ctx, reconcileExisting)
}

type createPoolBatchRequest struct {
	Label    string `json:"label"`
	Count    int    `json:"count"`
	VtaImage string `json:"vta_image"`
	Purpose  string `json:"purpose"`
}

type poolCounts struct {
	Total           int `json:"total"`
	Creating        int `json:"creating"`
	Waiting         int `json:"waiting"`
	Claiming        int `json:"claiming"`
	Activating      int `json:"activating"`
	Running         int `json:"running"`
	Failed          int `json:"failed"`
	Deleting        int `json:"deleting"`
	Available       int `json:"available"`
	Reserved        int `json:"reserved"`
	HandedOut       int `json:"handed_out"`
	FailedUnclaimed int `json:"failed_unclaimed"`
}

type poolSessionItem struct {
	SessionID  uint   `json:"session_id"`
	UserID     uint   `json:"user_id"`
	VtaName    string `json:"vta_name"`
	Status     string `json:"status"`
	ErrorMsg   string `json:"error_msg,omitempty"`
	URL        string `json:"url"`
	Claimed    bool   `json:"claimed"`
	Cleanable  bool   `json:"cleanable"`
	ClaimState string `json:"claim_state,omitempty"`
}

type poolBatchView struct {
	ID                    uint              `json:"id"`
	Label                 string            `json:"label"`
	Purpose               string            `json:"purpose"`
	RequestedCount        int               `json:"requested_count"`
	VtaImage              string            `json:"vta_image,omitempty"`
	PublicSignupEnabledAt *time.Time        `json:"public_signup_enabled_at,omitempty"`
	CreatedAt             time.Time         `json:"created_at"`
	Counts                poolCounts        `json:"counts"`
	Sessions              []poolSessionItem `json:"sessions,omitempty"`
}

type poolClaimView struct {
	BatchID    uint        `json:"batch_id"`
	SessionID  uint        `json:"session_id"`
	VtaName    string      `json:"vta_name"`
	Enabled    bool        `json:"enabled"`
	ServerTime time.Time   `json:"server_time"`
	Connection *mobileView `json:"connection"`
}

type publicPoolClaimView struct {
	ServerTime  time.Time                   `json:"server_time"`
	Connection  *publicMobileConnectionView `json:"connection"`
	PlatformVTC *publicVTCView              `json:"platform_vtc,omitempty"`
}

type publicVTCView struct {
	DID string `json:"did"`
	URL string `json:"url"`
}

type publicMobileConnectionView struct {
	RequestID   string     `json:"request_id"`
	Status      string     `json:"status"`
	VtaDid      string     `json:"vta_did,omitempty"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ConfirmBy   *time.Time `json:"confirm_by,omitempty"`
	TriggerLink string     `json:"trigger_link,omitempty"`
	ClaimCheck  string     `json:"claim_check,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// CreateBatch — POST /api/v1/admin/pool-batches.
func (h *PoolHandler) CreateBatch(c *gin.Context) {
	if h.setup == nil || h.setup.cf == nil || h.setup.k8s == nil || h.setup.orch == nil ||
		h.setup.didHosting == nil || h.dashboard == nil || h.mobile == nil || !h.mobile.enabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "pooled VTA creation is not fully configured"})
		return
	}
	if h.setup.ingressIP == "" || h.setup.clusterDomain == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "cluster not configured: CLUSTER_INGRESS_IP and CLUSTER_DOMAIN must be set"})
		return
	}
	var request createPoolBatchRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	request.Label = strings.TrimSpace(request.Label)
	request.VtaImage = strings.TrimSpace(request.VtaImage)
	request.Purpose = strings.TrimSpace(request.Purpose)
	if request.Purpose == "" {
		request.Purpose = model.PoolPurposeAdminStation
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if request.Label == "" || len(request.Label) > 120 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "label must be between 1 and 120 characters"})
		return
	}
	if request.Count < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "count must be at least 1"})
		return
	}
	if request.VtaImage == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "vta_image is required"})
		return
	}
	if request.Purpose != model.PoolPurposeAdminStation && request.Purpose != model.PoolPurposePublicSignup {
		c.JSON(http.StatusBadRequest, gin.H{"error": "purpose must be admin_station or public_signup"})
		return
	}
	if key == "" || len(key) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Idempotency-Key must be between 1 and 128 characters"})
		return
	}

	digestBody, _ := json.Marshal(request)
	digestBytes := sha256.Sum256(digestBody)
	digest := hex.EncodeToString(digestBytes[:])
	adminID := c.MustGet(middleware.ContextUserID).(uint)
	if existing, lookupErr := poolstore.LookupBatch(c.Request.Context(), h.db, adminID, key, digest); lookupErr == nil {
		view, viewErr := h.batchView(c.Request.Context(), existing.ID, true)
		if viewErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "batch exists but could not be loaded"})
			return
		}
		c.JSON(http.StatusOK, view)
		return
	} else if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
		if errors.Is(lookupErr, poolstore.ErrConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "Idempotency-Key was already used with a different request"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check pool batch request"})
		return
	}

	var infra sharedInfra
	var reason, detail string
	var profiles resourceprofile.Profiles
	var profileErr error
	var clusterStats *k8s.ClusterStats
	var clusterErr error
	var reads sync.WaitGroup
	reads.Add(3)
	go func() {
		defer reads.Done()
		infra, _, reason, detail = h.setup.resolvePlatformInfra()
	}()
	go func() {
		defer reads.Done()
		profiles, profileErr = resourceprofile.Load(c.Request.Context(), h.db)
	}()
	go func() {
		defer reads.Done()
		clusterStats, clusterErr = h.dashboard.clusterCapacitySnapshot(c.Request.Context())
	}()
	reads.Wait()
	if reason != "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": detail, "reason": reason})
		return
	}
	if profileErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load resource defaults"})
		return
	}
	if clusterErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read cluster capacity"})
		return
	}
	profile := profiles[k8s.ComponentVTA]
	capacitySnapshot := h.dashboard.vtaOnlyCapacityFromSnapshot(clusterStats, profiles)
	admitted, err := poolstore.Admit(c.Request.Context(), h.db, poolstore.Admission{
		AdminID:  adminID,
		Key:      key,
		Digest:   digest,
		Label:    request.Label,
		Purpose:  request.Purpose,
		Count:    request.Count,
		Capacity: capacitySnapshot,
		Session: model.SetupSession{
			Mode:                 model.ModeVtaOnly,
			DomainType:           model.DomainManaged,
			Domain:               h.setup.clusterDomain,
			Subdomain:            setup.EnvPrefix(h.setup.appEnv) + "vta-",
			MediatorDid:          infra.MediatorDid,
			DidHostingServerURL:  strings.TrimSuffix(infra.ServerURL, "/"),
			DidHostingControlURL: infra.ControlURL,
			DIDHostingDid:        infra.DaemonDid,
			ConnectionSource:     model.ConnectionPlatform,
			VtaImage:             request.VtaImage,
			Portable:             true,
			PreRotationCount:     1,
		},
		MemoryRequest: profile.MemoryRequest,
		MemoryLimit:   profile.MemoryLimit,
	})
	if err != nil {
		var capacityErr *poolstore.CapacityError
		switch {
		case errors.As(err, &capacityErr):
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":  fmt.Sprintf("the cluster currently has capacity for %d additional VTA-only sessions", capacityErr.Available),
				"reason": "insufficient_capacity", "available": capacityErr.Available,
			})
		case errors.Is(err, poolstore.ErrConflict):
			c.JSON(http.StatusConflict, gin.H{"error": "Idempotency-Key was already used with a different request"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create pool batch"})
		}
		return
	}
	for i := range admitted.Sessions {
		h.runner.Kick(admitted.Sessions[i].ID)
	}
	status := http.StatusOK
	var view *poolBatchView
	if admitted.Created {
		status = http.StatusCreated
		view = buildPoolBatchView(*admitted.Batch, admitted.Sessions, nil, true)
	} else {
		view, err = h.batchView(c.Request.Context(), admitted.Batch.ID, true)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "batch exists but could not be loaded"})
			return
		}
	}
	c.JSON(status, view)
}

type addPoolInventoryRequest struct {
	Count int `json:"count"`
}

func (h *PoolHandler) AddInventory(c *gin.Context) {
	if h.setup == nil || h.setup.cf == nil || h.setup.k8s == nil || h.setup.orch == nil ||
		h.setup.didHosting == nil || h.dashboard == nil || h.mobile == nil || !h.mobile.enabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "pooled VTA creation is not fully configured"})
		return
	}
	batchID, ok := positiveID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pool batch ID"})
		return
	}
	var request addPoolInventoryRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if request.Count < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "count must be at least 1"})
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Idempotency-Key must be between 1 and 128 characters"})
		return
	}

	var source model.SetupSession
	if err := h.db.WithContext(c.Request.Context()).Table("setup_sessions s").
		Select("s.*").Joins("JOIN pool_sessions ps ON ps.session_id = s.id").
		Where("ps.batch_id = ?", batchID).Order("s.id").Limit(1).Scan(&source).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load pool batch template"})
		return
	}
	if source.ID == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "pool batch not found"})
		return
	}
	template := model.SetupSession{
		Mode:                 model.ModeVtaOnly,
		DomainType:           source.DomainType,
		Domain:               source.Domain,
		Subdomain:            strings.TrimSuffix(source.Subdomain, source.VtaName),
		MediatorDid:          source.MediatorDid,
		DidHostingServerURL:  source.DidHostingServerURL,
		DidHostingControlURL: source.DidHostingControlURL,
		DIDHostingDid:        source.DIDHostingDid,
		ConnectionSource:     source.ConnectionSource,
		VtaImage:             source.VtaImage,
		Portable:             source.Portable,
		PreRotationCount:     source.PreRotationCount,
	}
	profiles, profileErr := resourceprofile.Load(c.Request.Context(), h.db)
	clusterStats, clusterErr := h.dashboard.clusterCapacitySnapshot(c.Request.Context())
	if profileErr != nil || clusterErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read cluster capacity"})
		return
	}
	profile := profiles[k8s.ComponentVTA]
	capacitySnapshot := h.dashboard.vtaOnlyCapacityFromSnapshot(clusterStats, profiles)
	digestBody, _ := json.Marshal(request)
	digestBytes := sha256.Sum256(append([]byte(strconv.FormatUint(uint64(batchID), 10)+":"), digestBody...))
	result, err := poolstore.AddInventory(c.Request.Context(), h.db, poolstore.Addition{
		AdminID: c.MustGet(middleware.ContextUserID).(uint), BatchID: batchID,
		Key: key, Digest: hex.EncodeToString(digestBytes[:]), Count: request.Count,
		Capacity: capacitySnapshot, Session: template,
		MemoryRequest: profile.MemoryRequest, MemoryLimit: profile.MemoryLimit,
	})
	if err != nil {
		var capacityErr *poolstore.CapacityError
		switch {
		case errors.As(err, &capacityErr):
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":  fmt.Sprintf("the cluster currently has capacity for %d additional VTA-only sessions", capacityErr.Available),
				"reason": "insufficient_capacity", "available": capacityErr.Available,
			})
		case errors.Is(err, poolstore.ErrConflict):
			c.JSON(http.StatusConflict, gin.H{"error": "pool is being cleaned or Idempotency-Key was reused"})
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "pool batch not found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add pool inventory"})
		}
		return
	}
	for i := range result.Sessions {
		h.runner.Kick(result.Sessions[i].ID)
	}
	view, viewErr := h.batchView(c.Request.Context(), result.Batch.ID, true)
	if viewErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "inventory was added but the batch could not be loaded"})
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	c.JSON(status, view)
}

func (h *PoolHandler) EnablePublicSignup(c *gin.Context) {
	h.setPublicSignup(c, true)
}

func (h *PoolHandler) DisablePublicSignup(c *gin.Context) {
	h.setPublicSignup(c, false)
}

func (h *PoolHandler) setPublicSignup(c *gin.Context, enabled bool) {
	batchID, ok := positiveID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pool batch ID"})
		return
	}
	batch, err := poolstore.SetPublicSignup(c.Request.Context(), h.db, batchID, enabled)
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "pool batch not found"})
		case errors.Is(err, poolstore.ErrConflict):
			c.JSON(http.StatusConflict, gin.H{"error": "only an active public-signup batch can be enabled or paused"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update public signup"})
		}
		return
	}
	view, err := h.batchView(c.Request.Context(), batch.ID, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "public signup changed but the batch could not be loaded"})
		return
	}
	c.JSON(http.StatusOK, view)
}

func (h *PoolHandler) ListBatches(c *gin.Context) {
	var batches []model.PoolBatch
	if err := h.db.WithContext(c.Request.Context()).Order("id DESC").Find(&batches).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list pool batches"})
		return
	}
	views := make([]poolBatchView, 0, len(batches))
	for _, batch := range batches {
		view, err := h.batchView(c.Request.Context(), batch.ID, false)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load pool batches"})
			return
		}
		views = append(views, *view)
	}
	c.JSON(http.StatusOK, views)
}

func (h *PoolHandler) GetBatch(c *gin.Context) {
	batchID, ok := positiveID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pool batch ID"})
		return
	}
	view, err := h.batchView(c.Request.Context(), batchID, true)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "pool batch not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load pool batch"})
		return
	}
	c.JSON(http.StatusOK, view)
}

func (h *PoolHandler) Reserve(c *gin.Context) {
	if h.mobile == nil || !h.mobile.enabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "VTA claim is not available", "reason": "vta_claim_disabled"})
		return
	}
	batchID, ok := positiveID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pool batch ID"})
		return
	}
	var exists int64
	if err := h.db.Model(&model.PoolBatch{}).Where("id = ?", batchID).Count(&exists).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load pool batch"})
		return
	}
	if exists == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "pool batch not found"})
		return
	}
	claim, err := poolstore.Reserve(c.Request.Context(), h.db, batchID)
	if err != nil {
		if errors.Is(err, poolstore.ErrEmpty) {
			c.JSON(http.StatusConflict, gin.H{"error": "no waiting VTA is available in this batch", "reason": "pool_empty"})
			return
		}
		writeConnectionError(c, err)
		return
	}
	h.writeClaim(c, claim.ID)
}

func publicClaimDigest(c *gin.Context) ([]byte, bool) {
	token := strings.TrimSpace(c.GetHeader("X-VTA-Claim-Token"))
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return nil, false
	}
	digest := sha256.Sum256(raw)
	return digest[:], true
}

func (h *PoolHandler) authorizePublicClaim(c *gin.Context) (*model.MobileConnection, bool) {
	digest, ok := publicClaimDigest(c)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "public claim not found", "reason": "public_claim_not_found"})
		return nil, false
	}
	var claim model.MobileConnection
	if err := h.db.WithContext(c.Request.Context()).First(&claim, "id = ?", c.Param("request_id")).Error; err != nil ||
		len(claim.PublicAccessDigest) != len(digest) || subtle.ConstantTimeCompare(claim.PublicAccessDigest, digest) != 1 {
		c.JSON(http.StatusNotFound, gin.H{"error": "public claim not found", "reason": "public_claim_not_found"})
		return nil, false
	}
	return &claim, true
}

func (h *PoolHandler) CreatePublicClaim(c *gin.Context) {
	if h.mobile == nil || !h.mobile.enabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "VTA reservation is currently unavailable. Please contact an administrator.", "reason": "public_signup_disabled"})
		return
	}
	digest, ok := publicClaimDigest(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-VTA-Claim-Token must contain 32 random bytes encoded as base64url", "reason": "invalid_claim_token"})
		return
	}
	claim, created, err := poolstore.ReservePublic(c.Request.Context(), h.db, digest)
	if err != nil {
		switch {
		case errors.Is(err, poolstore.ErrPublicDisabled):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "VTA reservation is currently unavailable. Please contact an administrator.", "reason": "public_signup_disabled"})
		case errors.Is(err, poolstore.ErrEmpty):
			c.JSON(http.StatusConflict, gin.H{"error": "No VTA is available right now. Please try again later or contact us on Discord.", "reason": "public_pool_empty"})
		default:
			writeConnectionError(c, err)
		}
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	h.writePublicClaim(c, claim.ID, status)
}

func (h *PoolHandler) GetPublicClaim(c *gin.Context) {
	claim, ok := h.authorizePublicClaim(c)
	if !ok {
		return
	}
	h.writePublicClaim(c, claim.ID, http.StatusOK)
}

func (h *PoolHandler) ConfirmPublicClaim(c *gin.Context) {
	claim, ok := h.authorizePublicClaim(c)
	if !ok {
		return
	}
	confirmed, err := poolstore.Confirm(c.Request.Context(), h.db, claim.ID)
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	if h.setup != nil && h.setup.orch != nil {
		h.setup.orch.KickProvision(confirmed.SessionID)
	}
	h.writePublicClaim(c, confirmed.ID, http.StatusOK)
}

func (h *PoolHandler) RejectPublicClaim(c *gin.Context) {
	claim, ok := h.authorizePublicClaim(c)
	if !ok {
		return
	}
	if err := poolstore.Reject(c.Request.Context(), h.db, claim.ID); err != nil {
		writeConnectionError(c, err)
		return
	}
	h.writePublicClaim(c, claim.ID, http.StatusOK)
}

func (h *PoolHandler) writePublicClaim(c *gin.Context, requestID string, status int) {
	var claim model.MobileConnection
	if err := h.db.WithContext(c.Request.Context()).First(&claim, "id = ?", requestID).Error; err != nil {
		writeConnectionError(c, err)
		return
	}
	var session model.SetupSession
	if err := h.db.WithContext(c.Request.Context()).First(&session, claim.SessionID).Error; err != nil {
		writeConnectionError(c, err)
		return
	}
	if err := connection.Reconcile(c.Request.Context(), h.db, &claim, &session); err != nil {
		writeConnectionError(c, err)
		return
	}
	now, err := connection.Now(h.db.WithContext(c.Request.Context()))
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	full := h.mobile.view(&claim)
	view := &publicMobileConnectionView{
		RequestID: full.RequestID, Status: full.Status, ExpiresAt: full.ExpiresAt,
		ConfirmBy: full.ConfirmBy, TriggerLink: full.TriggerLink,
		ClaimCheck: full.ClaimCheck, Error: full.Error,
	}
	var platformVTC *publicVTCView
	if full.Status == "released" {
		view.VtaDid = full.VtaDid
		platformVTC, err = h.publicSignupPlatformVTC(c.Request.Context())
		if err != nil {
			writeConnectionError(c, err)
			return
		}
	}
	c.JSON(status, publicPoolClaimView{ServerTime: now, Connection: view, PlatformVTC: platformVTC})
}

// publicSignupPlatformVTC returns only the public identity of this Farm's
// running platform community. The environment-specific setup row is the source
// of truth; frontend configuration must not point dev or staging at production.
func (h *PoolHandler) publicSignupPlatformVTC(ctx context.Context) (*publicVTCView, error) {
	var session model.SetupSession
	err := h.db.WithContext(ctx).
		Joins("JOIN domains ON domains.id = setup_sessions.domain_id").
		Where("domains.kind = ? AND setup_sessions.status = ?", model.DomainKindPlatform, "running").
		First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(session.VtcDid) == "" || strings.TrimSpace(session.VtcSubdomain) == "" || strings.TrimSpace(session.Domain) == "" {
		return nil, nil
	}
	return &publicVTCView{DID: session.VtcDid, URL: "https://" + session.VtcFQDN()}, nil
}

func (h *PoolHandler) GetClaim(c *gin.Context) {
	h.writeClaim(c, c.Param("request_id"))
}

func (h *PoolHandler) ConfirmClaim(c *gin.Context) {
	claim, err := poolstore.Confirm(c.Request.Context(), h.db, c.Param("request_id"))
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	if h.setup != nil && h.setup.orch != nil {
		h.setup.orch.KickProvision(claim.SessionID)
	}
	h.writeClaim(c, claim.ID)
}

func (h *PoolHandler) RejectClaim(c *gin.Context) {
	requestID := c.Param("request_id")
	if err := poolstore.Reject(c.Request.Context(), h.db, requestID); err != nil {
		writeConnectionError(c, err)
		return
	}
	h.writeClaim(c, requestID)
}

func (h *PoolHandler) Cleanup(c *gin.Context) {
	var request struct {
		SessionIDs []uint `json:"session_ids"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := poolstore.QueueCleanup(c.Request.Context(), h.db, request.SessionIDs); err != nil {
		if errors.Is(err, poolstore.ErrConflict) || errors.Is(err, connection.ErrConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "one or more sessions are still preparing, claimed, active, or no longer cleanable"})
			return
		}
		writeConnectionError(c, err)
		return
	}
	for _, sessionID := range request.SessionIDs {
		h.runner.Kick(sessionID)
	}
	c.JSON(http.StatusAccepted, gin.H{"session_ids": request.SessionIDs, "status": "deleting"})
}

func (h *PoolHandler) CleanupBatch(c *gin.Context) {
	batchID, ok := positiveID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pool batch ID"})
		return
	}
	sessionIDs, err := poolstore.QueueBatchCleanup(c.Request.Context(), h.db, batchID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "pool batch not found"})
		return
	}
	if errors.Is(err, poolstore.ErrConflict) || errors.Is(err, connection.ErrConflict) {
		c.JSON(http.StatusConflict, gin.H{"error": "one or more sessions are still preparing, claimed, active, or no longer cleanable"})
		return
	}
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	if len(sessionIDs) == 0 {
		c.Status(http.StatusNoContent)
		return
	}
	for _, sessionID := range sessionIDs {
		h.runner.Kick(sessionID)
	}
	c.JSON(http.StatusAccepted, gin.H{"batch_id": batchID, "session_ids": sessionIDs, "status": "deleting"})
}

func (h *PoolHandler) Retry(c *gin.Context) {
	sessionID, ok := positiveID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pool session ID"})
		return
	}
	if err := poolstore.RetryBuild(c.Request.Context(), h.db, sessionID); err != nil {
		if errors.Is(err, poolstore.ErrConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "only an unclaimed failed pool session can be retried"})
			return
		}
		writeConnectionError(c, err)
		return
	}
	h.runner.Kick(sessionID)
	c.JSON(http.StatusAccepted, gin.H{"session_id": sessionID, "status": model.StatusPoolCreating})
}

func (h *PoolHandler) batchView(ctx context.Context, batchID uint, includeSessions bool) (*poolBatchView, error) {
	var batch model.PoolBatch
	if err := h.db.WithContext(ctx).First(&batch, batchID).Error; err != nil {
		return nil, err
	}
	type sessionRow struct {
		model.SetupSession `gorm:"embedded"`
		ClaimStatus        *string    `gorm:"column:claim_status"`
		ClaimConfirmedAt   *time.Time `gorm:"column:claim_confirmed_at"`
		ClaimExpiresAt     *time.Time `gorm:"column:claim_expires_at"`
		ClaimConfirmBy     *time.Time `gorm:"column:claim_confirm_by"`
		ReadAt             time.Time  `gorm:"column:read_at"`
	}
	var rows []sessionRow
	if err := h.db.WithContext(ctx).Raw(`
		SELECT s.*,
		       claim.status AS claim_status,
		       claim.confirmed_at AS claim_confirmed_at,
		       claim.expires_at AS claim_expires_at,
		       claim.confirm_by AS claim_confirm_by,
		       clock_timestamp() AS read_at
		FROM setup_sessions s
		JOIN pool_sessions ps ON ps.session_id = s.id
		LEFT JOIN LATERAL (
			SELECT mc.status, mc.confirmed_at, mc.expires_at, mc.confirm_by
			FROM mobile_connections mc
			WHERE mc.session_id = s.id
			ORDER BY mc.created_at DESC, mc.id DESC
			LIMIT 1
		) claim ON true
		WHERE ps.batch_id = ?
		ORDER BY s.id`, batchID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	sessions := make([]model.SetupSession, len(rows))
	latest := make(map[uint]poolClaimSummary, len(rows))
	for i := range rows {
		sessions[i] = rows[i].SetupSession
		if rows[i].ClaimStatus == nil {
			continue
		}
		status := *rows[i].ClaimStatus
		if status == "open" && rows[i].ClaimExpiresAt != nil && !rows[i].ClaimExpiresAt.After(rows[i].ReadAt) {
			status = "lapsed"
		}
		if status == "claimed" && rows[i].ClaimConfirmBy != nil && !rows[i].ClaimConfirmBy.After(rows[i].ReadAt) {
			status = "lapsed"
		}
		latest[rows[i].ID] = poolClaimSummary{Status: status, ConfirmedAt: rows[i].ClaimConfirmedAt}
	}
	return buildPoolBatchView(batch, sessions, latest, includeSessions), nil
}

type poolClaimSummary struct {
	Status      string
	ConfirmedAt *time.Time
}

func buildPoolBatchView(batch model.PoolBatch, sessions []model.SetupSession, latest map[uint]poolClaimSummary, includeSessions bool) *poolBatchView {
	view := &poolBatchView{
		ID: batch.ID, Label: batch.Label, Purpose: batch.Purpose, RequestedCount: batch.RequestedCount,
		PublicSignupEnabledAt: batch.PublicSignupEnabledAt,
		CreatedAt:             batch.CreatedAt, Sessions: []poolSessionItem{},
	}
	if len(sessions) > 0 {
		view.VtaImage = sessions[0].VtaImage
	}
	for i := range sessions {
		session := &sessions[i]
		current, hasClaim := latest[session.ID]
		item := poolSessionItem{
			SessionID: session.ID, UserID: session.UserID, VtaName: session.VtaName,
			Status: session.Status, ErrorMsg: session.ErrorMsg, URL: session.PublicURL(),
		}
		if hasClaim {
			item.ClaimState = current.Status
		}
		item.Claimed = session.AdminDid != "" || hasClaim && current.ConfirmedAt != nil
		activeClaim := hasClaim && (current.Status == "open" || current.Status == "claimed" || current.Status == "confirmed" || current.Status == "provisioning")
		cleanupReady := session.Status == model.StatusPoolWaiting || session.Status == "failed"
		item.Cleanable = cleanupReady && !item.Claimed && !activeClaim
		view.Counts.Total++
		if session.Status == "failed" {
			view.Counts.Failed++
		}
		switch {
		case session.Status == "deleting":
			view.Counts.Deleting++
		case item.Claimed:
			view.Counts.HandedOut++
			switch {
			case session.Status == "running":
				view.Counts.Running++
			case session.Status == "provisioning" || current.Status == "confirmed" || current.Status == "provisioning":
				view.Counts.Activating++
			}
		case activeClaim:
			view.Counts.Claiming++
			view.Counts.Reserved++
		case session.Status == "failed":
			view.Counts.FailedUnclaimed++
		case session.Status == model.StatusPoolWaiting:
			view.Counts.Waiting++
			view.Counts.Available++
		default:
			view.Counts.Creating++
		}
		if includeSessions {
			view.Sessions = append(view.Sessions, item)
		}
	}
	return view
}

func (h *PoolHandler) writeClaim(c *gin.Context, requestID string) {
	var claim model.MobileConnection
	if err := h.db.WithContext(c.Request.Context()).First(&claim, "id = ?", requestID).Error; err != nil {
		writeConnectionError(c, err)
		return
	}
	var membership model.PoolSession
	if err := h.db.WithContext(c.Request.Context()).First(&membership, "session_id = ?", claim.SessionID).Error; err != nil {
		writeConnectionError(c, err)
		return
	}
	var session model.SetupSession
	if err := h.db.WithContext(c.Request.Context()).First(&session, claim.SessionID).Error; err != nil {
		writeConnectionError(c, err)
		return
	}
	if err := connection.Reconcile(c.Request.Context(), h.db, &claim, &session); err != nil {
		writeConnectionError(c, err)
		return
	}
	now, err := connection.Now(h.db.WithContext(c.Request.Context()))
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	c.JSON(http.StatusOK, poolClaimView{
		BatchID: membership.BatchID, SessionID: session.ID, VtaName: session.VtaName,
		Enabled: h.mobile != nil && h.mobile.enabled, ServerTime: now,
		Connection: h.mobile.view(&claim),
	})
}

func (h *PoolHandler) prepareSession(ctx context.Context, sessionID uint) error {
	if h.setup == nil || h.setup.cf == nil {
		return errors.New("cloudflare is not configured")
	}
	var session model.SetupSession
	if err := h.db.WithContext(ctx).First(&session, sessionID).Error; err != nil {
		return err
	}
	if !poolstore.IsSession(h.db.WithContext(ctx), sessionID) || session.Status == "deleting" {
		return poolstore.ErrConflict
	}
	if session.CFRecordID != "" {
		return nil
	}
	recordID, err := h.setup.cf.EnsureARecord(ctx, session.FQDN(), h.setup.ingressIP)
	if err != nil {
		return fmt.Errorf("failed to create DNS record: %w", err)
	}
	updated := h.db.WithContext(ctx).Model(&model.SetupSession{}).
		Where("id = ? AND cf_record_id = '' AND status <> 'deleting'", sessionID).
		Update("cf_record_id", recordID)
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected == 0 {
		var current model.SetupSession
		if err := h.db.WithContext(ctx).Select("cf_record_id", "status").First(&current, sessionID).Error; err != nil {
			return err
		}
		if current.Status == "deleting" {
			return poolstore.ErrConflict
		}
		if current.CFRecordID == "" {
			return errors.New("DNS record ownership changed during pool preparation")
		}
	}
	return nil
}

func (h *PoolHandler) cleanupSession(ctx context.Context, sessionID uint) error {
	if h.setup == nil {
		return errors.New("setup cleanup is not configured")
	}
	var membership model.PoolSession
	if err := h.db.WithContext(ctx).First(&membership, "session_id = ?", sessionID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if err := h.setup.cleanupPoolSession(ctx, sessionID); err != nil {
		return err
	}
	return poolstore.FinalizeBatchCleanup(ctx, h.db, membership.BatchID)
}

func positiveID(value string) (uint, bool) {
	id, err := strconv.ParseUint(value, 10, 64)
	return uint(id), err == nil && id > 0
}
