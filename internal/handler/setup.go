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

	c.JSON(http.StatusOK, gin.H{
		"cloudflare": "ok",
		"ingress_ip": h.ingressIP,
	})
}

type createSetupRequest struct {
	Mode   string `json:"mode"   binding:"required,oneof=vta_only full_stack"`
	Domain string `json:"domain" binding:"required"`
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

	userID := c.MustGet(middleware.ContextUserID).(uint)
	subdomain := setup.GenerateSubdomain(h.appEnv)
	fqdn := subdomain + "." + req.Domain

	recordID, err := h.cf.CreateARecord(c.Request.Context(), fqdn, h.ingressIP)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to create DNS record: " + err.Error()})
		return
	}

	session := model.SetupSession{
		UserID:     userID,
		Mode:       req.Mode,
		Status:     "dns_provisioned",
		Domain:     req.Domain,
		Subdomain:  subdomain,
		CFRecordID: recordID,
	}
	if err := h.db.Create(&session).Error; err != nil {
		// DNS record created but session save failed — clean up to avoid orphan records.
		_ = h.cf.DeleteRecord(c.Request.Context(), recordID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist session"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":        session.ID,
		"subdomain": fqdn,
		"url":       "https://" + fqdn,
		"status":    session.Status,
	})
}

// GET /did/:subdomain/did.jsonl
// Public endpoint — serves the VTA DID log so did:webvh resolvers can fetch it.
// Uses the random subdomain as the identifier to prevent session enumeration.
func (h *SetupHandler) ServeDidLog(c *gin.Context) {
	subdomain := c.Param("subdomain")

	var session model.SetupSession
	if err := h.db.Select("did_log").Where("subdomain = ?", subdomain).First(&session).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if session.DidLog == "" {
		c.Status(http.StatusNotFound)
		return
	}

	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(session.DidLog))
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
