package handler

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ic3software/vtafarm-api/internal/ghcr"
	"github.com/ic3software/vtafarm-api/internal/model"
)

type recoverSetupRequest struct {
	Component string `json:"component" binding:"required"`
	Image     string `json:"image" binding:"required"`
}

type deploymentRecovery struct {
	Stage       string
	Component   string
	ImageColumn string
}

func recoveryForStage(stage string) (deploymentRecovery, bool) {
	component := ""
	switch stage {
	case "deploy_dids":
		component = "dids"
	case "deploy_mediator":
		component = "mediator"
	case "deploy_vta":
		component = "vta"
	case "deploy_vtc":
		component = "vtc"
	default:
		return deploymentRecovery{}, false
	}
	return deploymentRecovery{Stage: stage, Component: component, ImageColumn: model.UpgradeImageColumn(component)}, true
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

	recovery, ok := recoveryForStage(session.FailureStage())
	if session.Mode != model.ModeFullStack || !ok {
		c.JSON(http.StatusConflict, gin.H{"error": "only full-stack sessions failed at a deploy stage can be recovered"})
		return
	}
	if req.Component != recovery.Component {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed stage " + recovery.Stage + " requires component " + recovery.Component})
		return
	}

	client := h.recoveryImageClient(recovery.Component)
	if client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image source not configured for " + recovery.Component})
		return
	}
	tags, err := client.ListTags(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch images: " + err.Error()})
		return
	}
	if !slices.ContainsFunc(tags, func(tag ghcr.ImageTag) bool { return tag.Image == req.Image }) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image is not available for " + recovery.Component})
		return
	}

	conflictMessage := "session is no longer recoverable"
	err = h.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var locked model.SetupSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, session.ID).Error; err != nil {
			return err
		}
		lockedRecovery, ok := recoveryForStage(locked.FailureStage())
		if locked.Mode != model.ModeFullStack || !ok {
			return errRecoveryConflict
		}
		if lockedRecovery.Component != req.Component {
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

		result := tx.Model(&locked).Where("status = ?", "failed").Updates(map[string]any{
			lockedRecovery.ImageColumn: req.Image,
			"status":                   lockedRecovery.Stage,
			"error_msg":                "",
			"failed_stage":             "",
			"updated_at":               time.Now(),
		})
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
		"status": recovery.Stage, "component": recovery.Component, "image": req.Image,
	})
}
