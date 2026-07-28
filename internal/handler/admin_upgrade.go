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

type componentImage struct {
	Component string `json:"component" binding:"required,oneof=vta mediator dids vtc"`
	Image     string `json:"image"     binding:"required"`
}

type createUpgradeRequest struct {
	// One target image per component to upgrade — a session gets one task per
	// listed component its mode runs, so a full_stack session can have its
	// vta + mediator + dids upgraded in a single batch.
	Components []componentImage `json:"components" binding:"required,min=1,dive"`
	// Exactly one of SessionIDs / All selects the targets. SessionIDs are the
	// sessions' names; All means every eligible session (running,
	// mode has the component, not already on the target image).
	SessionIDs []string `json:"session_ids"`
	All        bool     `json:"all"`
	// DryRun resolves and returns the target list without creating anything —
	// the confirm screen's preview.
	DryRun bool `json:"dry_run"`
}

type upgradeTargetItem struct {
	SessionID string `json:"session_id"` // vta_name
	VtaName   string `json:"vta_name"`
	Component string `json:"component"`
	FromImage string `json:"from_image"`
	ToImage   string `json:"to_image"`
}

type skippedItem struct {
	SessionID string `json:"session_id"`
	Component string `json:"component,omitempty"`
	Reason    string `json:"reason"`
}

// upgradeTargetPair is an internal (session, component) resolution result.
type upgradeTargetPair struct {
	session   model.SetupSession
	component string
	image     string
}

// Create — POST /api/v1/admin/upgrades (admin only). Validates every target
// image against its component's GHCR tag list (an admin can only roll out
// images that actually exist there), resolves the eligible (session,
// component) pairs, then creates the batch and starts the background runner.
// With dry_run it stops after resolution and just reports what would happen.
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
	seen := map[string]bool{}
	for _, ci := range req.Components {
		if seen[ci.Component] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate component " + ci.Component})
			return
		}
		seen[ci.Component] = true
	}
	if !req.DryRun && h.runner == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "K8s not configured — upgrades unavailable"})
		return
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

	targets, skipped, err := h.resolveTargets(&req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not resolve target sessions"})
		return
	}

	targetItems := make([]upgradeTargetItem, len(targets))
	for i, t := range targets {
		targetItems[i] = upgradeTargetItem{
			SessionID: t.session.VtaName,
			VtaName:   t.session.VtaName,
			Component: t.component,
			FromImage: t.session.ComponentImage(t.component),
			ToImage:   t.image,
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
		AdminID: &adminID,
		Status:  model.UpgradeBatchRunning,
	}
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&batch).Error; err != nil {
			return err
		}
		tasks := make([]model.UpgradeTask, len(targets))
		for i, t := range targets {
			tasks[i] = model.UpgradeTask{
				BatchID:   batch.ID,
				SessionID: t.session.ID,
				Component: t.component,
				FromImage: t.session.ComponentImage(t.component),
				ToImage:   t.image,
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
		"id":      batch.ID,
		"status":  batch.Status,
		"targets": targetItems,
		"skipped": skipped,
	})
}

// resolveTargets turns the request's selection into concrete (session,
// component) pairs to upgrade, plus per-pair skip reasons. Tasks are ordered
// session-major — all of one session's components upgrade back-to-back, so
// each customer's downtime window stays contiguous. Explicitly selected
// sessions get a reason for every exclusion; all=true silently excludes
// non-candidates.
func (h *UpgradeHandler) resolveTargets(req *createUpgradeRequest) ([]upgradeTargetPair, []skippedItem, error) {
	skipped := []skippedItem{}

	var sessions []model.SetupSession
	if req.All {
		if err := h.db.Where("status = ?", "running").Order("id").Find(&sessions).Error; err != nil {
			return nil, nil, err
		}
	} else {
		var found []model.SetupSession
		if err := h.db.Where("vta_name IN ?", req.SessionIDs).Order("id").Find(&found).Error; err != nil {
			return nil, nil, err
		}
		byName := make(map[string]*model.SetupSession, len(found))
		for i := range found {
			byName[found[i].VtaName] = &found[i]
		}
		for _, id := range req.SessionIDs {
			s, ok := byName[id]
			switch {
			case !ok:
				skipped = append(skipped, skippedItem{SessionID: id, Reason: "session not found"})
			case s.Status != "running":
				skipped = append(skipped, skippedItem{SessionID: id, Reason: "session is " + s.Status + ", not running"})
			default:
				sessions = append(sessions, *s)
			}
		}
	}

	targets := make([]upgradeTargetPair, 0, len(sessions)*len(req.Components))
	for _, s := range sessions {
		for _, ci := range req.Components {
			switch {
			case !slices.Contains(model.UpgradeComponentModes[ci.Component], s.Mode):
				if !req.All {
					skipped = append(skipped, skippedItem{SessionID: s.VtaName, Component: ci.Component,
						Reason: "mode " + s.Mode + " has no " + ci.Component + " component"})
				}
			case s.ComponentImage(ci.Component) == ci.Image:
				if !req.All {
					skipped = append(skipped, skippedItem{SessionID: s.VtaName, Component: ci.Component,
						Reason: "already on the target image"})
				}
			default:
				targets = append(targets, upgradeTargetPair{session: s, component: ci.Component, image: ci.Image})
			}
		}
	}
	return targets, skipped, nil
}

