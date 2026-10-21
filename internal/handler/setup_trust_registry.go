package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/capacity"
	"github.com/ic3software/vtafarm-api/internal/ghcr"
	"github.com/ic3software/vtafarm-api/internal/model"
)

func (h *SetupHandler) ConfigureTrustRegistry(client *ghcr.Client) { h.trustRegistryGhcr = client }

func registryJSON(s *model.SetupSession) gin.H {
	state := s.TrustRegistryState
	if state == "" {
		state = "disabled"
	}
	return gin.H{"enabled": s.TrustRegistryEnabled, "state": state, "phase": s.TrustRegistryPhase, "image": s.TrustRegistryImage, "did": s.TrustRegistryDid, "error": s.TrustRegistryError}
}

func (h *SetupHandler) GetTrustRegistry(c *gin.Context) {
	if s := h.userSession(c); s != nil {
		c.JSON(http.StatusOK, registryJSON(s))
	}
}
func (h *SetupHandler) EnableTrustRegistry(c *gin.Context) {
	if s := h.userSession(c); s != nil {
		h.enableTrustRegistry(c, s)
	}
}
func (h *SetupHandler) AdminGetTrustRegistry(c *gin.Context) {
	if s := h.platformSession(c); s != nil {
		c.JSON(http.StatusOK, registryJSON(s))
	}
}
func (h *SetupHandler) AdminEnableTrustRegistry(c *gin.Context) {
	if s := h.platformSession(c); s != nil {
		h.enableTrustRegistry(c, s)
	}
}

func (h *SetupHandler) validRegistryImage(c *gin.Context, image string) bool {
	if h.trustRegistryGhcr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Trust Registry image source is not configured"})
		return false
	}
	tags, err := h.trustRegistryGhcr.ListTags(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Could not load Trust Registry images"})
		return false
	}
	if !slices.ContainsFunc(tags, func(tag ghcr.ImageTag) bool { return tag.Image == image }) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Select an immutable image from the Trust Registry image list"})
		return false
	}
	return true
}

func (h *SetupHandler) enableTrustRegistry(c *gin.Context, s *model.SetupSession) {
	if !s.IsFullStack() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Trust Registry requires a Full Stack"})
		return
	}
	if s.Status != "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "The Full Stack must be running before enabling Trust Registry"})
		return
	}
	var request struct {
		Image string `json:"image" binding:"required"`
	}
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "An image is required"})
		return
	}
	if !h.orch.RegistryAvailable() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Trust Registry is unavailable because Kubernetes or Vault is not configured"})
		return
	}
	if s.TrustRegistryState == "active" {
		if s.TrustRegistryImage != request.Image {
			c.JSON(http.StatusConflict, gin.H{"error": "Use image upgrades to change the active Registry image"})
			return
		}
		c.JSON(http.StatusOK, registryJSON(s))
		return
	}
	if s.TrustRegistryState == "enabling" {
		if s.TrustRegistryImage != request.Image {
			c.JSON(http.StatusConflict, gin.H{"error": "Registry enablement is already in progress"})
			return
		}
		h.kickTrustRegistry(s.ID)
		c.JSON(http.StatusAccepted, registryJSON(s))
		return
	}
	if !h.validRegistryImage(c, request.Image) {
		return
	}
	if !h.capacityAllows(c, capacity.TrustRegistry) {
		return
	}
	release, err := h.acquireStackMaintenance(s.ID)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Another stack maintenance operation is in progress"})
		return
	}
	defer func() { release() }()
	var upgrading int64
	if err = h.db.Model(&model.UpgradeTask{}).Where("session_id = ? AND status IN ?", s.ID, []string{model.UpgradeTaskPending, model.UpgradeTaskRunning, model.UpgradeTaskRollingBack}).Count(&upgrading).Error; err != nil || upgrading > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Wait for the image upgrade or rollback to finish"})
		return
	}
	result := h.db.Model(&model.SetupSession{}).Where("id = ? AND status = ? AND trust_registry_state IN ?", s.ID, "running", []string{"disabled", "failed"}).Updates(map[string]any{"trust_registry_enabled": true, "trust_registry_state": "enabling", "trust_registry_image": request.Image, "trust_registry_phase": "queued", "trust_registry_error": "", "updated_at": time.Now()})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not save registry operation"})
		return
	}
	if result.RowsAffected != 1 {
		c.JSON(http.StatusConflict, gin.H{"error": "Stack state changed; reload and try again"})
		return
	}
	s.TrustRegistryEnabled = true
	s.TrustRegistryState = "enabling"
	s.TrustRegistryImage = request.Image
	s.TrustRegistryPhase = "queued"
	s.TrustRegistryError = ""
	// The worker can claim maintenance after this request releases it.
	release()
	release = func() {}
	h.kickTrustRegistry(s.ID)
	c.JSON(http.StatusAccepted, registryJSON(s))
}

func (h *SetupHandler) kickTrustRegistry(id uint) { go h.runTrustRegistry(context.Background(), id) }

func (h *SetupHandler) StartTrustRegistryWorker(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			var sessions []model.SetupSession
			if err := h.db.WithContext(ctx).Where("status = ? AND trust_registry_state = ?", "running", "enabling").Find(&sessions).Error; err == nil {
				for _, s := range sessions {
					go h.runTrustRegistry(ctx, s.ID)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (h *SetupHandler) runTrustRegistry(parent context.Context, id uint) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	if h.orch == nil {
		return
	}
	// Serialize registry workers across replicas, independently of the HTTP
	// request lifetime. The maintenance lease blocks other stack mutations.
	// Keep the lock through cancellation cleanup, which restarts the daemons
	// under its own bounded context after the operation deadline expires.
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			var locked bool
			if err := tx.Raw("SELECT pg_try_advisory_xact_lock(?, ?)", 565441, int64(id)).Scan(&locked).Error; err != nil {
				return err
			}
			if !locked {
				return nil
			}
		}
		var s model.SetupSession
		if err := h.db.First(&s, id).Error; err != nil {
			return err
		}
		if s.Status != "running" || s.TrustRegistryState != "enabling" {
			return nil
		}
		// The advisory lock proves no other registry worker owns this session.
		// Other maintenance rechecks enabling after its lease, so the timestamp
		// left by a dead registry worker can be released without a 15-minute outage.
		if err := h.db.Model(&model.VtaAclSnapshot{}).Where("session_id = ?", id).Update("maintenance_started_at", nil).Error; err != nil {
			return err
		}
		release, err := h.acquireStackMaintenance(id)
		if err != nil {
			return nil
		}
		defer release()
		err = h.orch.EnableTrustRegistry(ctx, &s)
		if parent.Err() != nil {
			return nil
		} // A new replica resumes the persisted phase.
		if err != nil {
			if saveErr := h.db.Model(&model.SetupSession{}).Where("id = ?", id).Updates(map[string]any{"trust_registry_state": "failed", "trust_registry_error": err.Error(), "updated_at": time.Now()}).Error; saveErr != nil {
				return saveErr
			}
			return h.orch.CleanupFailedRegistryJobs(&s)
		}
		return nil
	})
	if err != nil {
		log.Printf("[setup] registry worker %d failed: %v", id, err)
	}
}

func registryBusy(s *model.SetupSession) error {
	if s.TrustRegistryState == "enabling" {
		return errors.New("Trust Registry enablement is in progress")
	}
	return nil
}
