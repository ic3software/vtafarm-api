package handler

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/cloudflare"
	"github.com/ic3software/cipherportal-api/internal/ghcr"
	"github.com/ic3software/cipherportal-api/internal/k8s"
	"github.com/ic3software/cipherportal-api/internal/middleware"
	"github.com/ic3software/cipherportal-api/internal/model"
	"github.com/ic3software/cipherportal-api/internal/setup"
)

type SetupHandler struct {
	db           *gorm.DB
	cf           *cloudflare.Client
	appEnv       string
	ingressIP    string
	k8s          *k8s.Client
	orch         *setup.Orchestrator
	ghcr         *ghcr.Client // nil when not configured
	defaultImage string       // fallback when vta_image not provided in request
}

func NewSetupHandler(
	db *gorm.DB,
	cf *cloudflare.Client,
	appEnv, ingressIP string,
	k8sClient *k8s.Client,
	orch *setup.Orchestrator,
	ghcrClient *ghcr.Client,
	defaultImage string,
) *SetupHandler {
	return &SetupHandler{
		db:           db,
		cf:           cf,
		appEnv:       appEnv,
		ingressIP:    ingressIP,
		k8s:          k8sClient,
		orch:         orch,
		ghcr:         ghcrClient,
		defaultImage: defaultImage,
	}
}

func (h *SetupHandler) cfRequired(c *gin.Context) bool {
	if h.cf == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "cloudflare not configured"})
		return false
	}
	return true
}

// POST /api/v1/setup/validate
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

// GET /api/v1/setup/images
// Returns available VTA image tags fetched from GHCR.
// Falls back to the server default image if GHCR is not configured.
func (h *SetupHandler) Images(c *gin.Context) {
	type imageOption struct {
		Tag     string `json:"tag"`
		Image   string `json:"image"`
		Default bool   `json:"default,omitempty"`
	}

	// No GHCR configured — return default if available.
	if h.ghcr == nil {
		if h.defaultImage == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image source not configured"})
			return
		}
		tag := h.defaultImage
		if idx := len(h.defaultImage) - 1; idx >= 0 {
			for i := len(h.defaultImage) - 1; i >= 0; i-- {
				if h.defaultImage[i] == ':' {
					tag = h.defaultImage[i+1:]
					break
				}
			}
		}
		c.JSON(http.StatusOK, []imageOption{{Tag: tag, Image: h.defaultImage, Default: true}})
		return
	}

	tags, err := h.ghcr.ListTags(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch images: " + err.Error()})
		return
	}

	result := make([]imageOption, len(tags))
	for i, t := range tags {
		result[i] = imageOption{
			Tag:     t.Tag,
			Image:   t.Image,
			Default: t.Image == h.defaultImage,
		}
	}
	c.JSON(http.StatusOK, result)
}

type createSetupRequest struct {
	Mode        string `json:"mode"         binding:"required,oneof=vta_only full_stack"`
	Domain      string `json:"domain"       binding:"required"`
	VtaName     string `json:"vta_name"`
	MediatorDID string `json:"mediator_did" binding:"required"`
	VtaDidURL   string `json:"vta_did_url"  binding:"required,url"`
	VtaImage    string `json:"vta_image"`   // optional — full image URL, e.g. ghcr.io/ic3software/vta:0.5.0
	// Advanced — optional
	Portable         *bool `json:"portable"`
	PreRotationCount *int  `json:"pre_rotation_count"`
}

// POST /api/v1/setup
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

	// Resolve image: prefer explicit selection, fall back to server default.
	vtaImage := req.VtaImage
	if vtaImage == "" {
		vtaImage = h.defaultImage
	}
	if vtaImage == "" && h.orch != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "vta_image is required (VTA_IMAGE not configured on server)"})
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
		VtaName:          req.VtaName,
		MediatorDID:      req.MediatorDID,
		VtaDidURL:        req.VtaDidURL,
		VtaImage:         vtaImage,
		Portable:         portable,
		PreRotationCount: preRotationCount,
	}
	if err := h.db.Create(&session).Error; err != nil {
		_ = h.cf.DeleteRecord(c.Request.Context(), recordID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist session"})
		return
	}

	if h.orch != nil {
		h.orch.Start(session.ID)
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":        session.ID,
		"fqdn":      fqdn,
		"url":       "https://" + fqdn,
		"status":    session.Status,
		"vta_image": vtaImage,
	})
}

// GET /api/v1/setup
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
		VtaImage    string `json:"vta_image,omitempty"`
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
			VtaImage:    s.VtaImage,
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
		"vta_image":  session.VtaImage,
		"vta_did":    session.VtaDID,
		"created_at": session.CreatedAt,
		"updated_at": session.UpdatedAt,
	}
	if session.ErrorMsg != "" {
		resp["error_msg"] = session.ErrorMsg
	}
	c.JSON(http.StatusOK, resp)
}

// DELETE /api/v1/setup/:id
func (h *SetupHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	if h.orch != nil {
		h.orch.Cancel(session.ID)
	}

	if h.cf != nil && session.CFRecordID != "" {
		if err := h.cf.DeleteRecord(c.Request.Context(), session.CFRecordID); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to delete DNS record: " + err.Error()})
			return
		}
	}

	if h.k8s != nil {
		ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
		h.k8s.DeleteSetupResources(c.Request.Context(), ns, session.ID)
	}

	if err := h.db.Delete(&session).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete session"})
		return
	}

	c.Status(http.StatusNoContent)
}

// GET /api/v1/setup/:id/logs
func (h *SetupHandler) Logs(c *gin.Context) {
	id := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	if h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s not configured"})
		return
	}

	if session.Status == "dns_provisioned" {
		c.JSON(http.StatusConflict, gin.H{"error": "setup not started yet"})
		return
	}

	ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
	jobName := k8s.SetupJobName(session.ID)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	if session.Status == "vta_setup_running" {
		if err := h.k8s.StreamJobLogs(c.Request.Context(), ns, jobName, func(line string) {
			fmt.Fprintf(c.Writer, "data: %s\n\n", line)
			c.Writer.Flush()
		}); err != nil && c.Request.Context().Err() == nil {
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", err.Error())
			c.Writer.Flush()
		}
	} else {
		logs, err := h.k8s.JobLogs(c.Request.Context(), ns, jobName)
		if err != nil {
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", err.Error())
		} else {
			for _, line := range splitLines(logs) {
				fmt.Fprintf(c.Writer, "data: %s\n\n", line)
			}
		}
		c.Writer.Flush()
	}

	fmt.Fprintf(c.Writer, "event: done\ndata: stream ended\n\n")
	c.Writer.Flush()
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
