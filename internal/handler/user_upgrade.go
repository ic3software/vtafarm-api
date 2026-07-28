package handler

import (
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/ghcr"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
)

// User-facing self-service upgrades. A user changes the images of ONE session
// they own — every handler here loads the session with
// `vta_name = ? AND user_id = ?` (the same ownership rule as the rest of the
// setup routes), so a user can never touch another user's session. The
// machinery underneath (UpgradeBatch/UpgradeTask + the background runner) is
// shared with the admin batch API; user batches are marked with UserID and
// always target exactly one session.

type createSessionUpgradeRequest struct {
	// Target image per component — any image the component's registry serves,
	// so this covers both upgrades and downgrades.
	Components []componentImage `json:"components" binding:"required,min=1,dive"`
}

// ownSession loads the session for the authenticated user, enforcing
// ownership. Responds 404 (not 403) on someone else's session — same as
// SetupHandler — so session ids aren't probeable.
func (h *UpgradeHandler) ownSession(c *gin.Context) (*model.SetupSession, uint, bool) {
	userID := c.MustGet(middleware.ContextUserID).(uint)
	var session model.SetupSession
	if err := h.db.Where("vta_name = ? AND user_id = ?", c.Param("id"), userID).
		First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return nil, 0, false
	}
	return &session, userID, true
}

// CreateForSession — POST /api/v1/setup/:id/upgrade (user only). Changes the
// caller's own session to the requested images: validates each image against
// its component's registry, then creates a single-session batch and starts
// the shared background runner. One change at a time per session — a session
// with tasks still pending/running answers 409.
func (h *UpgradeHandler) CreateForSession(c *gin.Context) {
	session, userID, ok := h.ownSession(c)
	if !ok {
		return
	}

	var req createSessionUpgradeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.runner == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "K8s not configured — upgrades unavailable"})
		return
	}
	if session.Status != "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "session is " + session.Status + ", not running"})
		return
	}

	seen := map[string]bool{}
	for _, ci := range req.Components {
		if seen[ci.Component] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate component " + ci.Component})
			return
		}
		seen[ci.Component] = true
		if !slices.Contains(model.UpgradeComponentModes[ci.Component], session.Mode) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "mode " + session.Mode + " has no " + ci.Component + " component"})
			return
		}
		if session.ComponentImage(ci.Component) == ci.Image {
			c.JSON(http.StatusBadRequest, gin.H{"error": ci.Component + " is already on that image"})
			return
		}
	}

	// Every image must be one its component's registry actually serves.
	for _, ci := range req.Components {
		client := h.ghcrFor(ci.Component)
		if client == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image source not configured for " + ci.Component})
			return
		}
		tags, err := client.ListTags(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch " + ci.Component + " images: " + err.Error()})
			return
		}
		if !slices.ContainsFunc(tags, func(t ghcr.ImageTag) bool { return t.Image == ci.Image }) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "image not found in " + ci.Component + " registry: " + ci.Image})
			return
		}
	}

	// A user batch that paused on a failure would hold its remaining pending
	// tasks forever; a fresh request supersedes it. Only the caller's own
	// batches — a paused ADMIN batch is the admin's to resume, and its pending
	// tasks make the in-flight guard below answer 409.
	err := h.db.Transaction(func(tx *gorm.DB) error {
		var pausedIDs []uint
		if err := tx.Model(&model.UpgradeBatch{}).
			Where("user_id = ? AND status = ? AND id IN (?)", userID, model.UpgradeBatchPaused,
				tx.Model(&model.UpgradeTask{}).Select("batch_id").Where("session_id = ?", session.ID)).
			Pluck("id", &pausedIDs).Error; err != nil {
			return err
		}
		if len(pausedIDs) == 0 {
			return nil
		}
		if err := tx.Model(&model.UpgradeBatch{}).Where("id IN ?", pausedIDs).
			Update("status", model.UpgradeBatchCancelled).Error; err != nil {
			return err
		}
		return tx.Model(&model.UpgradeTask{}).
			Where("batch_id IN ? AND status = ?", pausedIDs, model.UpgradeTaskPending).
			Updates(map[string]any{"status": model.UpgradeTaskSkipped, "error_msg": "superseded by a newer upgrade"}).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create upgrade"})
		return
	}

	// In-flight guard: one change at a time per session, whoever started it.
	var inFlight int64
	if err := h.db.Model(&model.UpgradeTask{}).
		Where("session_id = ? AND status IN ?", session.ID,
			[]string{model.UpgradeTaskPending, model.UpgradeTaskRunning}).
		Count(&inFlight).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create upgrade"})
		return
	}
	if inFlight > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "an upgrade for this session is already in progress"})
		return
	}

	batch := model.UpgradeBatch{
		UserID: &userID,
		Status: model.UpgradeBatchRunning,
	}
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&batch).Error; err != nil {
			return err
		}
		tasks := make([]model.UpgradeTask, len(req.Components))
		for i, ci := range req.Components {
			tasks[i] = model.UpgradeTask{
				BatchID:   batch.ID,
				SessionID: session.ID,
				Component: ci.Component,
				FromImage: session.ComponentImage(ci.Component),
				ToImage:   ci.Image,
				Status:    model.UpgradeTaskPending,
			}
		}
		return tx.Create(&tasks).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create upgrade"})
		return
	}

	h.runner.Start(batch.ID)
	c.JSON(http.StatusCreated, h.sessionUpgradeJSON(&batch, session))
}

// GetForSession — GET /api/v1/setup/:id/upgrade (user only). The caller's
// latest upgrade of this session, with per-component task state — the polling
// endpoint behind the frontend's progress view. 404 until the user has
// upgraded the session at least once. Admin-initiated batches are not shown.
func (h *UpgradeHandler) GetForSession(c *gin.Context) {
	session, userID, ok := h.ownSession(c)
	if !ok {
		return
	}

	var batch model.UpgradeBatch
	err := h.db.
		Where("user_id = ? AND id IN (?)", userID,
			h.db.Model(&model.UpgradeTask{}).Select("batch_id").Where("session_id = ?", session.ID)).
		Order("id desc").First(&batch).Error
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no upgrade for this session"})
		return
	}
	c.JSON(http.StatusOK, h.sessionUpgradeJSON(&batch, session))
}

func (h *UpgradeHandler) sessionUpgradeJSON(batch *model.UpgradeBatch, session *model.SetupSession) gin.H {
	var tasks []model.UpgradeTask
	if err := h.db.Where("batch_id = ? AND session_id = ?", batch.ID, session.ID).
		Order("id").Find(&tasks).Error; err != nil {
		tasks = nil
	}

	type taskItem struct {
		Component string `json:"component"`
		FromImage string `json:"from_image"`
		ToImage   string `json:"to_image"`
		Status    string `json:"status"`
		ErrorMsg  string `json:"error_msg,omitempty"`
		UpdatedAt string `json:"updated_at"`
	}
	items := make([]taskItem, len(tasks))
	for i, t := range tasks {
		items[i] = taskItem{
			Component: t.Component,
			FromImage: t.FromImage,
			ToImage:   t.ToImage,
			Status:    t.Status,
			ErrorMsg:  t.ErrorMsg,
			UpdatedAt: t.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
	}
	return gin.H{
		"id":         batch.ID,
		"status":     batch.Status,
		"components": taskComponents(tasks),
		"created_at": batch.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"tasks":      items,
	}
}
