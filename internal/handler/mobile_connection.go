package handler

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/setup"
	farmtrusttask "github.com/ic3software/vtafarm-api/internal/trusttask"
	"gorm.io/gorm"
)

const vtaClaimFlowType = "/vti/flow/vta-claim/0.1"

type MobileConnectionHandler struct {
	db             *gorm.DB
	kickInitial    func(uint)
	kickAdditional func(string)
	enabled        bool
	farmDID        string
	linkHost       *url.URL
}

func NewMobileConnectionHandler(db *gorm.DB, orch *setup.Orchestrator, kickAdditional func(string), cfg config.VTAClaimConfig, trustTaskReady bool) *MobileConnectionHandler {
	h := &MobileConnectionHandler{db: db, farmDID: strings.TrimSpace(cfg.FarmDID), kickAdditional: kickAdditional}
	if orch != nil {
		h.kickInitial = orch.KickProvision
	}
	host, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(cfg.LinkHost), "/"))
	trustTaskBase, trustTaskErr := url.Parse(strings.TrimSpace(cfg.TrustTaskBase))
	_, signerErr := farmtrusttask.NewSigner(cfg.FarmDID, cfg.FarmSigningKey)
	if err == nil && host.Scheme == "https" && host.Host != "" && host.User == nil && (host.Path == "" || host.Path == "/") && host.RawQuery == "" && host.Fragment == "" &&
		trustTaskErr == nil && trustTaskBase.Scheme == "https" && trustTaskBase.Host != "" && trustTaskBase.User == nil && trustTaskBase.RawQuery == "" && trustTaskBase.Fragment == "" && trustTaskBase.RawPath == "" && strings.TrimSuffix(trustTaskBase.Path, "/") == "/trust-task" &&
		h.farmDID != "" && signerErr == nil {
		h.linkHost = host
		handle := strings.Repeat("A", 22)
		probe := &model.MobileConnection{ClaimHandle: &handle, ExpiresAt: time.Now().Add(connection.QRLifetime)}
		if len(h.triggerLink(probe)) <= 251 {
			h.enabled = orch != nil && trustTaskReady
		} else {
			h.linkHost = nil
		}
	}
	return h
}

func (h *MobileConnectionHandler) triggerLink(r *model.MobileConnection) string {
	if h.linkHost == nil || r == nil || r.ClaimHandle == nil {
		return ""
	}
	u := *h.linkHost
	u.Path = strings.TrimSuffix(u.Path, "/") + "/t"
	return u.String() + "#_from=" + url.QueryEscape(h.farmDID) +
		"&_id=" + url.QueryEscape(*r.ClaimHandle) +
		"&_exp=" + strconv.FormatInt(r.ExpiresAt.Unix(), 10) +
		"&_type=" + url.QueryEscape(vtaClaimFlowType)
}

func (h *MobileConnectionHandler) ownerSession(c *gin.Context) *model.SetupSession {
	var s model.SetupSession
	err := h.db.WithContext(c.Request.Context()).Where("vta_name = ? AND user_id = ?", c.Param("id"), c.MustGet(middleware.ContextUserID)).First(&s).Error
	if err != nil {
		writeConnectionError(c, err)
		return nil
	}
	return &s
}

func writeConnectionError(c *gin.Context, err error) {
	status, reason, message := http.StatusInternalServerError, "connection_unavailable", "Unable to update connection. Please retry."
	switch {
	case errors.Is(err, connection.ErrInvalidDID):
		status, reason, message = 400, "invalid_admin_did", err.Error()
	case errors.Is(err, connection.ErrConflict), errors.Is(err, connection.ErrAlreadyClaimed):
		status, reason, message = 409, "connection_conflict", err.Error()
	case errors.Is(err, connection.ErrExpired):
		status, reason, message = 410, "connection_expired", err.Error()
	case errors.Is(err, connection.ErrCooldown):
		status, reason, message = 429, "connection_cooldown", err.Error()
		c.Header("Retry-After", "5")
	case errors.Is(err, gorm.ErrRecordNotFound):
		status, reason, message = 404, "connection_not_found", "Connection or VTA not found."
	}
	c.JSON(status, gin.H{"error": message, "reason": reason})
}

