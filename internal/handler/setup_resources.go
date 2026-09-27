package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
)

const maxResourceBatchSessions = 50

type workloadTarget struct {
	Component  string
	Deployment string
	Selector   string
}

type memoryResourceInput struct {
	Component     string `json:"component" binding:"required"`
	MemoryRequest string `json:"memory_request" binding:"required"`
	MemoryLimit   string `json:"memory_limit" binding:"required"`
}

type resourceBatchInput struct {
	SessionIDs []string              `json:"session_ids" binding:"required"`
	Resources  []memoryResourceInput `json:"resources" binding:"required"`
}

type plannedResourceChange struct {
	session   *model.SetupSession
	target    workloadTarget
	namespace string
	input     memoryResourceInput
	old       k8s.ResourceProfile
}

type memoryDesiredState struct {
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

type workloadResourceView struct {
	Component  string              `json:"component"`
	Defaults   k8s.ResourceProfile `json:"defaults"`
	Desired    memoryDesiredState  `json:"desired"`
	Actual     k8s.ResourceProfile `json:"actual"`
	Status     string              `json:"status"`
	ApplyError string              `json:"apply_error,omitempty"`
}

func sessionWorkloadTargets(session *model.SetupSession) []workloadTarget {
	if !session.IsFullStack() {
		return []workloadTarget{{
			Component:  k8s.ComponentVTA,
			Deployment: k8s.VtaDeploymentName(session.ID),
			Selector:   fmt.Sprintf("app=vta,session-id=%d", session.ID),
		}}
	}
	return []workloadTarget{
		{Component: k8s.ComponentVTA, Deployment: k8s.FSVtaName(session.ID), Selector: fmt.Sprintf("app=fs-vta,session-id=%d", session.ID)},
		{Component: k8s.ComponentMediator, Deployment: k8s.FSMediatorName(session.ID), Selector: fmt.Sprintf("app=fs-mediator,session-id=%d", session.ID)},
		{Component: k8s.ComponentDids, Deployment: k8s.FSDidsName(session.ID), Selector: fmt.Sprintf("app=fs-dids,session-id=%d", session.ID)},
		{Component: k8s.ComponentVTC, Deployment: k8s.FSVtcName(session.ID), Selector: fmt.Sprintf("app=fs-vtc,session-id=%d", session.ID)},
	}
}

func sessionWorkloadTarget(session *model.SetupSession, component string) (workloadTarget, bool) {
	for _, target := range sessionWorkloadTargets(session) {
		if target.Component == component {
			return target, true
		}
	}
	return workloadTarget{}, false
}

// AdminSessionResources returns DB desired state beside the live Deployment
// template. The first read imports existing Deployments so legacy sessions are
// not silently replaced with today's defaults.
func (h *SetupHandler) AdminSessionResources(c *gin.Context) {
	if h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s not configured"})
		return
	}

	session, ok := h.adminResourceSession(c, c.Param("id"))
	if !ok {
		return
	}
	namespace := h.k8s.UserNamespace(strconv.FormatUint(uint64(session.UserID), 10))

	var stored []model.WorkloadResource
	if err := h.db.Where("setup_session_id = ?", session.ID).Find(&stored).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load resource settings"})
		return
	}
	byComponent := make(map[string]model.WorkloadResource, len(stored))
	for _, item := range stored {
		byComponent[item.Component] = item
	}

	views := make([]workloadResourceView, 0, len(sessionWorkloadTargets(session)))
	for _, target := range sessionWorkloadTargets(session) {
		actual, err := h.k8s.DeploymentResourceProfile(c.Request.Context(), namespace, target.Deployment)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		storedResource, exists := byComponent[target.Component]
		if !exists {
			storedResource = model.WorkloadResource{
				SetupSessionID: session.ID,
				Component:      target.Component,
				MemoryRequest:  actual.MemoryRequest,
				MemoryLimit:    actual.MemoryLimit,
			}
			if err := h.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&storedResource).Error; err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "could not import resource settings"})
				return
			}
			if err := h.db.Where("setup_session_id = ? AND component = ?", session.ID, target.Component).
				First(&storedResource).Error; err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read imported resource settings"})
				return
			}
		}

		defaults, _ := k8s.DefaultResourceProfile(target.Component)
		status := "in_sync"
		if !sameMemory(storedResource.MemoryRequest, actual.MemoryRequest) ||
			!sameMemory(storedResource.MemoryLimit, actual.MemoryLimit) {
			status = "drifted"
		}
		if storedResource.ApplyError != "" {
			status = "failed"
		}
		views = append(views, workloadResourceView{
			Component: target.Component,
			Defaults:  defaults,
			Desired: memoryDesiredState{
				MemoryRequest: storedResource.MemoryRequest,
				MemoryLimit:   storedResource.MemoryLimit,
			},
			Actual:     actual,
			Status:     status,
			ApplyError: storedResource.ApplyError,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"session_id": session.VtaName,
		"mode":       session.Mode,
		"resources":  views,
	})
}

