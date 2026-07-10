package handler

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/ghcr"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/upgrade"
)

// UpgradeHandler serves the admin batch-upgrade API. The heavy lifting
// happens in internal/upgrade's background runner; these handlers only
// validate, resolve targets, and read progress back out of the DB.
type UpgradeHandler struct {
	db     *gorm.DB
	runner *upgrade.Runner // nil when K8s isn't configured

	ghcr         *ghcr.Client // nil when not configured
	mediatorGhcr *ghcr.Client
	didsGhcr     *ghcr.Client
	vtcGhcr      *ghcr.Client
}

func NewUpgradeHandler(
	db *gorm.DB,
	runner *upgrade.Runner,
	ghcrClient, mediatorGhcrClient, didsGhcrClient, vtcGhcrClient *ghcr.Client,
) *UpgradeHandler {
	return &UpgradeHandler{
		db:           db,
		runner:       runner,
		ghcr:         ghcrClient,
		mediatorGhcr: mediatorGhcrClient,
		didsGhcr:     didsGhcrClient,
		vtcGhcr:      vtcGhcrClient,
	}
}

func (h *UpgradeHandler) ghcrFor(component string) *ghcr.Client {
	switch component {
	case "vta":
		return h.ghcr
	case "mediator":
		return h.mediatorGhcr
	case "dids":
		return h.didsGhcr
	case "vtc":
		return h.vtcGhcr
	}
	return nil
}

type createUpgradeRequest struct {
	Component string `json:"component" binding:"required,oneof=vta mediator dids vtc"`
	Image     string `json:"image"     binding:"required"`
	// Exactly one of SessionIDs / All selects the targets. SessionIDs are the
	// sessions' 8-char unique_ids; All means every eligible session (running,
	// mode has the component, not already on Image).
	SessionIDs []string `json:"session_ids"`
	All        bool     `json:"all"`
	// DryRun resolves and returns the target list without creating anything —
	// the confirm screen's preview.
	DryRun bool `json:"dry_run"`
}

type upgradeTargetItem struct {
	SessionID string `json:"session_id"` // unique_id
	VtaName   string `json:"vta_name"`
	FromImage string `json:"from_image"`
}

type skippedItem struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
}

// Create — POST /api/v1/admin/upgrades (admin only). Validates the target
// image against the component's GHCR tag list (an admin can only roll out
// images that actually exist there), resolves the eligible sessions, then
// creates the batch and starts the background runner. With dry_run it stops
// after resolution and just reports what would happen.
func (h *UpgradeHandler) Create(c *gin.Context) {
	var req createUpgradeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.All == (len(req.SessionIDs) > 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provide either session_ids or all=true (not both)"})
		return
	}
	if !req.DryRun && h.runner == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "K8s not configured — upgrades unavailable"})
		return
	}

	// The image must be one the component's registry actually serves.
	client := h.ghcrFor(req.Component)
	if client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image source not configured for " + req.Component})
		return
	}
	tags, err := client.ListTags(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch images: " + err.Error()})
		return
	}
	known := false
	for _, t := range tags {
		if t.Image == req.Image {
			known = true
			break
		}
	}
	if !known {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image not found in " + req.Component + " registry: " + req.Image})
		return
	}

	targets, skipped, err := h.resolveTargets(&req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not resolve target sessions"})
		return
	}

	targetItems := make([]upgradeTargetItem, len(targets))
	for i, s := range targets {
		targetItems[i] = upgradeTargetItem{
			SessionID: s.UniqueId,
			VtaName:   s.VtaName,
			FromImage: s.ComponentImage(req.Component),
		}
	}

	if req.DryRun {
		c.JSON(http.StatusOK, gin.H{"targets": targetItems, "skipped": skipped})
		return
	}
	if len(targets) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no eligible sessions to upgrade", "skipped": skipped})
		return
	}

	adminID := c.MustGet(middleware.ContextUserID).(uint)
	batch := model.UpgradeBatch{
		AdminID:   adminID,
		Component: req.Component,
		Image:     req.Image,
		Status:    model.UpgradeBatchRunning,
	}
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&batch).Error; err != nil {
			return err
		}
		tasks := make([]model.UpgradeTask, len(targets))
		for i, s := range targets {
			tasks[i] = model.UpgradeTask{
				BatchID:   batch.ID,
				SessionID: s.ID,
				FromImage: s.ComponentImage(req.Component),
				Status:    model.UpgradeTaskPending,
			}
		}
		return tx.Create(&tasks).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create upgrade batch"})
		return
	}

	h.runner.Start(batch.ID)
	c.JSON(http.StatusCreated, gin.H{
		"id":        batch.ID,
		"component": batch.Component,
		"image":     batch.Image,
		"status":    batch.Status,
		"targets":   targetItems,
		"skipped":   skipped,
	})
}