// taskComponents returns the distinct components in a set of tasks, in
// model.UpgradeComponents order — the batch-level summary of what it touches.
func taskComponents(tasks []model.UpgradeTask) []string {
	present := map[string]bool{}
	for _, t := range tasks {
		present[t.Component] = true
	}
	out := []string{}
	for _, c := range model.UpgradeComponents {
		if present[c] {
			out = append(out, c)
		}
	}
	return out
}

// List — GET /api/v1/admin/upgrades (admin only). The 20 most recent batches
// with their components and per-status task counts, newest first.
func (h *UpgradeHandler) List(c *gin.Context) {
	var batches []model.UpgradeBatch
	if err := h.db.Order("id desc").Limit(20).Find(&batches).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch upgrade batches"})
		return
	}

	type batchItem struct {
		model.UpgradeBatch
		// "admin" for batch rollouts, "user" for a user's self-service
		// upgrade of their own session (POST /setup/:id/upgrade).
		Initiator  string           `json:"initiator"`
		Components []string         `json:"components"`
		TaskCounts map[string]int64 `json:"task_counts"`
	}
	items := make([]batchItem, len(batches))
	if len(batches) > 0 {
		ids := make([]uint, len(batches))
		for i, b := range batches {
			ids[i] = b.ID
		}
		var tasks []model.UpgradeTask
		if err := h.db.Where("batch_id IN ?", ids).Find(&tasks).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch tasks"})
			return
		}
		byBatch := make(map[uint][]model.UpgradeTask)
		for _, t := range tasks {
			byBatch[t.BatchID] = append(byBatch[t.BatchID], t)
		}
		for i, b := range batches {
			counts := map[string]int64{}
			for _, t := range byBatch[b.ID] {
				counts[t.Status]++
			}
			items[i] = batchItem{
				UpgradeBatch: b,
				Initiator:    batchInitiator(&b),
				Components:   taskComponents(byBatch[b.ID]),
				TaskCounts:   counts,
			}
		}
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
		SessionID string `json:"session_id"` // vta_name; "" if session deleted
		VtaName   string `json:"vta_name,omitempty"`
		Component string `json:"component"`
		FromImage string `json:"from_image"`
		ToImage   string `json:"to_image"`
		Status    string `json:"status"`
		ErrorMsg  string `json:"error_msg,omitempty"`
		UpdatedAt string `json:"updated_at"`
	}
	items := make([]taskItem, len(tasks))
	for i, t := range tasks {
		item := taskItem{
			Component: t.Component,
			FromImage: t.FromImage,
			ToImage:   t.ToImage,
			Status:    t.Status,
			ErrorMsg:  t.ErrorMsg,
			UpdatedAt: t.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if s := sessionInfo[t.SessionID]; s != nil {
			item.SessionID = s.VtaName
			item.VtaName = s.VtaName
		}
		items[i] = item
	}

	c.JSON(http.StatusOK, gin.H{
		"id":         batch.ID,
		"initiator":  batchInitiator(batch),
		"components": taskComponents(tasks),
		"status":     batch.Status,
		"created_at": batch.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"tasks":      items,
	})
}

// batchInitiator says who started a batch: "admin" for batch rollouts,
// "user" for a user's self-service upgrade of their own session.
func batchInitiator(b *model.UpgradeBatch) string {
	if b.UserID != nil {
		return "user"
	}
	return "admin"
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
