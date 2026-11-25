// Package upgrade runs admin-triggered image upgrade batches in the
// background. The queue is the upgrade_tasks table, not memory: the runner is
// a goroutine per batch that works through the rows with bounded concurrency,
// persisting every state change, so batches survive API restarts and the
// frontend follows progress by polling the DB via GET /admin/upgrades/:id.
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/resourceprofile"
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

// Resume also recovers in-flight tasks in paused/cancelled batches. A saved
// rolling_back task must never reapply the failed target image after restart.
func (r *Runner) Resume() {
	var batches []model.UpgradeBatch
	if err := r.db.Where("status = ? OR id IN (?)", model.UpgradeBatchRunning,
		r.db.Model(&model.UpgradeTask{}).Select("batch_id").Where("status IN ?",
			[]string{model.UpgradeTaskRunning, model.UpgradeTaskRollingBack})).Find(&batches).Error; err != nil {
		log.Printf("[upgrade] resume: query failed: %v", err)
		return
	}
	for _, b := range batches {
		log.Printf("[upgrade] resuming batch %d", b.ID)
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
	var tasks []model.UpgradeTask
	if err := r.db.Model(&model.UpgradeTask{}).
		Where("batch_id = ? AND status IN ?", batchID,
			[]string{model.UpgradeTaskRunning, model.UpgradeTaskPending, model.UpgradeTaskRollingBack}).
		Order("CASE WHEN status = 'rolling_back' THEN 0 WHEN status = 'running' THEN 1 ELSE 2 END, id").Find(&tasks).Error; err != nil {
		log.Printf("[upgrade] batch %d: list tasks failed: %v", batchID, err)
		return
	}

	concurrency := max(batch.Concurrency, 1)
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, task := range tasks {
		sem <- struct{}{}
		// Re-check between dispatches so pause (task failure) and cancel take
		// effect immediately instead of after the whole snapshot.
		if task.Status == model.UpgradeTaskPending && r.batchStatus(batchID) != model.UpgradeBatchRunning {
			<-sem
			continue
		}
		wg.Add(1)
		go func(id uint) {
			defer func() { <-sem; wg.Done() }()
			r.runTask(&batch, id)
		}(task.ID)
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
			[]string{model.UpgradeTaskRunning, model.UpgradeTaskPending, model.UpgradeTaskRollingBack}).
		Count(&remaining).Error; err != nil || remaining > 0 {
		return
	}
	r.db.Model(&model.UpgradeBatch{}).
		Where("id = ? AND status = ?", batchID, model.UpgradeBatchRunning).
		Update("status", model.UpgradeBatchCompleted)
}

func (r *Runner) runTask(batch *model.UpgradeBatch, taskID uint) {
	ctx, cancel := context.WithTimeout(context.Background(), rolloutTimeout)
	defer cancel()

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

	if session.TrustRegistryState == "enabling" {
		r.failTask(batch, &task, "Trust Registry enablement is in progress")
		return
	}
	var placementActive int64
	if err := r.db.Model(&model.PlacementMaintenanceLock{}).Where("session_id = ?", session.ID).Count(&placementActive).Error; err != nil {
		r.failTask(batch, &task, "could not check workload placement: "+err.Error())
		return
	}
	if placementActive > 0 {
		r.failTask(batch, &task, "workload placement is in progress")
		return
	}
	deployName, err := deploymentName(&session, task.Component)
	if err != nil {
		r.failTask(batch, &task, err.Error())
		return
	}
	ns := r.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
	if task.Status == model.UpgradeTaskRollingBack {
		oldProfile, err := r.savedResourceProfile(ctx, session.ID, task.Component)
		if err != nil {
			r.failTask(batch, &task, "load rollback resources: "+err.Error())
			return
		}
		r.rollbackTask(batch, &task, ns, deployName, oldProfile, task.ErrorMsg)
		return
	}
	if task.Status != model.UpgradeTaskPending && task.Status != model.UpgradeTaskRunning {
		return
	}
	if task.FromImage == "" {
		r.failTask(batch, &task, "previous image is missing; refusing upgrade without a rollback target")
		return
	}
	oldProfile, desiredProfile, err := r.upgradeResourceProfiles(ctx, session.ID, task.Component, ns, deployName)
	if err != nil {
		r.failTask(batch, &task, "prepare upgrade resources: "+err.Error())
		return
	}
	if err := r.cacheRollbackProfile(ctx, session.ID, task.Component, oldProfile); err != nil {
		r.failTask(batch, &task, "save rollback resources: "+err.Error())
		return
	}
	if err := r.setTask(&task, model.UpgradeTaskRunning, ""); err != nil {
		return
	}

	if err := r.k8s.SetDeploymentImageAndResources(ctx, ns, deployName, task.ToImage, desiredProfile); err != nil {
		r.rollbackTask(batch, &task, ns, deployName, &oldProfile, "patch deployment: "+err.Error())
		return
	}
	if err := r.waitForRollout(ctx, ns, deployName, task.ToImage); err != nil {
		r.rollbackTask(batch, &task, ns, deployName, &oldProfile, err.Error())
		return
	}
	if err := recordSuccessfulUpgrade(r.db, &session, &task, desiredProfile); err != nil {
		r.rollbackTask(batch, &task, ns, deployName, &oldProfile, "record new image and resources: "+err.Error())
		return
	}
	r.setTask(&task, model.UpgradeTaskSucceeded, "")
	log.Printf("[upgrade] batch %d: session %d %s → %s ready", batch.ID, session.ID, task.Component, task.ToImage)
}

// recordSuccessfulUpgrade persists the image and resources already confirmed
// ready. Any component the platform stack runs is also the release users should
// get by default. Keep those writes atomic so a failed update restores the
// Deployment instead of leaving Kubernetes and the advertised state out of
// sync.
func recordSuccessfulUpgrade(db *gorm.DB, session *model.SetupSession, task *model.UpgradeTask, profile k8s.ResourceProfile) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.SetupSession{}).Where("id = ?", session.ID).
			Update(model.UpgradeImageColumn(task.Component), task.ToImage).Error; err != nil {
			return err
		}
		resources := model.WorkloadResource{
			SetupSessionID: session.ID,
			Component:      task.Component,
			CPURequest:     profile.CPURequest,
			CPULimit:       profile.CPULimit,
			MemoryRequest:  profile.MemoryRequest,
			MemoryLimit:    profile.MemoryLimit,
			ApplyError:     "",
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "setup_session_id"}, {Name: "component"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"cpu_request", "cpu_limit", "memory_request", "memory_limit", "apply_error", "updated_at",
			}),
		}).Create(&resources).Error; err != nil {
			return err
		}
		if session.DomainType != model.DomainPlatform {
			return nil
		}
		return model.UpsertImageDefault(tx, task.Component, task.ToImage)
	})
}