type mobileView struct {
	RequestID    string     `json:"request_id"`
	Status       string     `json:"status"`
	VtaDid       string     `json:"vta_did"`
	Operation    string     `json:"operation"`
	ExpiresAt    time.Time  `json:"expires_at"`
	ConfirmBy    *time.Time `json:"confirm_by,omitempty"`
	TriggerLink  string     `json:"trigger_link,omitempty"`
	ClaimCheck   string     `json:"claim_check,omitempty"`
	CandidateDID string     `json:"candidate_did,omitempty"`
	Error        string     `json:"error,omitempty"`
}

func (h *MobileConnectionHandler) view(r *model.MobileConnection) *mobileView {
	if r == nil {
		return nil
	}
	v := &mobileView{
		RequestID: r.ID,
		Status:    r.Status,
		VtaDid:    r.VtaDid,
		Operation: r.Operation,
		ExpiresAt: r.ExpiresAt,
		ConfirmBy: r.ConfirmBy,
	}
	if r.Status == "open" && h.enabled {
		v.TriggerLink = h.triggerLink(r)
	}
	if r.Status == "claimed" {
		v.ClaimCheck = connection.ClaimCheck(r.AdminDid)
		v.CandidateDID = r.AdminDid
	}
	if r.ProvisionError != "" && (r.Status == "confirmed" || r.Status == "provisioning") {
		v.Error = r.ProvisionError
	}
	return v
}

func (h *MobileConnectionHandler) reply(c *gin.Context, r *model.MobileConnection, s *model.SetupSession) {
	if err := connection.Reconcile(c.Request.Context(), h.db, r, s); err != nil {
		writeConnectionError(c, err)
		return
	}
	now, err := connection.Now(h.db.WithContext(c.Request.Context()))
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	c.JSON(200, gin.H{"enabled": h.enabled, "server_time": now, "connection": h.view(r)})
}

func (h *MobileConnectionHandler) Current(c *gin.Context) {
	s := h.ownerSession(c)
	if s == nil {
		return
	}
	r, err := connection.Current(h.db.WithContext(c.Request.Context()), s.ID)
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	if r != nil && r.Status == "confirmed" {
		if !h.startWorker(c, r) {
			return
		}
		if err := h.db.WithContext(c.Request.Context()).First(r, "id = ?", r.ID).Error; err != nil {
			writeConnectionError(c, err)
			return
		}
	}
	h.reply(c, r, s)
}

func (h *MobileConnectionHandler) Change(c *gin.Context) {
	s := h.ownerSession(c)
	if s == nil {
		return
	}
	action := "create"
	if c.Request.Method == http.MethodDelete {
		action = "cancel"
	} else if c.Param("request_id") != "" {
		action = "refresh"
	}
	if action != "cancel" && !h.enabled {
		c.JSON(503, gin.H{"error": "VTA claim is not available.", "reason": "vta_claim_disabled"})
		return
	}
	r, err := connection.Change(c.Request.Context(), h.db, s.ID, action, c.Param("request_id"))
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	h.reply(c, r, s)
}

func (h *MobileConnectionHandler) Confirm(c *gin.Context) {
	s := h.ownerSession(c)
	if s == nil {
		return
	}
	r, err := connection.Confirm(c.Request.Context(), h.db, s.ID, c.Param("request_id"))
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	if !h.startWorker(c, r) {
		return
	}
	if err := h.db.WithContext(c.Request.Context()).First(r, "id = ?", r.ID).Error; err != nil {
		writeConnectionError(c, err)
		return
	}
	h.reply(c, r, s)
}

func (h *MobileConnectionHandler) Reject(c *gin.Context) {
	s := h.ownerSession(c)
	if s == nil {
		return
	}
	r, err := connection.Reject(c.Request.Context(), h.db, s.ID, c.Param("request_id"))
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	h.reply(c, r, s)
}

func (h *MobileConnectionHandler) startWorker(c *gin.Context, r *model.MobileConnection) bool {
	started, err := connection.StartProvisioning(c.Request.Context(), h.db, r.ID)
	if err != nil {
		writeConnectionError(c, err)
		return false
	}
	*r = *started
	if r.Operation == connection.OperationGrantACL {
		if h.kickAdditional != nil {
			h.kickAdditional(r.ID)
		}
	} else if h.kickInitial != nil {
		h.kickInitial(r.SessionID)
	}
	return true
}
