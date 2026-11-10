package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/maintenance"
	"github.com/ic3software/vtafarm-api/internal/middleware"
)

type MaintenanceHandler struct {
	manager *maintenance.Manager
}

func NewMaintenanceHandler(manager *maintenance.Manager) *MaintenanceHandler {
	return &MaintenanceHandler{manager: manager}
}

func (h *MaintenanceHandler) Status(c *gin.Context) {
	state, err := h.manager.State(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "maintenance status unavailable"})
		return
	}
	c.JSON(http.StatusOK, state)
}

func (h *MaintenanceHandler) AdminStatus(c *gin.Context) {
	snapshot, err := h.manager.Snapshot(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "maintenance status unavailable"})
		return
	}
	c.JSON(http.StatusOK, snapshot)
}

func (h *MaintenanceHandler) Set(c *gin.Context) {
	var request struct {
		Enabled *bool  `json:"enabled"`
		Message string `json:"message"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enabled is required"})
		return
	}
	request.Message = strings.TrimSpace(request.Message)
	if len(request.Message) > 500 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message must be 500 characters or fewer"})
		return
	}
	adminID := c.MustGet(middleware.ContextUserID).(uint)
	snapshot, err := h.manager.Set(c.Request.Context(), *request.Enabled, request.Message, adminID)
	if errors.Is(err, maintenance.ErrOperationsInProgress) {
		c.JSON(http.StatusConflict, gin.H{
			"error":       "Maintenance mode cannot be changed while operations are running.",
			"reason":      "operations_in_progress",
			"maintenance": snapshot,
		})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not update maintenance mode"})
		return
	}
	c.JSON(http.StatusOK, snapshot)
}
