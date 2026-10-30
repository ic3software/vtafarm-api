package handler

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ic3software/vtafarm-api/internal/ghcr"
	"github.com/ic3software/vtafarm-api/internal/model"
)

// loadImageDefault returns the admin-pinned image for a component, or "" when
// none is pinned. Like resource defaults, it reads the DB on every call so all
// API replicas agree without a process-local cache.
func loadImageDefault(ctx context.Context, db *gorm.DB, component string) (string, error) {
	var row model.ImageDefault
	err := db.WithContext(ctx).Where("component = ?", component).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return row.Image, nil
}

// markImageDefault flags the listed tag whose image matches the pin. A pin
// that no longer appears in the list flags nothing, so frontends fall back to
// the "latest" channel.
func markImageDefault(tags []ghcr.ImageTag, pinned string) {
	if pinned == "" {
		return
	}
	for i := range tags {
		tags[i].Default = tags[i].Image == pinned
	}
}

func imageDefaultsResponse(ctx context.Context, db *gorm.DB) (gin.H, error) {
	var rows []model.ImageDefault
	if err := db.WithContext(ctx).Order("component").Find(&rows).Error; err != nil {
		return nil, err
	}
	images := make(map[string]string, len(rows))
	for _, row := range rows {
		images[row.Component] = row.Image
	}
	return gin.H{"images": images}, nil
}

// GET /api/v1/admin/image-defaults
// Returns {"images": {"<component>": "<image>"}} for the components that have
// a pinned default. Components without a pin are omitted.
func (h *SetupHandler) AdminImageDefaults(c *gin.Context) {
	response, err := imageDefaultsResponse(c.Request.Context(), h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load image defaults"})
		return
	}
	c.JSON(http.StatusOK, response)
}

// PUT /api/v1/admin/image-defaults
// Body {"component": "vta", "image": "ghcr.io/…:tag"}. An empty image clears
// the pin so users follow "latest" again. A non-empty image must be one of the
// immutable images currently listed for that component.
func (h *SetupHandler) AdminSaveImageDefault(c *gin.Context) {
	var input struct {
		Component string `json:"component"`
		Image     string `json:"image"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || input.Component == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "component is required"})
		return
	}
	client, ok := h.ghcrClientFor(input.Component)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown component " + input.Component + unknownComponentMessage})
		return
	}
	ctx := c.Request.Context()
	image := strings.TrimSpace(input.Image)

	if image == "" {
		if err := h.db.WithContext(ctx).Where("component = ?", input.Component).Delete(&model.ImageDefault{}).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not clear image default"})
			return
		}
	} else {
		if client == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image source not configured for " + input.Component})
			return
		}
		tags, err := client.ListTags(ctx)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch images: " + err.Error()})
			return
		}
		if !slices.ContainsFunc(tags, func(tag ghcr.ImageTag) bool { return tag.Image == image }) {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "select an immutable image from the " + input.Component + " image list"})
			return
		}
		row := model.ImageDefault{Component: input.Component, Image: image}
		if err := h.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "component"}},
			DoUpdates: clause.AssignmentColumns([]string{"image", "updated_at"}),
		}).Create(&row).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not save image default"})
			return
		}
	}

	response, err := imageDefaultsResponse(ctx, h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load image defaults"})
		return
	}
	c.JSON(http.StatusOK, response)
}