// resolveTargets turns the request's selection into concrete sessions to
// upgrade plus per-session skip reasons. Explicitly selected sessions get a
// reason for every exclusion; all=true silently excludes non-candidates.
func (h *UpgradeHandler) resolveTargets(req *createUpgradeRequest) ([]model.SetupSession, []skippedItem, error) {
	modes := model.UpgradeComponentModes[req.Component]
	skipped := []skippedItem{}

	if req.All {
		var sessions []model.SetupSession
		err := h.db.Where("status = ? AND mode IN ?", "running", modes).
			Where(model.UpgradeImageColumn(req.Component)+" != ?", req.Image).
			Order("id").Find(&sessions).Error
		return sessions, skipped, err
	}

	var found []model.SetupSession
	if err := h.db.Where("unique_id IN ?", req.SessionIDs).Order("id").Find(&found).Error; err != nil {
		return nil, nil, err
	}
	byUniqueId := make(map[string]*model.SetupSession, len(found))
	for i := range found {
		byUniqueId[found[i].UniqueId] = &found[i]
	}

	targets := make([]model.SetupSession, 0, len(found))
	for _, id := range req.SessionIDs {
		s, ok := byUniqueId[id]
		switch {
		case !ok:
			skipped = append(skipped, skippedItem{SessionID: id, Reason: "session not found"})
		case s.Status != "running":
			skipped = append(skipped, skippedItem{SessionID: id, Reason: "session is " + s.Status + ", not running"})
		case !slices.Contains(modes, s.Mode):
			skipped = append(skipped, skippedItem{SessionID: id, Reason: "mode " + s.Mode + " has no " + req.Component + " component"})
		case s.ComponentImage(req.Component) == req.Image:
			skipped = append(skipped, skippedItem{SessionID: id, Reason: "already on the target image"})
		default:
			targets = append(targets, *s)
		}
	}
	return targets, skipped, nil
}

// List — GET /api/v1/admin/upgrades (admin only). The 20 most recent batches
// with per-status task counts, newest first.
func (h *UpgradeHandler) List(c *gin.Context) {
	var batches []model.UpgradeBatch
	if err := h.db.Order("id desc").Limit(20).Find(&batches).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch upgrade batches"})
		return
	}

	counts := make(map[uint]map[string]int64)
	if len(batches) > 0 {
		ids := make([]uint, len(batches))
		for i, b := range batches {
			ids[i] = b.ID
		}
		var rows []struct {
			BatchID uint
			Status  string
			N       int64
		}
		if err := h.db.Model(&model.UpgradeTask{}).
			Select("batch_id, status, COUNT(*) as n").
			Where("batch_id IN ?", ids).
			Group("batch_id, status").Scan(&rows).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch task counts"})
			return
		}
		for _, row := range rows {
			if counts[row.BatchID] == nil {
				counts[row.BatchID] = make(map[string]int64)
			}
			counts[row.BatchID][row.Status] = row.N
		}
	}

	type batchItem struct {
		model.UpgradeBatch
		TaskCounts map[string]int64 `json:"task_counts"`
	}
	items := make([]batchItem, len(batches))
	for i, b := range batches {
		tc := counts[b.ID]
		if tc == nil {
			tc = map[string]int64{}
		}
		items[i] = batchItem{UpgradeBatch: b, TaskCounts: tc}
	}
	c.JSON(http.StatusOK, items)
}

