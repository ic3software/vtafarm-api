package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/maintenance"
)

func Workflow(manager *maintenance.Manager, operationType string, allowDuringMaintenance bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		target := strings.TrimSpace(c.Param("id"))
		if target == "" {
			target = strings.TrimSpace(c.Param("request_id"))
		}
		release, err := manager.Acquire(c.Request.Context(), operationType, target, allowDuringMaintenance)
		if err != nil {
			if errors.Is(err, maintenance.ErrEnabled) {
				state, _ := manager.State(c.Request.Context())
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
					"error":       "VTA Farm is under maintenance. Changes are temporarily unavailable.",
					"reason":      "maintenance_mode",
					"maintenance": state,
				})
				return
			}
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "workflow admission is temporarily unavailable"})
			return
		}
		defer release()
		c.Next()
	}
}
