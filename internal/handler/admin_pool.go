package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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

func (h *PoolHandler) Start(ctx context.Context) {
	h.runner.Start(ctx)
}

type createPoolBatchRequest struct {
	Label    string `json:"label"`
	Count    int    `json:"count"`
	VtaImage string `json:"vta_image"`
}

type poolCounts struct {
	Total      int `json:"total"`
	Creating   int `json:"creating"`
	Waiting    int `json:"waiting"`
	Claiming   int `json:"claiming"`
	Activating int `json:"activating"`
	Running    int `json:"running"`
	Failed     int `json:"failed"`
	Deleting   int `json:"deleting"`
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
	ID             uint              `json:"id"`
	Label          string            `json:"label"`
	RequestedCount int               `json:"requested_count"`
	VtaImage       string            `json:"vta_image,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	Counts         poolCounts        `json:"counts"`
	Sessions       []poolSessionItem `json:"sessions,omitempty"`
}

type poolClaimView struct {
	BatchID    uint        `json:"batch_id"`
	SessionID  uint        `json:"session_id"`
	VtaName    string      `json:"vta_name"`
	Enabled    bool        `json:"enabled"`
	ServerTime time.Time   `json:"server_time"`
	Connection *mobileView `json:"connection"`
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
	if key == "" || len(key) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Idempotency-Key must be between 1 and 128 characters"})
		return
	}

	infra, _, reason, detail := h.setup.resolvePlatformInfra()
	if reason != "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": detail, "reason": reason})
		return
	}
	profiles, err := resourceprofile.Load(c.Request.Context(), h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load resource defaults"})
		return
	}
	profile := profiles[k8s.ComponentVTA]
	digestBody, _ := json.Marshal(request)
	digestBytes := sha256.Sum256(digestBody)
	adminID := c.MustGet(middleware.ContextUserID).(uint)
	batch, created, err := poolstore.Admit(c.Request.Context(), h.db, poolstore.Admission{
		AdminID:   adminID,
		Key:       key,
		Digest:    hex.EncodeToString(digestBytes[:]),
		Label:     request.Label,
		Count:     request.Count,
		Available: h.dashboard.AvailableVTAOnly,
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
	h.kickBatch(batch.ID)
	view, err := h.batchView(c.Request.Context(), batch.ID, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "batch was created but could not be loaded"})
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, view)
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
			c.JSON(http.StatusConflict, gin.H{"error": "one or more sessions are claimed, active, or no longer cleanable"})
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
	var sessions []model.SetupSession
	if err := h.db.WithContext(ctx).Table("setup_sessions s").Select("s.*").
		Joins("JOIN pool_sessions ps ON ps.session_id = s.id").
		Where("ps.batch_id = ?", batchID).Order("s.id").Find(&sessions).Error; err != nil {
		return nil, err
	}
	latest := make(map[uint]model.MobileConnection, len(sessions))
	if len(sessions) > 0 {
		ids := make([]uint, len(sessions))
		for i := range sessions {
			ids[i] = sessions[i].ID
		}
		now, err := connection.Now(h.db.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		if err := h.db.WithContext(ctx).Model(&model.MobileConnection{}).
			Where("session_id IN ? AND status = 'open' AND expires_at <= ?", ids, now).
			Update("status", "lapsed").Error; err != nil {
			return nil, err
		}
		if err := h.db.WithContext(ctx).Model(&model.MobileConnection{}).
			Where("session_id IN ? AND status = 'claimed' AND confirm_by <= ?", ids, now).
			Updates(map[string]any{"status": "lapsed", "admin_did": ""}).Error; err != nil {
			return nil, err
		}
		var connections []model.MobileConnection
		if err := h.db.WithContext(ctx).Raw(`
			SELECT DISTINCT ON (session_id) *
			FROM mobile_connections
			WHERE session_id IN ?
			ORDER BY session_id, created_at DESC, id DESC`, ids).Scan(&connections).Error; err != nil {
			return nil, err
		}
		for _, item := range connections {
			latest[item.SessionID] = item
		}
	}
	view := &poolBatchView{
		ID: batch.ID, Label: batch.Label, RequestedCount: batch.RequestedCount,
		CreatedAt: batch.CreatedAt, Sessions: []poolSessionItem{},
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
		item.Cleanable = !item.Claimed && !activeClaim && session.Status != "deleting"
		view.Counts.Total++
		switch {
		case session.Status == "deleting":
			view.Counts.Deleting++
		case session.Status == "running":
			view.Counts.Running++
		case session.Status == "failed":
			view.Counts.Failed++
		case item.Claimed || session.Status == "provisioning":
			view.Counts.Activating++
		case activeClaim:
			view.Counts.Claiming++
		case session.Status == model.StatusPoolWaiting:
			view.Counts.Waiting++
		default:
			view.Counts.Creating++
		}
		if includeSessions {
			view.Sessions = append(view.Sessions, item)
		}
	}
	return view, nil
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

func (h *PoolHandler) kickBatch(batchID uint) {
	var sessionIDs []uint
	if err := h.db.Model(&model.PoolSession{}).Where("batch_id = ?", batchID).
		Pluck("session_id", &sessionIDs).Error; err != nil {
		return
	}
	for _, sessionID := range sessionIDs {
		h.runner.Kick(sessionID)
	}
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
	return h.setup.cleanupPoolSession(ctx, sessionID)
}

func positiveID(value string) (uint, bool) {
	id, err := strconv.ParseUint(value, 10, 64)
	return uint(id), err == nil && id > 0
}
