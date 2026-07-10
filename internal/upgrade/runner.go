// Package upgrade runs admin-triggered image upgrade batches in the
// background. The queue is the upgrade_tasks table, not memory: the runner is
// a goroutine per batch that works through the rows with bounded concurrency,
// persisting every state change, so batches survive API restarts and the
// frontend follows progress by polling the DB via GET /admin/upgrades/:id.
package upgrade

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
)

const (
	// rolloutTimeout bounds how long one task waits for its Deployment to
	// come back ready on the new image before the task fails.
	rolloutTimeout = 5 * time.Minute
	pollInterval   = 5 * time.Second
)

type Runner struct {
	db  *gorm.DB
	k8s *k8s.Client

	mu     sync.Mutex
	active map[uint]bool // batch IDs with a live scheduler goroutine
}

func NewRunner(db *gorm.DB, k8sClient *k8s.Client) *Runner {
	return &Runner{db: db, k8s: k8sClient, active: make(map[uint]bool)}
}

// Start launches the scheduler goroutine for a batch. Idempotent — a batch
// already being processed by this process is left alone.
func (r *Runner) Start(batchID uint) {
	r.mu.Lock()
	if r.active[batchID] {
		r.mu.Unlock()
		return
	}
	r.active[batchID] = true
	r.mu.Unlock()
	go r.run(batchID)
}

// Resume re-attaches scheduler goroutines for batches interrupted by a
// restart. Tasks stuck in "running" are simply re-run: the Deployment patch
// is idempotent, so re-processing an interrupted task converges to the same
// end state.
func (r *Runner) Resume() {
	var batches []model.UpgradeBatch
	if err := r.db.Where("status = ?", model.UpgradeBatchRunning).Find(&batches).Error; err != nil {
		log.Printf("[upgrade] resume: query failed: %v", err)
		return
	}
	for _, b := range batches {
		log.Printf("[upgrade] resuming batch %d (%s → %s)", b.ID, b.Component, b.Image)
		r.Start(b.ID)
	}
}

func (r *Runner) run(batchID uint) {
	defer func() {
		r.mu.Lock()
		delete(r.active, batchID)
		r.mu.Unlock()
	}()

	var batch model.UpgradeBatch
	if err := r.db.First(&batch, batchID).Error; err != nil {
		log.Printf("[upgrade] batch %d: load failed: %v", batchID, err)
		return
	}

	// Snapshot the work up front — a batch's task set is fixed at creation.
	// "running" rows are orphans from a previous process; they go first.
	var taskIDs []uint
	if err := r.db.Model(&model.UpgradeTask{}).
		Where("batch_id = ? AND status IN ?", batchID,
			[]string{model.UpgradeTaskRunning, model.UpgradeTaskPending}).
		Order("id").Pluck("id", &taskIDs).Error; err != nil {
		log.Printf("[upgrade] batch %d: list tasks failed: %v", batchID, err)
		return
	}

	concurrency := max(batch.Concurrency, 1)
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, taskID := range taskIDs {
		sem <- struct{}{}
		// Re-check between dispatches so pause (task failure) and cancel take
		// effect immediately instead of after the whole snapshot.
		if r.batchStatus(batchID) != model.UpgradeBatchRunning {
			<-sem
			break
		}
		wg.Add(1)
		go func(id uint) {
			defer func() { <-sem; wg.Done() }()
			r.runTask(&batch, id)
		}(taskID)
	}
	wg.Wait()
	r.finalize(batchID)
}

func (r *Runner) batchStatus(batchID uint) string {
	var status string
	if err := r.db.Model(&model.UpgradeBatch{}).Where("id = ?", batchID).
		Pluck("status", &status).Error; err != nil {
		log.Printf("[upgrade] batch %d: status check failed: %v", batchID, err)
		return "" // treated as not-running → stop scheduling, never crash on
	}
	return status
}

// finalize closes out a batch whose scheduler loop has drained. A batch that
// was paused (task failure) or cancelled keeps that status for the admin to
// act on; only a fully drained running batch becomes completed.
func (r *Runner) finalize(batchID uint) {
	var remaining int64
	if err := r.db.Model(&model.UpgradeTask{}).
		Where("batch_id = ? AND status IN ?", batchID,
			[]string{model.UpgradeTaskRunning, model.UpgradeTaskPending}).
		Count(&remaining).Error; err != nil || remaining > 0 {
		return
	}
	r.db.Model(&model.UpgradeBatch{}).
		Where("id = ? AND status = ?", batchID, model.UpgradeBatchRunning).
		Update("status", model.UpgradeBatchCompleted)
}

