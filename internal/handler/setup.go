package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/cloudflare"
	"github.com/ic3software/cipherportal-api/internal/middleware"
	"github.com/ic3software/cipherportal-api/internal/model"
	"github.com/ic3software/cipherportal-api/internal/setup"
)

type SetupHandler struct {
	db        *gorm.DB
	cf        *cloudflare.Client
	appEnv    string
	ingressIP string
}

func NewSetupHandler(db *gorm.DB, cf *cloudflare.Client, appEnv, ingressIP string) *SetupHandler {
	return &SetupHandler{db: db, cf: cf, appEnv: appEnv, ingressIP: ingressIP}
}

func (h *SetupHandler) cfRequired(c *gin.Context) bool {
	if h.cf == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "cloudflare not configured"})
		return false
	}
	return true
}

// POST /api/v1/setup/validate
// Verifies Cloudflare connectivity and that CLUSTER_INGRESS_IP is set.
func (h *SetupHandler) Validate(c *gin.Context) {
	if !h.cfRequired(c) {
		return
	}

	if err := h.cf.VerifyZone(c.Request.Context()); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "cloudflare connectivity failed: " + err.Error()})
		return
	}

	if h.ingressIP == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "cluster not configured: CLUSTER_INGRESS_IP not set"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"cloudflare": "ok"})
}

type createSetupRequest struct {
	Mode             string `json:"mode"         binding:"required,oneof=vta_only full_stack"`
	Domain           string `json:"domain"       binding:"required"`
	VtaName          string `json:"vta_name"`
	MediatorDID      string `json:"mediator_did" binding:"required"`
	VtaDidURL        string `json:"vta_did_url"  binding:"required,url"`
	// Advanced — optional
	Portable         *bool  `json:"portable"`
	PreRotationCount *int   `json:"pre_rotation_count"`
}

// POST /api/v1/setup
// Generates a random subdomain, creates a Cloudflare DNS A record, and stores the session.
func (h *SetupHandler) Create(c *gin.Context) {
	if !h.cfRequired(c) {
		return
	}

	var req createSetupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if h.ingressIP == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "cluster not configured: CLUSTER_INGRESS_IP not set"})
		return
	}

	if req.VtaName == "" {
		req.VtaName = "personal-vta"
	}
	portable := true
	if req.Portable != nil {
		portable = *req.Portable
	}
	preRotationCount := 1
	if req.PreRotationCount != nil {
		preRotationCount = *req.PreRotationCount
	}

	userID := c.MustGet(middleware.ContextUserID).(uint)
	subdomain := setup.GenerateSubdomain(h.appEnv)
	fqdn := subdomain + "." + req.Domain

	recordID, err := h.cf.CreateARecord(c.Request.Context(), fqdn, h.ingressIP)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to create DNS record: " + err.Error()})
		return
	}

	session := model.SetupSession{
		UserID:           userID,
		Mode:             req.Mode,
		Status:           "dns_provisioned",
		Domain:           req.Domain,
		Subdomain:        subdomain,
		CFRecordID:       recordID,
		VtaName:     req.VtaName,
		MediatorDID: req.MediatorDID,
		VtaDidURL:   req.VtaDidURL,
		Portable:         portable,
		PreRotationCount: preRotationCount,
	}
	if err := h.db.Create(&session).Error; err != nil {
		// DNS record created but session save failed — clean up to avoid orphan records.
		_ = h.cf.DeleteRecord(c.Request.Context(), recordID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist session"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":     session.ID,
		"fqdn":   fqdn,
		"url":    "https://" + fqdn,
		"status": session.Status,
	})
}

// GET /api/v1/setup
// Lists all setup sessions owned by the requesting user.
func (h *SetupHandler) List(c *gin.Context) {
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var sessions []model.SetupSession
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&sessions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sessions"})
		return
	}

	type item struct {
		ID          uint   `json:"id"`
		Status      string `json:"status"`
		Mode        string `json:"mode"`
		FQDN        string `json:"fqdn"`
		URL         string `json:"url"`
		VtaName     string `json:"vta_name"`
		MediatorDID string `json:"mediator_did"`
		VtaDidURL   string `json:"vta_did_url"`
		VtaDID      string `json:"vta_did,omitempty"`
		ErrorMsg    string `json:"error_msg,omitempty"`
		CreatedAt   any    `json:"created_at"`
	}

	result := make([]item, len(sessions))
	for i, s := range sessions {
		result[i] = item{
			ID:          s.ID,
			Status:      s.Status,
			Mode:        s.Mode,
			FQDN:        s.FQDN(),
			URL:         s.PublicURL(),
			VtaName:     s.VtaName,
			MediatorDID: s.MediatorDID,
			VtaDidURL:   s.VtaDidURL,
			VtaDID:      s.VtaDID,
			ErrorMsg:    s.ErrorMsg,
			CreatedAt:   s.CreatedAt,
		}
	}
	c.JSON(http.StatusOK, result)
}

// GET /api/v1/setup/:id
// Returns the current state of a setup session owned by the requesting user.
func (h *SetupHandler) Get(c *gin.Context) {
	id := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	resp := gin.H{
		"id":         session.ID,
		"status":     session.Status,
		"mode":       session.Mode,
		"fqdn":       session.FQDN(),
		"url":        "https://" + session.FQDN(),
		"created_at": session.CreatedAt,
		"updated_at": session.UpdatedAt,
	}
	if session.ErrorMsg != "" {
		resp["error_msg"] = session.ErrorMsg
	}
	c.JSON(http.StatusOK, resp)
}

// DELETE /api/v1/setup/:id
// Removes the Cloudflare DNS record and soft-deletes the session.
func (h *SetupHandler) Delete(c *gin.Context) {
	id := c.Param("id")

	var session model.SetupSession
	if err := h.db.First(&session, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	if h.cf != nil && session.CFRecordID != "" {
		if err := h.cf.DeleteRecord(c.Request.Context(), session.CFRecordID); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to delete DNS record: " + err.Error()})
			return
		}
	}

	if err := h.db.Delete(&session).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete session"})
		return
	}

	c.Status(http.StatusNoContent)
}
