package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/setup"
	"github.com/ic3software/vtafarm-api/internal/siop/webvh"
)

// DownloadDIDLog gives the owner the exact log produced by VTA setup.
func (h *SetupHandler) DownloadDIDLog(c *gin.Context) {
	session := h.userSession(c)
	if session == nil {
		return
	}
	if session.ConnectionSource != model.ConnectionExternal || session.VtaDid == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "this session has no external DID log to publish"})
		return
	}
	didLog := session.VtaDidLog
	if didLog == "" || len(didLog) >= setup.MaxVtaDIDLogBytes {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "VTA setup DID log is missing or too large"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Disposition", "attachment; filename=\""+session.VtaName+"-vta.did.jsonl\"")
	c.Data(http.StatusOK, "application/jsonl", []byte(didLog+"\n"))
}

// ValidatePublishedDID only advances an external session after its public DID
// log resolves and its cryptographic history validates against the VTA DID.
func (h *SetupHandler) ValidatePublishedDID(c *gin.Context) {
	session := h.userSession(c)
	if session == nil {
		return
	}
	if session.ConnectionSource != model.ConnectionExternal || session.Status != "awaiting_did_publication" {
		c.JSON(http.StatusConflict, gin.H{"error": "session is not awaiting external DID publication"})
		return
	}
	if session.VtaDid == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "VTA DID is not available yet"})
		return
	}
	if err := webvh.NewResolver(5*time.Second).ResolveDID(c.Request.Context(), session.VtaDid); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "VTA DID is not published or valid: " + err.Error()})
		return
	}
	result := h.db.Model(&model.SetupSession{}).
		Where("id = ? AND status = ?", session.ID, "awaiting_did_publication").
		Updates(map[string]any{"status": "vta_setup_complete", "updated_at": time.Now()})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record DID publication"})
		return
	}
	if result.RowsAffected != 1 {
		c.JSON(http.StatusConflict, gin.H{"error": "session state changed; refresh and try again"})
		return
	}
	if session.AdminDid != "" && h.orch != nil {
		h.orch.Provision(session.ID, session.AdminDid)
	}
	c.JSON(http.StatusOK, gin.H{"valid": true, "status": "vta_setup_complete"})
}