// Get — GET /api/v1/admin/upgrades/:id (admin only). Full batch progress:
// every task with its session's identifiers, for the polling progress view.
func (h *UpgradeHandler) Get(c *gin.Context) {
	batch, ok := h.loadBatch(c)
	if !ok {
		return
	}

	var tasks []model.UpgradeTask
	if err := h.db.Where("batch_id = ?", batch.ID).Order("id").Find(&tasks).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch tasks"})
		return
	}

	sessionIDs := make([]uint, len(tasks))
	for i, t := range tasks {
		sessionIDs[i] = t.SessionID
	}
	sessionInfo := make(map[uint]*model.SetupSession, len(sessionIDs))
	if len(sessionIDs) > 0 {
		var sessions []model.SetupSession
		if err := h.db.Where("id IN ?", sessionIDs).Find(&sessions).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch sessions"})
			return
		}
		for i := range sessions {
			sessionInfo[sessions[i].ID] = &sessions[i]
		}
	}

	type taskItem struct {
		SessionID string `json:"session_id"` // unique_id; "" if session deleted
		VtaName   string `json:"vta_name,omitempty"`
		FromImage string `json:"from_image"`
		Status    string `json:"status"`
		ErrorMsg  string `json:"error_msg,omitempty"`
		UpdatedAt string `json:"updated_at"`
	}
	items := make([]taskItem, len(tasks))
	for i, t := range tasks {
		item := taskItem{
			FromImage: t.FromImage,
			Status:    t.Status,
			ErrorMsg:  t.ErrorMsg,
			UpdatedAt: t.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if s := sessionInfo[t.SessionID]; s != nil {
			item.SessionID = s.UniqueId
			item.VtaName = s.VtaName
		}
		items[i] = item
	}

	c.JSON(http.StatusOK, gin.H{
		"id":         batch.ID,
		"component":  batch.Component,
		"image":      batch.Image,
		"status":     batch.Status,
		"created_at": batch.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"tasks":      items,
	})
}

// Cancel — POST /api/v1/admin/upgrades/:id/cancel (admin only). Pending tasks
// are skipped; tasks already in flight finish on their own (their result is
// still recorded, we just stop feeding the queue).
func (h *UpgradeHandler) Cancel(c *gin.Context) {
	batch, ok := h.loadBatch(c)
	if !ok {
		return
	}
	if batch.Status != model.UpgradeBatchRunning && batch.Status != model.UpgradeBatchPaused {
		c.JSON(http.StatusConflict, gin.H{"error": "batch is already " + batch.Status})
		return
	}

	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.UpgradeBatch{}).Where("id = ?", batch.ID).
			Update("status", model.UpgradeBatchCancelled).Error; err != nil {
			return err
		}
		return tx.Model(&model.UpgradeTask{}).
			Where("batch_id = ? AND status = ?", batch.ID, model.UpgradeTaskPending).
			Updates(map[string]any{"status": model.UpgradeTaskSkipped, "error_msg": "batch cancelled"}).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not cancel batch"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": batch.ID, "status": model.UpgradeBatchCancelled})
}

// Resume — POST /api/v1/admin/upgrades/:id/resume (admin only). Re-opens a
// paused batch: failed tasks stay failed, remaining pending tasks are
// scheduled again.
func (h *UpgradeHandler) Resume(c *gin.Context) {
	if h.runner == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "K8s not configured — upgrades unavailable"})
		return
	}
	batch, ok := h.loadBatch(c)
	if !ok {
		return
	}
	if batch.Status != model.UpgradeBatchPaused {
		c.JSON(http.StatusConflict, gin.H{"error": "only paused batches can be resumed (batch is " + batch.Status + ")"})
		return
	}

	if err := h.db.Model(&model.UpgradeBatch{}).Where("id = ?", batch.ID).
		Update("status", model.UpgradeBatchRunning).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not resume batch"})
		return
	}
	h.runner.Start(batch.ID)
	c.JSON(http.StatusOK, gin.H{"id": batch.ID, "status": model.UpgradeBatchRunning})
}

func (h *UpgradeHandler) loadBatch(c *gin.Context) (*model.UpgradeBatch, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id"})
		return nil, false
	}
	var batch model.UpgradeBatch
	if err := h.db.First(&batch, uint(id)).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upgrade batch not found"})
		return nil, false
	}
	return &batch, true
}
