package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/capacity"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
)

const (
	maxLoadTestSessions = 50
	loadTestWorkers     = 10
	loadTestStaleAfter  = 5 * time.Minute
)

type createLoadTestRequest struct {
	Count    int    `json:"count"`
	AdminDid string `json:"admin_did"`
	VtaImage string `json:"vta_image"`
}

type loadTestSessionItem struct {
	ID       uint   `json:"id"`
	VtaName  string `json:"vta_name"`
	Status   string `json:"status"`
	ErrorMsg string `json:"error_msg,omitempty"`
	FQDN     string `json:"fqdn"`
}

type loadTestRunItem struct {
	ID             uint                  `json:"id"`
	Status         string                `json:"status"`
	ErrorMsg       string                `json:"error_msg,omitempty"`
	RequestedCount int                   `json:"requested_count"`
	CreatedCount   int                   `json:"created_count"`
	RunningCount   int                   `json:"running_count"`
	FailedCount    int                   `json:"failed_count"`
	VtaImage       string                `json:"vta_image"`
	CreatedAt      time.Time             `json:"created_at"`
	UpdatedAt      time.Time             `json:"updated_at"`
	Sessions       []loadTestSessionItem `json:"sessions"`
}

// AdminCreateLoadTest — POST /api/v1/admin/load-tests.
//
// Creates one isolated system user for the run, then starts up to ten ordinary
// VTA-only creates in parallel. The request returns as soon as the run has been
// recorded; GET /admin/load-tests is the progress view.
func (h *SetupHandler) AdminCreateLoadTest(c *gin.Context) {
	if !h.cfRequired(c) {
		return
	}
	if h.k8s == nil || h.orch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s/orchestrator not configured"})
		return
	}
	if h.ingressIP == "" || h.clusterDomain == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "cluster not configured: CLUSTER_INGRESS_IP and CLUSTER_DOMAIN must be set"})
		return
	}

	var req createLoadTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.AdminDid = strings.TrimSpace(req.AdminDid)
	req.VtaImage = strings.TrimSpace(req.VtaImage)
	if req.Count < 1 || req.Count > maxLoadTestSessions {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("count must be between 1 and %d", maxLoadTestSessions)})
		return
	}
	if !didKeyRe.MatchString(req.AdminDid) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "admin_did must be a did:key value produced by pnm setup"})
		return
	}
	if req.VtaImage == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "vta_image is required"})
		return
	}

	h.reconcileStaleLoadTests()
	var busy int64
	if err := h.db.Model(&model.LoadTestRun{}).
		Where("status IN ?", []string{
			model.LoadTestCreating, model.LoadTestActive, model.LoadTestPartial,
			model.LoadTestDeleting, model.LoadTestDeleteFailed,
		}).Count(&busy).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check active load tests"})
		return
	}
	if busy > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "a load test still has active resources — delete it before starting another"})
		return
	}

	infra, provider, reason, detail := h.resolveProvider("")
	if reason != "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": detail, "reason": reason})
		return
	}
	if estimates, _, ok := h.capacity.Estimates(c.Request.Context()); ok {
		if available := estimates[capacity.VtaOnly.Name].Count; req.Count > available {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":     fmt.Sprintf("the cluster currently has capacity for %d VTA-only sessions, fewer than the requested %d", available, req.Count),
				"available": available,
			})
			return
		}
	}

	adminID := c.MustGet(middleware.ContextUserID).(uint)
	run := model.LoadTestRun{
		RequestedBy:    &adminID,
		RequestedCount: req.Count,
		VtaImage:       req.VtaImage,
		Status:         model.LoadTestCreating,
	}
	if err := h.db.Create(&run).Error; err != nil {
		if isUniqueViolation(err, "load_test_runs_one_active") {
			c.JSON(http.StatusConflict, gin.H{"error": "a load test still has active resources — delete it before starting another"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create load-test run"})
		return
	}

	user := model.User{UniqueId: fmt.Sprintf("load-test-%d", run.ID)}
	if err := h.db.Create(&user).Error; err != nil {
		h.db.Model(&run).Updates(map[string]any{
			"status": model.LoadTestFailed, "error_msg": "failed to create load-test account",
		})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create load-test account"})
		return
	}
	run.UserID = &user.ID
	if err := h.db.Model(&run).Update("user_id", user.ID).Error; err != nil {
		_ = h.db.Delete(&user).Error
		h.db.Model(&run).Updates(map[string]any{
			"status": model.LoadTestFailed, "error_msg": "failed to attach load-test account",
		})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to attach load-test account"})
		return
	}

	go h.startLoadTest(run.ID, user.ID, req, infra, provider)
	c.JSON(http.StatusAccepted, gin.H{"id": run.ID, "status": run.Status})
}

func (h *SetupHandler) startLoadTest(
	runID, userID uint,
	request createLoadTestRequest,
	infra sharedInfra,
	provider *model.SetupSession,
) {
	type createResult struct{ err error }
	jobs := make(chan int)
	results := make(chan createResult, request.Count)
	workers := min(request.Count, loadTestWorkers)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sequence := range jobs {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				name := fmt.Sprintf("load-%d-%03d", runID, sequence)
				_, _, err := h.createManagedVtaOnlySession(ctx, userID, createSetupRequest{
					Mode:     model.ModeVtaOnly,
					VtaName:  name,
					VtaImage: request.VtaImage,
					AdminDid: request.AdminDid,
				}, infra, provider, &runID)
				cancel()
				results <- createResult{err: err}
			}
		}()
	}
	go func() {
		for i := 1; i <= request.Count; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	created := 0
	errorsSeen := make([]string, 0)
	for result := range results {
		if result.err == nil {
			created++
			continue
		}
		errorsSeen = append(errorsSeen, result.err.Error())
	}

	status := model.LoadTestActive
	if created == 0 {
		status = model.LoadTestFailed
		if err := h.db.Delete(&model.User{}, userID).Error; err != nil {
			log.Printf("[load-test] run %d: failed to remove unused test account: %v", runID, err)
		}
	} else if created < request.Count {
		status = model.LoadTestPartial
	}
	errorMsg := strings.Join(errorsSeen, "; ")
	if len(errorMsg) > 1000 {
		errorMsg = errorMsg[:1000]
	}
	if err := h.db.Model(&model.LoadTestRun{}).Where("id = ?", runID).Updates(map[string]any{
		"status": status, "created_count": created, "error_msg": errorMsg, "updated_at": time.Now(),
	}).Error; err != nil {
		log.Printf("[load-test] run %d: failed to persist creation result: %v", runID, err)
	}
}

// AdminListLoadTests — GET /api/v1/admin/load-tests.
func (h *SetupHandler) AdminListLoadTests(c *gin.Context) {
	h.reconcileStaleLoadTests()
	var runs []model.LoadTestRun
	if err := h.db.Order("id DESC").Limit(10).Find(&runs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list load tests"})
		return
	}

	runIDs := make([]uint, len(runs))
	for i := range runs {
		runIDs[i] = runs[i].ID
	}
	var sessions []model.SetupSession
	if len(runIDs) > 0 {
		if err := h.db.Where("load_test_run_id IN ?", runIDs).Order("id ASC").Find(&sessions).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list load-test sessions"})
			return
		}
	}

	byRun := make(map[uint][]model.SetupSession, len(runs))
	for _, session := range sessions {
		if session.LoadTestRunID != nil {
			byRun[*session.LoadTestRunID] = append(byRun[*session.LoadTestRunID], session)
		}
	}
	items := make([]loadTestRunItem, len(runs))
	for i := range runs {
		items[i] = loadTestRunView(&runs[i], byRun[runs[i].ID])
	}
	c.JSON(http.StatusOK, items)
}