// AdminApplySessionResources validates every requested change first, including
// Kubernetes server-side dry-run. Only then is desired state saved and the
// Deployment templates updated.
func (h *SetupHandler) AdminApplySessionResources(c *gin.Context) {
	if h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s not configured"})
		return
	}

	var input resourceBatchInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_ids and resources are required"})
		return
	}
	if err := validateResourceBatch(&input); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	var sessions []model.SetupSession
	if err := h.db.Where("vta_name IN ?", input.SessionIDs).Find(&sessions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load sessions"})
		return
	}
	if len(sessions) != len(input.SessionIDs) {
		c.JSON(http.StatusNotFound, gin.H{"error": "one or more sessions were not found"})
		return
	}
	sessionByName := make(map[string]*model.SetupSession, len(sessions))
	for i := range sessions {
		session := &sessions[i]
		if session.Status != "running" {
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("session %s is not running", session.VtaName)})
			return
		}
		sessionByName[session.VtaName] = session
	}

	changes := make([]plannedResourceChange, 0, len(input.SessionIDs)*len(input.Resources))
	matchedComponents := make(map[string]bool, len(input.Resources))
	for _, sessionID := range input.SessionIDs {
		session := sessionByName[sessionID]
		namespace := h.k8s.UserNamespace(strconv.FormatUint(uint64(session.UserID), 10))
		for _, requested := range input.Resources {
			target, exists := sessionWorkloadTarget(session, requested.Component)
			if !exists {
				continue
			}
			matchedComponents[requested.Component] = true
			old, err := h.k8s.DeploymentResourceProfile(c.Request.Context(), namespace, target.Deployment)
			if err != nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
				return
			}
			if err := h.k8s.DryRunDeploymentMemoryUpdate(c.Request.Context(), namespace, target.Deployment, requested.MemoryRequest, requested.MemoryLimit); err != nil {
				c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Kubernetes rejected the change: " + err.Error()})
				return
			}
			changes = append(changes, plannedResourceChange{session: session, target: target, namespace: namespace, input: requested, old: old})
		}
	}
	for _, requested := range input.Resources {
		if !matchedComponents[requested.Component] {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error": fmt.Sprintf("component %s does not exist in the selected sessions", requested.Component),
			})
			return
		}
	}
	if err := h.validateResourceCapacity(c, changes); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	if err := h.db.Transaction(func(tx *gorm.DB) error {
		for _, change := range changes {
			item := model.WorkloadResource{
				SetupSessionID: change.session.ID,
				Component:      change.target.Component,
				MemoryRequest:  change.input.MemoryRequest,
				MemoryLimit:    change.input.MemoryLimit,
				ApplyError:     "",
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "setup_session_id"}, {Name: "component"}},
				DoUpdates: clause.AssignmentColumns([]string{
					"memory_request", "memory_limit", "apply_error", "updated_at",
				}),
			}).Create(&item).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not save resource settings"})
		return
	}

	type result struct {
		SessionID  string `json:"session_id"`
		Component  string `json:"component"`
		Status     string `json:"status"`
		ApplyError string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(changes))
	stopReason := ""
	for _, change := range changes {
		resultItem := result{SessionID: change.session.VtaName, Component: change.target.Component, Status: "applied"}
		if stopReason != "" {
			resultItem.Status = "skipped"
			resultItem.ApplyError = stopReason
			h.setResourceApplyError(change, stopReason)
			results = append(results, resultItem)
			continue
		}

		applyCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		err := h.k8s.UpdateDeploymentMemory(applyCtx, change.namespace, change.target.Deployment, change.input.MemoryRequest, change.input.MemoryLimit)
		if err == nil {
			err = h.k8s.WaitForComponentDeploymentReadyOrRestarts(
				applyCtx, change.namespace, change.target.Deployment, change.target.Selector, 3*time.Minute, 3,
			)
		}
		cancel()
		if err != nil {
			resultItem.Status = "failed"
			resultItem.ApplyError = err.Error()
			if rollbackErr := h.rollbackResourceChange(change); rollbackErr != nil {
				resultItem.ApplyError += "; rollback failed: " + rollbackErr.Error()
			}
			h.setResourceApplyError(change, resultItem.ApplyError)
			stopReason = "not applied after an earlier rollout failed"
		}
		results = append(results, resultItem)
	}

	c.JSON(http.StatusOK, gin.H{"results": results})
}

