package handler

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/k8s"
	"github.com/ic3software/cipherportal-api/internal/model"
)

type PodHandler struct {
	db        *gorm.DB
	k8sClient *k8s.Client
}

func NewPodHandler(db *gorm.DB, k8sClient *k8s.Client) *PodHandler {
	return &PodHandler{db: db, k8sClient: k8sClient}
}

func (h *PodHandler) k8sRequired(c *gin.Context) bool {
	if h.k8sClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "kubernetes not configured"})
		return false
	}
	return true
}

// POST /api/v1/pods
// Body: { "user_id": 1, "yaml": "<pod yaml>" }
func (h *PodHandler) Create(c *gin.Context) {
	if !h.k8sRequired(c) {
		return
	}

	var req struct {
		UserID      uint   `json:"user_id" binding:"required"`
		YAMLContent string `json:"yaml"    binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userIDStr := fmt.Sprintf("%d", req.UserID)
	ctx := c.Request.Context()

	if err := h.k8sClient.EnsureUserEnvironment(ctx, userIDStr); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to setup user namespace: " + err.Error()})
		return
	}

	pod, err := h.k8sClient.CreatePodFromYAML(ctx, userIDStr, req.YAMLContent)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	record := model.PodDeployment{
		UserID:      req.UserID,
		Name:        pod.Name,
		Namespace:   pod.Namespace,
		YAMLContent: req.YAMLContent,
		Status:      "created",
	}
	if err := h.db.Create(&record).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist record"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":        record.ID,
		"name":      pod.Name,
		"namespace": pod.Namespace,
		"status":    string(pod.Status.Phase),
	})
}

// GET /api/v1/pods?user_id=...
func (h *PodHandler) List(c *gin.Context) {
	if !h.k8sRequired(c) {
		return
	}

	userIDStr := c.Query("user_id")
	if userIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id query param required"})
		return
	}
	if _, err := strconv.ParseUint(userIDStr, 10, 64); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id must be a positive integer"})
		return
	}

	pods, err := h.k8sClient.ListPods(c.Request.Context(), userIDStr)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	type podSummary struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Status    string `json:"status"`
	}
	result := make([]podSummary, 0, len(pods))
	for _, p := range pods {
		result = append(result, podSummary{
			Name:      p.Name,
			Namespace: p.Namespace,
			Status:    string(p.Status.Phase),
		})
	}
	c.JSON(http.StatusOK, result)
}

// GET /api/v1/pods/:name?user_id=...
func (h *PodHandler) Get(c *gin.Context) {
	if !h.k8sRequired(c) {
		return
	}

	userIDStr := c.Query("user_id")
	if userIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id query param required"})
		return
	}
	if _, err := strconv.ParseUint(userIDStr, 10, 64); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id must be a positive integer"})
		return
	}

	pod, err := h.k8sClient.GetPod(c.Request.Context(), userIDStr, c.Param("name"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"name":       pod.Name,
		"namespace":  pod.Namespace,
		"status":     string(pod.Status.Phase),
		"conditions": pod.Status.Conditions,
	})
}

// DELETE /api/v1/pods/:name?user_id=...
func (h *PodHandler) Delete(c *gin.Context) {
	if !h.k8sRequired(c) {
		return
	}

	userIDStr := c.Query("user_id")
	if userIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id query param required"})
		return
	}
	userID, err := strconv.ParseUint(userIDStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id must be a positive integer"})
		return
	}

	podName := c.Param("name")
	if err := h.k8sClient.DeletePod(c.Request.Context(), userIDStr, podName); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	h.db.Model(&model.PodDeployment{}).
		Where("name = ? AND user_id = ?", podName, uint(userID)).
		Update("status", "deleted")

	c.JSON(http.StatusOK, gin.H{"message": "pod deleted"})
}