func loadTestRunView(run *model.LoadTestRun, sessions []model.SetupSession) loadTestRunItem {
	createdCount := max(run.CreatedCount, len(sessions))
	item := loadTestRunItem{
		ID: run.ID, Status: run.Status, ErrorMsg: run.ErrorMsg,
		RequestedCount: run.RequestedCount, CreatedCount: createdCount,
		VtaImage: run.VtaImage, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
		Sessions: make([]loadTestSessionItem, len(sessions)),
	}
	for i := range sessions {
		s := &sessions[i]
		item.Sessions[i] = loadTestSessionItem{
			ID: s.ID, VtaName: s.VtaName, Status: s.Status,
			ErrorMsg: s.ErrorMsg, FQDN: s.FQDN(),
		}
		if s.Status == "running" {
			item.RunningCount++
		}
		if s.Status == "failed" {
			item.FailedCount++
		}
	}
	return item
}

// AdminCheckLoadTest — POST /api/v1/admin/load-tests/:id/check.
// A session is online only when the state machine says running and Kubernetes
// currently reports a ready VTA replica.
func (h *SetupHandler) AdminCheckLoadTest(c *gin.Context) {
	runID, ok := loadTestID(c)
	if !ok {
		return
	}
	if h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s not configured"})
		return
	}
	var run model.LoadTestRun
	if err := h.db.First(&run, runID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "load test not found"})
		return
	}
	var sessions []model.SetupSession
	if err := h.db.Where("load_test_run_id = ?", runID).Order("id ASC").Find(&sessions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list load-test sessions"})
		return
	}

	type checkItem struct {
		VtaName string `json:"vta_name"`
		Status  string `json:"status"`
		Online  bool   `json:"online"`
		Reason  string `json:"reason,omitempty"`
	}
	items := make([]checkItem, len(sessions))
	online := 0
	for i := range sessions {
		s := &sessions[i]
		item := checkItem{VtaName: s.VtaName, Status: s.Status}
		switch {
		case s.Status != "running":
			item.Reason = "session status is " + s.Status
		default:
			ns := h.k8s.UserNamespace(strconv.FormatUint(uint64(s.UserID), 10))
			ready, err := h.k8s.ComponentDeploymentReady(
				c.Request.Context(), ns, k8s.VtaDeploymentName(s.ID),
			)
			if err != nil {
				item.Reason = err.Error()
			} else if !ready {
				item.Reason = "deployment has no ready replicas"
			} else {
				item.Online = true
				online++
			}
		}
		items[i] = item
	}
	c.JSON(http.StatusOK, gin.H{
		"id": run.ID, "checked_at": time.Now().UTC(), "online_count": online,
		"total": len(items), "all_online": len(items) > 0 && online == len(items), "sessions": items,
	})
}

