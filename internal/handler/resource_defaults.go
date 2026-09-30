package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/clause"

	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/resourceprofile"
)

func (h *SetupHandler) AdminResourceDefaults(c *gin.Context) {
	profiles, err := resourceprofile.Load(c.Request.Context(), h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load resource defaults"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"resources": profiles, "factory_defaults": resourceprofile.Factory()})
}

func (h *SetupHandler) AdminSaveResourceDefaults(c *gin.Context) {
	var input struct {
		Resources []memoryResourceInput `json:"resources"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "resources are required"})
		return
	}
	if len(input.Resources) != 4 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "resources must contain all four components"})
		return
	}
	if err := validateMemoryInputs(input.Resources); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	rows := make([]model.ResourceDefault, 0, 4)
	profiles := resourceprofile.Factory()
	// Fixed ordering avoids conflicting lock order when admins save concurrently.
	for _, component := range []string{"mediator", "vtc", "vta", "dids"} {
		for _, item := range input.Resources {
			if item.Component != component {
				continue
			}
			rows = append(rows, model.ResourceDefault{Component: component, MemoryRequest: item.MemoryRequest, MemoryLimit: item.MemoryLimit})
			profile := profiles[component]
			profile.MemoryRequest, profile.MemoryLimit = item.MemoryRequest, item.MemoryLimit
			profiles[component] = profile
		}
	}
	if err := h.db.WithContext(c.Request.Context()).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "component"}},
		DoUpdates: clause.AssignmentColumns([]string{"memory_request", "memory_limit", "updated_at"}),
	}).Create(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not save resource defaults"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"resources": profiles, "factory_defaults": resourceprofile.Factory()})
}
