package handler

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/cloudflare"
	"github.com/ic3software/cipherportal-api/internal/didhosting"
	"github.com/ic3software/cipherportal-api/internal/ghcr"
	"github.com/ic3software/cipherportal-api/internal/k8s"
	"github.com/ic3software/cipherportal-api/internal/middleware"
	"github.com/ic3software/cipherportal-api/internal/model"
	"github.com/ic3software/cipherportal-api/internal/setup"
)

type SetupHandler struct {
	db             *gorm.DB
	cf             *cloudflare.Client
	appEnv         string
	ingressIP      string
	clusterDomain  string
	mediatorDid    string
	didHostingBase string // DID_HOSTING_SERVER_URL — public server URL used to build vta_did_url
	didHosting     *didhosting.Client // nil when not configured
	k8s            *k8s.Client
	orch           *setup.Orchestrator
	ghcr           *ghcr.Client // nil when not configured
}

func NewSetupHandler(
	db *gorm.DB,
	cf *cloudflare.Client,
	appEnv, ingressIP, clusterDomain, mediatorDid, didHostingBase string,
	dhClient *didhosting.Client,
	k8sClient *k8s.Client,
	orch *setup.Orchestrator,
	ghcrClient *ghcr.Client,
) *SetupHandler {
	return &SetupHandler{
		db:             db,
		cf:             cf,
		appEnv:         appEnv,
		ingressIP:      ingressIP,
		clusterDomain:  clusterDomain,
		mediatorDid:    mediatorDid,
		didHostingBase: didHostingBase,
		didHosting:     dhClient,
		k8s:            k8sClient,
		orch:           orch,
		ghcr:           ghcrClient,
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
func (h *SetupHandler) Images(c *gin.Context) {
	type imageOption struct {
		Tag   string `json:"tag"`
		Image string `json:"image"`
	}

	if h.ghcr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image source not configured"})
		return
	}

	tags, err := h.ghcr.ListTags(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch images: " + err.Error()})
		return
	}

	result := make([]imageOption, len(tags))
	for i, t := range tags {
		result[i] = imageOption{Tag: t.Tag, Image: t.Image}
	}
	c.JSON(http.StatusOK, result)
}

type createSetupRequest struct {
	Mode     string `json:"mode"      binding:"required,oneof=vta_only full_stack"`
	VtaName  string `json:"vta_name"`
	VtaImage string `json:"vta_image" binding:"required"`
	// Optional — if set, Phase 2 (import-did + Deployment) starts automatically after Phase 1.
	AdminDid string `json:"admin_did"`
	// Advanced — optional, defaults: portable=true, pre_rotation_count=1
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

	if h.ingressIP == "" || h.clusterDomain == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "cluster not configured: CLUSTER_INGRESS_IP and CLUSTER_DOMAIN must be set"})
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

	var user model.User
	if err := h.db.First(&user, userID).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
		return
	}

	var existing int64
	h.db.Model(&model.SetupSession{}).Where("user_id = ? AND vta_name = ?", userID, req.VtaName).Count(&existing)
	if existing > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "vta_name already in use"})
		return
	}

	vtaDidUrl := h.didHostingBase + "/user-" + user.UniqueId + "/" + req.VtaName

	subdomain := setup.GenerateSubdomain(h.appEnv)
	fqdn := subdomain + "." + h.clusterDomain

	recordID, err := h.cf.CreateARecord(c.Request.Context(), fqdn, h.ingressIP)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to create DNS record: " + err.Error()})
		return
	}

	session := model.SetupSession{
		UserID:           userID,
		Mode:             req.Mode,
		Status:           "dns_provisioned",
		Domain:           h.clusterDomain,
		Subdomain:        subdomain,
		CFRecordID:       recordID,
		VtaName:          req.VtaName,
		MediatorDid:      h.mediatorDid,
		VtaDidUrl:        vtaDidUrl,
		VtaImage:         req.VtaImage,
		AdminDid:         req.AdminDid,
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
		"vta_image": req.VtaImage,
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
		MediatorDid string `json:"mediator_did"`
		VtaDidUrl   string `json:"vta_did_url"`
		VtaDid      string `json:"vta_did,omitempty"`
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
			MediatorDid: s.MediatorDid,
			VtaDidUrl:   s.VtaDidUrl,
			VtaDid:      s.VtaDid,
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
		"vta_did":    session.VtaDid,
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

	if h.didHosting != nil && session.VtaDidUrl != "" {
		path := session.VtaDidUrl
		if idx := strings.Index(path, "/user-"); idx >= 0 {
			path = path[idx+1:] // user-abc/pvta
		}
		if err := h.didHosting.DeleteDid(c.Request.Context(), path); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to delete DID from hosting: " + err.Error()})
			return
		}
	}

	if h.k8s != nil {
		ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
		h.k8s.DeleteSetupResources(c.Request.Context(), ns, session.ID)
		h.k8s.DeleteVtaResources(c.Request.Context(), ns, session.ID)
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

// POST /api/v1/setup/:id/admin
func (h *SetupHandler) ProvisionAdmin(c *gin.Context) {
	id := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	if session.Status != "vta_setup_complete" {
		c.JSON(http.StatusConflict, gin.H{"error": "session must be in vta_setup_complete status"})
		return
	}

	if h.orch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s not configured"})
		return
	}

	var req struct {
		AdminDid string `json:"admin_did" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	h.orch.Provision(session.ID, req.AdminDid)

	c.JSON(http.StatusAccepted, gin.H{"status": "provisioning"})
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
