package handler

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/placement"
)

type PlacementHandler struct {
	service *placement.Service
	cfg     config.PlacementConfig
}

func NewPlacementHandler(service *placement.Service, cfg config.PlacementConfig) *PlacementHandler {
	return &PlacementHandler{service: service, cfg: cfg}
}

type placementPlanRequest struct {
	Kind  string               `json:"kind" binding:"required,oneof=reconcile relocate"`
	All   bool                 `json:"all"`
	Items []placement.PlanItem `json:"items"`
}

func (h *PlacementHandler) Overview(c *gin.Context) {
	if h.service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "placement service unavailable"})
		return
	}
	overview, err := h.service.Overview(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, overview)
}

func (h *PlacementHandler) Preview(c *gin.Context) {
	var req placementPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Kind == model.PlacementPlanReconcile {
		req.All = true
	}
	if req.Kind == model.PlacementPlanRelocate && (req.All || len(req.Items) == 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "relocation requires explicit items"})
		return
	}
	items, err := h.service.Preview(c.Request.Context(), req.Items, req.All)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"mode": h.cfg.Mode, "items": items})
}

func (h *PlacementHandler) Create(c *gin.Context) {
	if h.cfg.Mode == config.PlacementDisabled {
		c.JSON(http.StatusConflict, gin.H{"error": "placement activation mode is disabled"})
		return
	}
	var req placementPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Kind == model.PlacementPlanReconcile {
		req.All = true
	}
	if req.Kind == model.PlacementPlanRelocate && (req.All || len(req.Items) == 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "relocation requires explicit items"})
		return
	}
	adminID := c.MustGet(middleware.ContextUserID).(uint)
	plan, preview, err := h.service.CreatePlan(c.Request.Context(), adminID, req.Kind, req.Items, req.All)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "items": preview})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"plan": plan, "items": preview})
}

func (h *PlacementHandler) List(c *gin.Context) {
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page must be a positive integer"})
		return
	}
	pageSize, err := strconv.Atoi(c.DefaultQuery("page_size", "10"))
	if err != nil || (pageSize != 10 && pageSize != 20 && pageSize != 50) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page_size must be one of 10, 20, or 50"})
		return
	}
	plans, total, hasRunningPlan, err := h.service.ListPlans(page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not list placement plans"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"plans": plans, "total": total, "page": page, "page_size": pageSize, "has_running_plan": hasRunningPlan,
	})
}

func (h *PlacementHandler) Get(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan id"})
		return
	}
	plan, err := h.service.GetPlan(uint(id))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "placement plan not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load placement plan"})
		return
	}
	c.JSON(http.StatusOK, plan)
}

func (h *PlacementHandler) Cancel(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan id"})
		return
	}
	adminID := c.MustGet(middleware.ContextUserID).(uint)
	if err := h.service.CancelPlan(uint(id), adminID); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *PlacementHandler) Retry(c *gin.Context) {
	planID, planErr := strconv.ParseUint(c.Param("id"), 10, 64)
	taskID, taskErr := strconv.ParseUint(c.Param("task_id"), 10, 64)
	if planErr != nil || taskErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan or task id"})
		return
	}
	var req struct {
		TargetNode string `json:"target_node"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	adminID := c.MustGet(middleware.ContextUserID).(uint)
	if err := h.service.RetryTask(uint(planID), uint(taskID), adminID, req.TargetNode); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *PlacementHandler) Rollback(c *gin.Context) {
	planID, planErr := strconv.ParseUint(c.Param("id"), 10, 64)
	taskID, taskErr := strconv.ParseUint(c.Param("task_id"), 10, 64)
	if planErr != nil || taskErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan or task id"})
		return
	}
	adminID := c.MustGet(middleware.ContextUserID).(uint)
	if err := h.service.RollbackTask(c.Request.Context(), uint(planID), uint(taskID), adminID); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