func (r *Runner) runTask(batch *model.UpgradeBatch, taskID uint) {
	ctx := context.Background()

	var task model.UpgradeTask
	if err := r.db.First(&task, taskID).Error; err != nil {
		log.Printf("[upgrade] task %d: load failed: %v", taskID, err)
		return
	}

	var session model.SetupSession
	if err := r.db.First(&session, task.SessionID).Error; err != nil {
		// Session deleted since the batch was created — nothing to upgrade,
		// and not a reason to pause the rest of the batch.
		r.setTask(&task, model.UpgradeTaskSkipped, "session no longer exists")
		return
	}

	r.setTask(&task, model.UpgradeTaskRunning, "")

	deployName, err := deploymentName(&session, batch.Component)
	if err != nil {
		r.failTask(batch, &task, err.Error())
		return
	}
	ns := r.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))

	if err := r.k8s.SetDeploymentImage(ctx, ns, deployName, batch.Image); err != nil {
		r.failTask(batch, &task, "patch deployment: "+err.Error())
		return
	}

	deadline := time.Now().Add(rolloutTimeout)
	lastReason := ""
	for time.Now().Before(deadline) {
		status, err := r.k8s.DeploymentRollout(ctx, ns, deployName, batch.Image)
		if err == nil && status.Ready {
			if err := r.db.Model(&model.SetupSession{}).Where("id = ?", session.ID).
				Update(model.UpgradeImageColumn(batch.Component), batch.Image).Error; err != nil {
				log.Printf("[upgrade] task %d: record new image failed: %v", task.ID, err)
			}
			r.setTask(&task, model.UpgradeTaskSucceeded, "")
			log.Printf("[upgrade] batch %d: session %d %s → %s ready", batch.ID, session.ID, batch.Component, batch.Image)
			return
		}
		if err == nil && status.Reason != "" {
			lastReason = status.Reason
		}
		time.Sleep(pollInterval)
	}

	msg := "timed out waiting for rollout"
	if lastReason != "" {
		msg += " (" + lastReason + ")"
	}
	r.failTask(batch, &task, msg)
}

func (r *Runner) setTask(task *model.UpgradeTask, status, errMsg string) {
	if err := r.db.Model(task).Updates(map[string]any{
		"status":    status,
		"error_msg": errMsg,
	}).Error; err != nil {
		log.Printf("[upgrade] task %d: update to %s failed: %v", task.ID, status, err)
	}
}

// failTask records the failure and pauses the batch (fail-fast): no further
// tasks are dispatched, so a broken image stops after at most `concurrency`
// sessions instead of the whole fleet. In-flight tasks finish on their own.
func (r *Runner) failTask(batch *model.UpgradeBatch, task *model.UpgradeTask, msg string) {
	log.Printf("[upgrade] batch %d: session %d failed: %s", batch.ID, task.SessionID, msg)
	r.setTask(task, model.UpgradeTaskFailed, msg)
	r.db.Model(&model.UpgradeBatch{}).
		Where("id = ? AND status = ?", batch.ID, model.UpgradeBatchRunning).
		Update("status", model.UpgradeBatchPaused)
}

// deploymentName maps a session + component to the Deployment the setup flow
// created for it. vta_only's VTA runs as vta-<id>; the full_stack family's
// components run as fs-<id>-<component>.
func deploymentName(s *model.SetupSession, component string) (string, error) {
	switch component {
	case "vta":
		if s.Mode == model.ModeVtaOnly {
			return k8s.VtaDeploymentName(s.ID), nil
		}
		return k8s.FSVtaName(s.ID), nil
	case "mediator":
		return k8s.FSMediatorName(s.ID), nil
	case "dids":
		return k8s.FSDidsName(s.ID), nil
	case "vtc":
		return k8s.FSVtcName(s.ID), nil
	}
	return "", fmt.Errorf("unknown component %q", component)
}
