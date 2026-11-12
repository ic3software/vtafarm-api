package handler

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/ghcr"
	"github.com/ic3software/vtafarm-api/internal/model"
)

// loadImageDefault returns the configured image for a component, or "" when
// none is set. Like resource defaults, it reads the DB on every call so all
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
// a configured default. Components without one are omitted.
func (h *SetupHandler) AdminImageDefaults(c *gin.Context) {
	response, err := imageDefaultsResponse(c.Request.Context(), h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load image defaults"})
		return
	}
	c.JSON(http.StatusOK, response)
}

// PUT /api/v1/admin/image-defaults
// Body {"images": {"vta": "ghcr.io/…:tag", "mediator": ""}}. Only the listed
// components change; an empty image clears that pin so pickers follow
// "latest" again. A non-empty image must be one of the immutable images
// currently listed for its component. All-or-nothing: every entry is
// validated before any is written, and the writes share one transaction.
func (h *SetupHandler) AdminSaveImageDefaults(c *gin.Context) {
	var input struct {
		Images map[string]string `json:"images"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || len(input.Images) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "images is required: an object of component to image"})
		return
	}
	ctx := c.Request.Context()

	// Fixed order: errors are deterministic, and concurrent saves lock rows in
	// the same order (as resource defaults do).
	changes := make([]model.ImageDefault, 0, len(input.Images))
	for component := range input.Images {
		if _, ok := h.ghcrClientFor(component); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown component " + component + unknownComponentMessage})
			return
		}
	}
	for _, component := range model.UpgradeComponents {
		raw, listed := input.Images[component]
		if !listed {
			continue
		}
		image := strings.TrimSpace(raw)
		if image != "" {
			client, _ := h.ghcrClientFor(component)
			if client == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image source not configured for " + component})
				return
			}
			tags, err := client.ListTags(ctx)
			if err != nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch " + component + " images: " + err.Error()})
				return
			}
			if !slices.ContainsFunc(tags, func(tag ghcr.ImageTag) bool { return tag.Image == image }) {
				c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "select an immutable image from the " + component + " image list"})
				return
			}
		}
		changes = append(changes, model.ImageDefault{Component: component, Image: image})
	}

	if err := h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, change := range changes {
			if change.Image == "" {
				if err := tx.Where("component = ?", change.Component).Delete(&model.ImageDefault{}).Error; err != nil {
					return err
				}
				continue
			}
			if err := model.UpsertImageDefault(tx, change.Component, change.Image); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not save image defaults"})
		return
	}

	response, err := imageDefaultsResponse(ctx, h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load image defaults"})
		return
	}
	c.JSON(http.StatusOK, response)
}