func (r *Runner) upgradeResourceProfiles(
	ctx context.Context,
	sessionID uint,
	component, namespace, deployment string,
) (k8s.ResourceProfile, k8s.ResourceProfile, error) {
	observed, err := r.k8s.DeploymentResourceProfile(ctx, namespace, deployment)
	if err != nil {
		return k8s.ResourceProfile{}, k8s.ResourceProfile{}, err
	}
	desired := observed
	saved, err := r.savedResourceProfile(ctx, sessionID, component)
	if err != nil {
		return k8s.ResourceProfile{}, k8s.ResourceProfile{}, err
	}
	if saved != nil {
		desired = *saved
	}
	profiles, err := resourceprofile.Load(ctx, r.db)
	if err != nil {
		return k8s.ResourceProfile{}, k8s.ResourceProfile{}, err
	}
	defaults, ok := profiles[component]
	if !ok {
		return k8s.ResourceProfile{}, k8s.ResourceProfile{}, fmt.Errorf("unknown component %q", component)
	}
	fillMissingProfileValues(&desired, defaults)
	return observed, desired, nil
}

func fillMissingProfileValues(profile *k8s.ResourceProfile, defaults k8s.ResourceProfile) {
	if profile.CPURequest == "" {
		profile.CPURequest = defaults.CPURequest
	}
	if profile.CPULimit == "" {
		profile.CPULimit = defaults.CPULimit
	}
	if profile.MemoryRequest == "" {
		profile.MemoryRequest = defaults.MemoryRequest
	}
	if profile.MemoryLimit == "" {
		profile.MemoryLimit = defaults.MemoryLimit
	}
}

