package handler

import (
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/setup"
)

// PoolActivationGate is polled only by the prewarmed VTA init container. It
// releases activation after both the durable comparison-code confirmation and
// the provision worker's post-ACL authorization annotation are present.
func (h *PoolHandler) PoolActivationGate(c *gin.Context) {
	if h == nil || h.db == nil || h.setup == nil || h.setup.k8s == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	sessionID, ok := positiveID(c.Param("session_id"))
	activationID := strings.ToLower(strings.TrimSpace(c.Param("activation_id")))
	decoded, decodeErr := hex.DecodeString(activationID)
	if !ok || decodeErr != nil || len(decoded) != 16 {
		c.Status(http.StatusNotFound)
		return
	}

	var session model.SetupSession
	if err := h.db.WithContext(c.Request.Context()).First(&session, sessionID).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	var membership model.PoolSession
	if err := h.db.WithContext(c.Request.Context()).First(&membership, "session_id = ?", sessionID).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	namespace := h.setup.k8s.UserNamespace(strconv.FormatUint(uint64(session.UserID), 10))

	claim, err := connection.Current(h.db.WithContext(c.Request.Context()), sessionID)
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if claim == nil || setup.PoolActivationID(sessionID, claim.ID) != activationID {
		c.Status(http.StatusGone)
		return
	}
	if claim.Status == "claimed" {
		now, nowErr := connection.Now(h.db.WithContext(c.Request.Context()))
		if nowErr != nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		if claim.ConfirmBy == nil || !now.Before(*claim.ConfirmBy) {
			c.Status(http.StatusGone)
			return
		}
		c.Status(http.StatusAccepted)
		return
	}
	switch claim.Status {
	case "confirmed", "provisioning", "released":
		if claim.ConfirmedAt == nil || claim.AdminDid == "" || session.AdminDid != claim.AdminDid {
			c.Status(http.StatusAccepted)
			return
		}
	case "failed":
		c.Status(http.StatusAccepted)
		return
	default:
		c.Status(http.StatusGone)
		return
	}
	authorized, err := h.setup.k8s.PoolActivationAuthorized(c.Request.Context(), namespace, sessionID, activationID)
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if !authorized {
		c.Status(http.StatusAccepted)
		return
	}
	c.Status(http.StatusOK)
}