func (h *SetupHandler) rollbackResourceChange(change plannedResourceChange) error {
	if err := k8s.ValidateMemoryResources(change.old.MemoryRequest, change.old.MemoryLimit); err != nil {
		return fmt.Errorf("previous resources are invalid: %w", err)
	}
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := h.k8s.UpdateDeploymentMemory(rollbackCtx, change.namespace, change.target.Deployment, change.old.MemoryRequest, change.old.MemoryLimit); err != nil {
		return err
	}
	return h.k8s.WaitForComponentDeploymentReadyOrRestarts(
		rollbackCtx, change.namespace, change.target.Deployment, change.target.Selector, 3*time.Minute, 3,
	)
}

func (h *SetupHandler) setResourceApplyError(change plannedResourceChange, message string) {
	h.db.Model(&model.WorkloadResource{}).
		Where("setup_session_id = ? AND component = ?", change.session.ID, change.target.Component).
		Update("apply_error", message)
}

func (h *SetupHandler) adminResourceSession(c *gin.Context, publicID string) (*model.SetupSession, bool) {
	var session model.SetupSession
	if err := h.db.Where("vta_name = ?", publicID).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load session"})
		}
		return nil, false
	}
	if session.Status != "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "resources can only be changed for a running session"})
		return nil, false
	}
	return &session, true
}

func validateResourceBatch(input *resourceBatchInput) error {
	if len(input.SessionIDs) == 0 || len(input.SessionIDs) > maxResourceBatchSessions {
		return fmt.Errorf("session_ids must contain between 1 and %d sessions", maxResourceBatchSessions)
	}
	if len(input.Resources) == 0 || len(input.Resources) > 4 {
		return fmt.Errorf("resources must contain between 1 and 4 components")
	}
	seenSessions := map[string]bool{}
	for i, id := range input.SessionIDs {
		id = strings.TrimSpace(id)
		if id == "" || seenSessions[id] {
			return fmt.Errorf("session_ids must be non-empty and unique")
		}
		seenSessions[id] = true
		input.SessionIDs[i] = id
	}
	seenComponents := map[string]bool{}
	for i := range input.Resources {
		item := &input.Resources[i]
		item.Component = strings.TrimSpace(item.Component)
		if _, ok := k8s.DefaultResourceProfile(item.Component); !ok || seenComponents[item.Component] {
			return fmt.Errorf("resources must contain unique known components")
		}
		seenComponents[item.Component] = true
		if err := k8s.ValidateMemoryResources(item.MemoryRequest, item.MemoryLimit); err != nil {
			return fmt.Errorf("%s: %w", item.Component, err)
		}
		request, _ := resource.ParseQuantity(strings.TrimSpace(item.MemoryRequest))
		limit, _ := resource.ParseQuantity(strings.TrimSpace(item.MemoryLimit))
		maxRequest := resource.MustParse("512Mi")
		if request.Cmp(maxRequest) > 0 {
			return fmt.Errorf("%s: memory_request must not exceed 512Mi", item.Component)
		}
		item.MemoryRequest = request.String()
		item.MemoryLimit = limit.String()
	}
	return nil
}

func sameMemory(left, right string) bool {
	a, errA := resource.ParseQuantity(left)
	b, errB := resource.ParseQuantity(right)
	return errA == nil && errB == nil && a.Cmp(b) == 0
}

func (h *SetupHandler) validateResourceCapacity(c *gin.Context, changes []plannedResourceChange) error {
	stats, err := h.k8s.ClusterResourceStats(c.Request.Context())
	if err != nil {
		return fmt.Errorf("could not check cluster memory capacity: %w", err)
	}
	var free, largestNode, increase int64
	for _, node := range stats.Nodes {
		if !node.Schedulable {
			continue
		}
		free += max(node.MemAllocatableBytes-node.MemRequestedBytes, 0)
		largestNode = max(largestNode, node.MemAllocatableBytes)
	}
	if largestNode == 0 {
		return fmt.Errorf("cluster has no schedulable nodes")
	}
	for _, change := range changes {
		requested, _ := resource.ParseQuantity(change.input.MemoryRequest)
		limit, _ := resource.ParseQuantity(change.input.MemoryLimit)
		old, _ := resource.ParseQuantity(change.old.MemoryRequest)
		if requested.Value() > largestNode {
			return fmt.Errorf("%s memory request exceeds every schedulable node", change.target.Component)
		}
		if limit.Value() > largestNode {
			return fmt.Errorf("%s memory limit exceeds every schedulable node", change.target.Component)
		}
		increase += max(requested.Value()-old.Value(), 0)
	}
	if increase > free {
		return fmt.Errorf("resource changes need %s additional requested memory but the cluster has %s available", resource.NewQuantity(increase, resource.BinarySI).String(), resource.NewQuantity(free, resource.BinarySI).String())
	}
	return nil
}