func (r *Runner) savedResourceProfile(ctx context.Context, sessionID uint, component string) (*k8s.ResourceProfile, error) {
	var saved model.WorkloadResource
	err := r.db.WithContext(ctx).
		Where("setup_session_id = ? AND component = ?", sessionID, component).
		First(&saved).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &k8s.ResourceProfile{
		CPURequest:    saved.CPURequest,
		CPULimit:      saved.CPULimit,
		MemoryRequest: saved.MemoryRequest,
		MemoryLimit:   saved.MemoryLimit,
	}, nil
}

func (r *Runner) cacheRollbackProfile(ctx context.Context, sessionID uint, component string, profile k8s.ResourceProfile) error {
	resources := model.WorkloadResource{
		SetupSessionID: sessionID,
		Component:      component,
		CPURequest:     profile.CPURequest,
		CPULimit:       profile.CPULimit,
		MemoryRequest:  profile.MemoryRequest,
		MemoryLimit:    profile.MemoryLimit,
		ApplyError:     "",
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&resources).Error
}

func (r *Runner) waitForRollout(ctx context.Context, ns, deployName, image string) error {
	ctx, cancel := context.WithTimeout(ctx, rolloutTimeout)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	lastReason := ""
	for {
		status, err := r.k8s.DeploymentRollout(ctx, ns, deployName, image)
		if err != nil {
			lastReason = err.Error()
		} else if status.CrashLoopRestarts >= 3 {
			return fmt.Errorf("rollout failed: %s", status.Reason)
		} else if status.Ready {
			return nil
		} else if status.Reason != "" {
			lastReason = status.Reason
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for rollout: %w (%s)", ctx.Err(), lastReason)
		case <-ticker.C:
		}
	}
}

func (r *Runner) rollbackTask(
	batch *model.UpgradeBatch,
	task *model.UpgradeTask,
	ns, deployName string,
	profile *k8s.ResourceProfile,
	cause string,
) {
	// Persist the direction before touching Kubernetes so a restarted API
	// continues restoring the old image even though the batch is paused.
	if err := r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(task).Updates(map[string]any{"status": model.UpgradeTaskRollingBack, "error_msg": cause}).Error; err != nil {
			return err
		}
		return tx.Model(&model.UpgradeBatch{}).Where("id = ? AND status = ?", batch.ID, model.UpgradeBatchRunning).
			Update("status", model.UpgradeBatchPaused).Error
	}); err != nil {
		log.Printf("[upgrade] task %d: cannot persist rollback: %v", task.ID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rolloutTimeout)
	defer cancel()
	if task.FromImage == "" {
		r.setTask(task, model.UpgradeTaskRollbackFailed, cause+"; rollback failed: previous image is missing")
		return
	}
	var err error
	if profile == nil {
		err = r.k8s.RollbackDeploymentImage(ctx, ns, deployName, task.FromImage)
	} else {
		err = r.k8s.RollbackDeploymentImageAndResources(ctx, ns, deployName, task.FromImage, *profile)
	}
	if err != nil {
		r.setTask(task, model.UpgradeTaskRollbackFailed, cause+"; rollback failed: "+err.Error())
		return
	}
	if err := r.waitForRollout(ctx, ns, deployName, task.FromImage); err != nil {
		r.setTask(task, model.UpgradeTaskRollbackFailed, cause+"; rollback failed: "+err.Error())
		return
	}
	if err := r.db.Model(&model.SetupSession{}).Where("id = ?", task.SessionID).
		Update(model.UpgradeImageColumn(task.Component), task.FromImage).Error; err != nil {
		r.setTask(task, model.UpgradeTaskRollbackFailed, cause+"; previous image is ready but recording it failed: "+err.Error())
		return
	}
	r.setTask(task, model.UpgradeTaskRolledBack, cause+"; rolled back to "+task.FromImage+" and confirmed ready")
}

func (r *Runner) setTask(task *model.UpgradeTask, status, errMsg string) error {
	if err := r.db.Model(task).Updates(map[string]any{
		"status":    status,
		"error_msg": errMsg,
	}).Error; err != nil {
		log.Printf("[upgrade] task %d: update to %s failed: %v", task.ID, status, err)
		return err
	}
	return nil
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
	case "trust-registry":
		return k8s.FSTrustRegistryName(s.ID), nil
	case "vtc":
		return k8s.FSVtcName(s.ID), nil
	}
	return "", fmt.Errorf("unknown component %q", component)
}
