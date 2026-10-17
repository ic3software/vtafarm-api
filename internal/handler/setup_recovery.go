package handler

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ic3software/vtafarm-api/internal/ghcr"
	"github.com/ic3software/vtafarm-api/internal/model"
)

type recoverSetupRequest struct {
	Components []componentImage `json:"components" binding:"required,min=1,dive"`
}

type deploymentRecovery struct {
	Stage string
}

func recoveryForStage(mode, stage string) (deploymentRecovery, bool) {
	if mode == model.ModeVtaOnly {
		if stage != "deploy_vta" {
			return deploymentRecovery{}, false
		}
		return deploymentRecovery{Stage: stage}, true
	}
	if mode != model.ModeFullStack || !slices.Contains([]string{
		"deploy_dids", "deploy_mediator", "deploy_vta", "deploy_vtc",
	}, stage) {
		return deploymentRecovery{}, false
	}
	return deploymentRecovery{Stage: stage}, true
}

func recoveryComponentsForMode(mode string) []string {
	if mode == model.ModeVtaOnly {
		return []string{"vta"}
	}
	if mode == model.ModeFullStack {
		return []string{"vta", "mediator", "dids", "vtc"}
	}
	return nil
}

func validateRecoveryComponents(mode string, components []componentImage) error {
	required := recoveryComponentsForMode(mode)
	if len(required) == 0 {
		return fmt.Errorf("mode %s cannot be recovered", mode)
	}
	seen := make(map[string]bool, len(components))
	for _, ci := range components {
		if seen[ci.Component] {
			return fmt.Errorf("duplicate component %s", ci.Component)
		}
		seen[ci.Component] = true
	}
	var missing, extra []string
	for _, component := range required {
		if !seen[component] {
			missing = append(missing, component)
		}
	}
	for component := range seen {
		if !slices.Contains(required, component) {
			extra = append(extra, component)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		parts := make([]string, 0, 2)
		if len(missing) > 0 {
			parts = append(parts, "missing "+strings.Join(missing, ", "))
		}
		if len(extra) > 0 {
			parts = append(parts, "unsupported "+strings.Join(extra, ", "))
		}
		return fmt.Errorf("mode %s requires exactly its component images: %s", mode, strings.Join(parts, "; "))
	}
	return nil
}

func (h *SetupHandler) recoveryImageClient(component string) *ghcr.Client {
	switch component {
	case "vta":
		return h.ghcr
	case "mediator":
		return h.mediatorGhcr
	case "dids":
		return h.didsGhcr
	case "vtc":
		return h.vtcGhcr
	default:
		return nil
	}
}

// RecoverSession retries a failed full-stack deployment owned by the caller.
func (h *SetupHandler) RecoverSession(c *gin.Context) {
	if session := h.userSession(c); session != nil {
		h.recoverSession(c, session)
	}
}

// AdminRecoverSession is the admin-cookie twin of RecoverSession.
func (h *SetupHandler) AdminRecoverSession(c *gin.Context) {
	if session := h.adminSession(c); session != nil {
		h.recoverSession(c, session)
	}
}

var errRecoveryConflict = errors.New("session is not recoverable")

func (h *SetupHandler) recoverSession(c *gin.Context, session *model.SetupSession) {
	if h.k8s == nil || h.orch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "K8s not configured — recovery unavailable"})
		return
	}

	var req recoverSetupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	recovery, ok := recoveryForStage(session.Mode, session.FailureStage())
	if !ok {
		c.JSON(http.StatusConflict, gin.H{"error": "only sessions failed at a recoverable deploy stage can be recovered"})
		return
	}
	if err := validateRecoveryComponents(session.Mode, req.Components); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	for _, ci := range req.Components {
		// Keeping a component on its persisted image is valid even when that old
		// tag has since fallen out of the registry listing. Only replacements
		// need to prove that the registry currently serves them.
		if session.ComponentImage(ci.Component) == ci.Image {
			continue
		}
		client := h.recoveryImageClient(ci.Component)
		if client == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image source not configured for " + ci.Component})
			return
		}
		tags, err := client.ListTags(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch " + ci.Component + " images: " + err.Error()})
			return
		}
		if !slices.ContainsFunc(tags, func(tag ghcr.ImageTag) bool { return tag.Image == ci.Image }) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "image is not available for " + ci.Component})
			return
		}
	}

	conflictMessage := "session is no longer recoverable"
	err := h.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var locked model.SetupSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, session.ID).Error; err != nil {
			return err
		}
		lockedRecovery, ok := recoveryForStage(locked.Mode, locked.FailureStage())
		if !ok {
			return errRecoveryConflict
		}
		if locked.Mode != session.Mode || lockedRecovery.Stage != recovery.Stage {
			conflictMessage = "session failure stage changed; reload before recovering"
			return errRecoveryConflict
		}

		var activeUpgrades int64
		if err := tx.Model(&model.UpgradeTask{}).
			Where("session_id = ? AND status IN ?", locked.ID, []string{
				model.UpgradeTaskPending, model.UpgradeTaskRunning, model.UpgradeTaskRollingBack,
			}).Count(&activeUpgrades).Error; err != nil {
			return err
		}
		if activeUpgrades > 0 {
			conflictMessage = "an upgrade or rollback for this session is already in progress"
			return errRecoveryConflict
		}

		updates := map[string]any{
			"status":       lockedRecovery.Stage,
			"error_msg":    "",
			"failed_stage": "",
			"updated_at":   time.Now(),
		}
		for _, ci := range req.Components {
			updates[model.UpgradeImageColumn(ci.Component)] = ci.Image
		}
		result := tx.Model(&locked).Where("status = ?", "failed").Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errRecoveryConflict
		}
		recovery = lockedRecovery
		return nil
	})
	if errors.Is(err, errRecoveryConflict) {
		c.JSON(http.StatusConflict, gin.H{"error": conflictMessage})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not start recovery"})
		return
	}

	h.orch.RecoverDeployment(session.ID)
	c.JSON(http.StatusAccepted, gin.H{
		"status": recovery.Stage, "components": req.Components,
	})
}
