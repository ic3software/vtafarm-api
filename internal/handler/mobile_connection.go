package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/setup"
	"gorm.io/gorm"
)

type MobileConnectionHandler struct {
	db      *gorm.DB
	kick    func(uint)
	enabled bool
	origin  string
	key     []byte
}

func NewMobileConnectionHandler(db *gorm.DB, orch *setup.Orchestrator, cfg config.MobileConnectionConfig, clusterDomain string) *MobileConnectionHandler {
	h := &MobileConnectionHandler{db: db, key: []byte(cfg.SigningKey)}
	if orch != nil {
		h.kick = orch.KickProvision
	}
	domain := strings.TrimSuffix(clusterDomain, ".")
	origin := "https://vtafarm-api." + domain
	u, err := url.Parse(origin)
	if domain != "" && err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == "" && len(h.key) >= 32 {
		h.origin = origin
		h.enabled = orch != nil
	}
	return h
}

func (h *MobileConnectionHandler) token(purpose, id string) string {
	mac := hmac.New(sha256.New, h.key)
	mac.Write([]byte("vtafarm/mobile/" + purpose + "/" + id))
	return id + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *MobileConnectionHandler) tokenID(purpose, token string) (string, bool) {
	id, _, ok := strings.Cut(token, ".")
	if !ok || len(token) != 80 || len(h.key) < 32 {
		return "", false
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id || !hmac.Equal([]byte(h.token(purpose, id)), []byte(token)) {
		return "", false
	}
	return id, true
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
	case errors.Is(err, connection.ErrConflict):
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
	RequestID   string    `json:"request_id"`
	Status      string    `json:"status"`
	VtaDid      string    `json:"vta_did"`
	ExpiresAt   time.Time `json:"expires_at"`
	CallbackURL string    `json:"callback_url,omitempty"`
	Error       string    `json:"error,omitempty"`
}

func (h *MobileConnectionHandler) view(r *model.MobileConnection, s *model.SetupSession, now time.Time) *mobileView {
	if r == nil {
		return nil
	}
	v := &mobileView{RequestID: r.ID, Status: r.Status, VtaDid: r.VtaDid, ExpiresAt: r.ExpiresAt}
	switch r.Status {
	case "pending":
		if !now.Before(r.ExpiresAt) {
			v.Status = "expired"
		} else if h.enabled {
			v.CallbackURL = h.origin + "/api/v1/mobile-connections/callback/" + h.token("callback", r.ID)
		}
	case "accepted":
		v.Status = "provisioning"
		if s.Status == "failed" {
			v.Status, v.Error = "failed", "VTA setup failed. View the setup details for the next step."
		} else if r.ConnectedAt != nil {
			v.Status = "connected"
		} else if s.Status == "running" {
			v.Status = "awaiting_mobile"
			if r.AcceptedAt != nil && !now.Before(r.AcceptedAt.Add(connection.ProgressLifetime)) {
				v.Status, v.Error = "failed", "Mobile confirmation expired. Your VTA remains configured; continue in your app."
			}
		}
	}
	return v
}

func (h *MobileConnectionHandler) reply(c *gin.Context, r *model.MobileConnection, s *model.SetupSession) {
	now, err := connection.Now(h.db.WithContext(c.Request.Context()))
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	c.JSON(200, gin.H{"enabled": h.enabled, "server_time": now, "connection": h.view(r, s, now)})
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
	h.reply(c, r, s)
}

func (h *MobileConnectionHandler) Change(c *gin.Context) {
	s := h.ownerSession(c)
	if s == nil {
		return
	}
	action := "create"
	if c.Request.Method == "DELETE" {
		action = "cancel"
	} else if c.Param("request_id") != "" {
		action = "refresh"
	}
	if action != "cancel" && !h.enabled {
		c.JSON(503, gin.H{"error": "Automatic mobile connection is not available.", "reason": "mobile_connection_disabled"})
		return
	}
	r, err := connection.Change(c.Request.Context(), h.db, s.ID, action, c.Param("request_id"))
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	if err := h.db.WithContext(c.Request.Context()).First(s, s.ID).Error; err != nil {
		writeConnectionError(c, err)
		return
	}
	h.reply(c, r, s)
}

func decodeConnectionBody(c *gin.Context, value any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		c.JSON(400, gin.H{"error": "Invalid connection request body."})
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		c.JSON(400, gin.H{"error": "Expected one JSON object."})
		return false
	}
	return true
}

func (h *MobileConnectionHandler) Callback(c *gin.Context) {
	if !h.enabled {
		c.JSON(503, gin.H{"error": "Automatic mobile connection is not available."})
		return
	}
	id, ok := h.tokenID("callback", c.Param("token"))
	if !ok {
		c.JSON(404, gin.H{"error": "Connection not found."})
		return
	}
	var body struct {
		AdminDID string `json:"admin_did"`
	}
	if !decodeConnectionBody(c, &body) {
		return
	}
	r, err := connection.AcceptMobile(c.Request.Context(), h.db, id, body.AdminDID)
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	h.kick(r.SessionID)
	var s model.SetupSession
	if err := h.db.WithContext(c.Request.Context()).First(&s, r.SessionID).Error; err != nil {
		writeConnectionError(c, err)
		return
	}
	now, err := connection.Now(h.db.WithContext(c.Request.Context()))
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	result := gin.H{"connection": h.view(r, &s, now), "server_time": now}
	if r.AcceptedAt != nil && now.Before(r.AcceptedAt.Add(connection.ProgressLifetime)) {
		result["progress_token"] = h.token("progress", r.ID)
		result["progress_url"] = h.origin + "/api/v1/mobile-connections/" + r.ID
		result["completion_url"] = h.origin + "/api/v1/mobile-connections/" + r.ID + "/complete"
		result["progress_expires_at"] = r.AcceptedAt.Add(connection.ProgressLifetime)
	}
	// The callback token is not a long-lived status credential.
	view := result["connection"].(*mobileView)
	view.CallbackURL = ""
	c.JSON(http.StatusAccepted, result)
}

func (h *MobileConnectionHandler) Progress(c *gin.Context) {
	token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	id, ok := h.tokenID("progress", token)
	if !ok || id != c.Param("request_id") {
		c.JSON(401, gin.H{"error": "Invalid mobile progress credential."})
		return
	}
	var r model.MobileConnection
	if err := h.db.WithContext(c.Request.Context()).First(&r, "id = ?", id).Error; err != nil {
		writeConnectionError(c, err)
		return
	}
	now, err := connection.Now(h.db.WithContext(c.Request.Context()))
	if err != nil {
		writeConnectionError(c, err)
		return
	}
	if r.Status != "accepted" || r.AcceptedAt == nil || !now.Before(r.AcceptedAt.Add(connection.ProgressLifetime)) {
		writeConnectionError(c, connection.ErrExpired)
		return
	}
	if c.Request.Method == "POST" {
		var body struct {
			Status string `json:"status"`
		}
		if !decodeConnectionBody(c, &body) {
			return
		}
		if body.Status != "connected" {
			c.JSON(400, gin.H{"error": "status must be connected"})
			return
		}
		if err := connection.Complete(c.Request.Context(), h.db, id); err != nil {
			writeConnectionError(c, err)
			return
		}
		r.ConnectedAt = &now
	}
	var s model.SetupSession
	if err := h.db.WithContext(c.Request.Context()).First(&s, r.SessionID).Error; err != nil {
		writeConnectionError(c, err)
		return
	}
	c.JSON(200, gin.H{"server_time": now, "connection": h.view(&r, &s, now)})
}