// AdminDeleteLoadTest — DELETE /api/v1/admin/load-tests/:id.
// Cleanup runs in the background because every member follows the complete
// teardown path and may make DNS and DID-hosting calls.
func (h *SetupHandler) AdminDeleteLoadTest(c *gin.Context) {
	runID, ok := loadTestID(c)
	if !ok {
		return
	}
	var run model.LoadTestRun
	if err := h.db.First(&run, runID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "load test not found"})
		return
	}
	switch run.Status {
	case model.LoadTestCreating:
		c.JSON(http.StatusConflict, gin.H{"error": "load-test members are still being created; retry when creation finishes"})
		return
	case model.LoadTestDeleting:
		c.JSON(http.StatusAccepted, gin.H{"id": run.ID, "status": run.Status})
		return
	case model.LoadTestDeleted:
		c.Status(http.StatusNoContent)
		return
	}
	if err := h.db.Model(&run).Updates(map[string]any{
		"status": model.LoadTestDeleting, "error_msg": "", "updated_at": time.Now(),
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start load-test cleanup"})
		return
	}
	go h.deleteLoadTest(run.ID, run.UserID)
	c.JSON(http.StatusAccepted, gin.H{"id": run.ID, "status": model.LoadTestDeleting})
}

func (h *SetupHandler) deleteLoadTest(runID uint, userID *uint) {
	var sessions []model.SetupSession
	if err := h.db.Where("load_test_run_id = ?", runID).Order("id ASC").Find(&sessions).Error; err != nil {
		h.failLoadTestDelete(runID, "failed to list sessions for cleanup")
		return
	}

	errorsSeen := make([]string, 0)
	for i := range sessions {
		s := &sessions[i]
		if h.orch != nil {
			h.orch.Cancel(s.ID)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		_, err := h.teardownVtaOnlySession(ctx, s)
		cancel()
		if err != nil {
			errorsSeen = append(errorsSeen, fmt.Sprintf("%s: %v", s.VtaName, err))
		}
	}
	if len(errorsSeen) > 0 {
		h.failLoadTestDelete(runID, strings.Join(errorsSeen, "; "))
		return
	}
	if userID != nil {
		// The sessions are gone first, so this removes only the synthetic account.
		if err := h.db.Delete(&model.User{}, *userID).Error; err != nil {
			h.failLoadTestDelete(runID, "sessions were removed but the load-test account could not be deleted")
			return
		}
	}
	if err := h.db.Model(&model.LoadTestRun{}).Where("id = ?", runID).Updates(map[string]any{
		"status": model.LoadTestDeleted, "error_msg": "", "updated_at": time.Now(),
	}).Error; err != nil {
		log.Printf("[load-test] run %d: cleanup finished but status update failed: %v", runID, err)
	}
}

func (h *SetupHandler) failLoadTestDelete(runID uint, message string) {
	if len(message) > 1000 {
		message = message[:1000]
	}
	if err := h.db.Model(&model.LoadTestRun{}).Where("id = ?", runID).Updates(map[string]any{
		"status": model.LoadTestDeleteFailed, "error_msg": message, "updated_at": time.Now(),
	}).Error; err != nil {
		log.Printf("[load-test] run %d: failed to persist delete error: %v", runID, err)
	}
}

// reconcileStaleLoadTests makes background work recoverable across an API
// restart. Ordinary session orchestrators resume independently; this only
// moves a run out of a transient state so an admin can inspect or retry it.
func (h *SetupHandler) reconcileStaleLoadTests() {
	cutoff := time.Now().Add(-loadTestStaleAfter)
	var runs []model.LoadTestRun
	if err := h.db.Where("status IN ? AND updated_at < ?",
		[]string{model.LoadTestCreating, model.LoadTestDeleting}, cutoff).Find(&runs).Error; err != nil {
		log.Printf("[load-test] stale-run query failed: %v", err)
		return
	}
	for i := range runs {
		run := &runs[i]
		if run.Status == model.LoadTestDeleting {
			h.failLoadTestDelete(run.ID, "cleanup was interrupted; retry delete")
			continue
		}
		var created int64
		if err := h.db.Model(&model.SetupSession{}).Where("load_test_run_id = ?", run.ID).Count(&created).Error; err != nil {
			continue
		}
		status := model.LoadTestPartial
		if created == 0 {
			status = model.LoadTestFailed
		} else if int(created) == run.RequestedCount {
			status = model.LoadTestActive
		}
		h.db.Model(run).Updates(map[string]any{
			"status": status, "created_count": created,
			"error_msg":  "member creation was interrupted; inspect the sessions before retrying",
			"updated_at": time.Now(),
		})
	}
}

func loadTestID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "load-test id must be a positive integer"})
		return 0, false
	}
	return uint(id), true
}
