package placement

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
)

const placementStepTimeout = 5 * time.Minute

type Runner struct {
	db      *gorm.DB
	k8s     *k8s.Client
	service *Service
	cfg     config.PlacementConfig
}

func NewRunner(db *gorm.DB, client *k8s.Client, cfg config.PlacementConfig) (*Runner, error) {
	service, err := NewService(db, client, cfg)
	if err != nil {
		return nil, err
	}
	return &Runner{db: db, k8s: client, service: service, cfg: cfg}, nil
}

func (r *Runner) Run(ctx context.Context) {
	interval := time.Duration(max(r.cfg.PollSeconds, 5)) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	r.cycle(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.cycle(ctx)
		}
	}
}

func (r *Runner) cycle(ctx context.Context) {
	placements, err := r.k8s.ListVTAPlacements(ctx)
	if err != nil {
		log.Printf("[placement] inventory VTA workloads: %v", err)
		return
	}
	if err := r.refreshNodes(ctx, placements); err != nil {
		log.Printf("[placement] refresh nodes: %v", err)
	}
	if r.cfg.Mode == config.PlacementDisabled {
		return
	}
	if err := r.ensureProvisionPlans(ctx, placements); err != nil {
		log.Printf("[placement] discover gated VTA pods: %v", err)
	}
	var plans []model.PlacementPlan
	if err := r.db.Where("status = ?", model.PlacementPlanRunning).Order("id").Find(&plans).Error; err != nil {
		log.Printf("[placement] list plans: %v", err)
		return
	}
	for _, plan := range plans {
		if err := r.runNextTask(ctx, &plan, placements); err != nil {
			log.Printf("[placement] plan %d: %v", plan.ID, err)
		}
	}
}

func (r *Runner) ensureProvisionPlans(ctx context.Context, placements []k8s.VTAPlacement) error {
	for _, placement := range placements {
		if !placement.Gated || placement.PodUID == "" {
			continue
		}
		if r.cfg.Mode == config.PlacementCanary && !containsSession(r.cfg.CanarySessionIDs, placement.SessionID) {
			continue
		}
		err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var active int64
			if err := tx.Model(&model.PlacementTask{}).Where(
				"session_id = ? AND (state NOT IN ? OR (state = ? AND pod_uid = ?))", placement.SessionID,
				[]string{model.PlacementCompleted, model.PlacementFailed, model.PlacementCancelled, model.PlacementRolledBack},
				model.PlacementFailed, placement.PodUID,
			).Count(&active).Error; err != nil {
				return err
			}
			if active > 0 {
				return nil
			}
			var session model.SetupSession
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, placement.SessionID).Error; err != nil {
				return err
			}
			if session.Mode != model.ModeVtaOnly || session.Status == "failed" || session.Status == "deleting" {
				return nil
			}
			plan := model.PlacementPlan{Kind: model.PlacementPlanProvision, Status: model.PlacementPlanRunning}
			if err := tx.Create(&plan).Error; err != nil {
				return err
			}
			if err := recordPlanEvent(tx, plan.ID, "", model.PlacementPlanRunning, model.PlacementActorWorker, nil, "automatic provisioning plan created"); err != nil {
				return err
			}
			task := model.PlacementTask{
				PlanID: plan.ID, SessionID: placement.SessionID, Component: "vta",
				State: model.PlacementRequested, P95MemoryBytes: r.service.p95,
				Namespace: placement.Namespace, DeploymentName: placement.DeploymentName,
				DeploymentUID: placement.DeploymentUID, PodUID: placement.PodUID,
			}
			if err := tx.Create(&task).Error; err != nil {
				return err
			}
			return recordTaskEvent(tx, &task, "", model.PlacementRequested, model.PlacementActorWorker, nil, "gated Pod discovered", map[string]any{"pod_uid": task.PodUID})
		})
		if err != nil {
			return fmt.Errorf("session %d: %w", placement.SessionID, err)
		}
	}
	return nil
}

func (r *Runner) refreshNodes(ctx context.Context, placements []k8s.VTAPlacement) error {
	reservations, err := r.service.reservations()
	if err != nil {
		return err
	}
	nodes, err := r.k8s.PlacementNodesFromPlacements(ctx, reservations, placements)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, node := range nodes {
		state := r.nodeState(node, now)
		if err := r.k8s.UpdateNodePlacementLabels(ctx, node.Name, state.MemoryState, state.Eligible); err != nil {
			return err
		}
		state.ObservedAt = &now
		state.UpdatedAt = now
		if err := r.db.Save(&state).Error; err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) nodeState(node k8s.NodePlacement, now time.Time) model.PlacementNodeState {
	state := model.PlacementNodeState{NodeName: node.Name, MemoryState: "unknown"}
	_ = r.db.First(&state, "node_name = ?", node.Name).Error
	if !node.MetricsAvailable || !node.Schedulable || node.MemAllocatable <= 0 {
		state.MemoryState, state.Eligible = "unknown", false
		return state
	}
	used := max(node.MemUsed, node.MemRequested) + node.MemReserved
	pct := int(used * 100 / node.MemAllocatable)
	previous := state.MemoryState
	switch {
	case pct >= r.cfg.HotAtPercent:
		state.RecoverySince = nil
		if state.HotSince == nil {
			state.HotSince = &now
		}
		if now.Sub(*state.HotSince) >= time.Duration(r.cfg.HotObservationMin)*time.Minute {
			state.MemoryState = "hot"
		} else {
			state.MemoryState = "orange"
		}
	case previous == "hot" && pct <= r.cfg.YellowBelowPercent:
		state.HotSince = nil
		if state.RecoverySince == nil {
			state.RecoverySince = &now
		}
		if now.Sub(*state.RecoverySince) >= time.Duration(r.cfg.RecoveryObservationMin)*time.Minute {
			state.MemoryState = memoryBand(pct, r.cfg)
			state.RecoverySince = nil
		} else {
			state.MemoryState = "hot"
		}
	default:
		state.HotSince, state.RecoverySince = nil, nil
		state.MemoryState = memoryBand(pct, r.cfg)
	}
	projected := used + r.service.p95
	state.Eligible = state.MemoryState != "hot" && projected*100/node.MemAllocatable < int64(r.cfg.HotAtPercent)
	return state
}

func memoryBand(percent int, cfg config.PlacementConfig) string {
	switch {
	case percent < cfg.GreenBelowPercent:
		return "green"
	case percent < cfg.YellowBelowPercent:
		return "yellow"
	case percent < cfg.HotAtPercent:
		return "orange"
	default:
		return "hot"
	}
}

func (r *Runner) runNextTask(parent context.Context, plan *model.PlacementPlan, placements []k8s.VTAPlacement) error {
	var task model.PlacementTask
	err := r.db.Where("plan_id = ? AND state NOT IN ?", plan.ID,
		[]string{model.PlacementCompleted, model.PlacementFailed, model.PlacementCancelled, model.PlacementRolledBack}).Order("id").First(&task).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return r.completePlanIfDone(plan.ID)
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, placementStepTimeout)
	defer cancel()
	if err := r.advance(ctx, plan, &task, placements); err != nil {
		r.fail(plan, &task, err)
		return err
	}
	if task.State == model.PlacementCompleted {
		return r.completePlanIfDone(plan.ID)
	}
	return nil
}

func (r *Runner) completePlanIfDone(planID uint) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var plan model.PlacementPlan
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&plan, planID).Error; err != nil {
			return err
		}
		if plan.Status != model.PlacementPlanRunning {
			return nil
		}
		var remaining int64
		if err := tx.Model(&model.PlacementTask{}).Where("plan_id = ? AND state NOT IN ?", planID,
			[]string{model.PlacementCompleted, model.PlacementFailed, model.PlacementCancelled, model.PlacementRolledBack}).Count(&remaining).Error; err != nil {
			return err
		}
		if remaining > 0 {
			return nil
		}
		return transitionPlan(tx, &plan, model.PlacementPlanCompleted, nil, model.PlacementActorWorker, nil, "all tasks reached a terminal state")
	})
}

func (r *Runner) advance(ctx context.Context, plan *model.PlacementPlan, task *model.PlacementTask, placements []k8s.VTAPlacement) error {
	var session model.SetupSession
	if err := r.db.First(&session, task.SessionID).Error; err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	if session.Mode != model.ModeVtaOnly {
		return fmt.Errorf("session is not a VTA-only workload")
	}
	if r.cfg.Mode == config.PlacementCanary && !containsSession(r.cfg.CanarySessionIDs, session.ID) {
		return fmt.Errorf("session is not in the placement canary allowlist")
	}
	namespace := r.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
	if plan.Kind == model.PlacementPlanProvision {
		return r.advanceProvision(ctx, task, namespace, placements)
	}
	if session.Status != "running" {
		return fmt.Errorf("session is not a running VTA-only workload")
	}

	switch task.State {
	case model.PlacementRequested, model.PlacementValidating:
		return r.validateAndReserve(ctx, task, namespace, placements)
	case model.PlacementReserved, model.PlacementScalingDown:
		if err := r.setState(task, model.PlacementScalingDown, nil); err != nil {
			return err
		}
		if err := r.k8s.ScaleVTADeployment(ctx, namespace, session.ID, 0); err != nil {
			return err
		}
		return r.setState(task, model.PlacementWaitingPodTermination, nil)
	case model.PlacementWaitingPodTermination:
		selector := fmt.Sprintf("app=vta,session-id=%d", session.ID)
		if err := r.k8s.WaitForComponentPodsGone(ctx, namespace, selector, placementStepTimeout); err != nil {
			return err
		}
		return r.setState(task, model.PlacementWaitingVolumeDetach, nil)
	case model.PlacementWaitingVolumeDetach:
		identity, err := r.k8s.WaitVTAStorageDetached(ctx, namespace, session.ID, placementStepTimeout)
		if err != nil {
			return err
		}
		if !matchesTaskStorage(task, identity) {
			return errors.New("storage identity changed while detaching; refusing to continue")
		}
		if err := r.k8s.ReconcileVTADeployment(ctx, namespace, session.ID); err != nil {
			return err
		}
		if err := r.setState(task, model.PlacementCreatingGatedPod, nil); err != nil {
			return err
		}
		return r.k8s.ScaleVTADeployment(ctx, namespace, session.ID, max(task.OriginalReplicas, 1))
	case model.PlacementCreatingGatedPod:
		pod, err := r.k8s.WaitForGatedVTAPod(ctx, namespace, session.ID, placementStepTimeout)
		if err != nil {
			return err
		}
		return r.db.Transaction(func(tx *gorm.DB) error {
			if err := transitionTask(tx, task, model.PlacementAssigningTargetNode, map[string]any{"pod_uid": string(pod.UID)}, model.PlacementActorWorker, nil, "gated Pod created"); err != nil {
				return err
			}
			return tx.Model(&model.PlacementReservation{}).Where("task_id = ? AND status = ?", task.ID, "active").Update("pod_uid", string(pod.UID)).Error
		})
	case model.PlacementAssigningTargetNode:
		var podName string
		for _, item := range placements {
			if item.SessionID == session.ID {
				podName = item.PodName
				break
			}
		}
		if podName == "" {
			return errors.New("gated VTA pod disappeared")
		}
		if _, err := r.k8s.AssignGatedPod(ctx, namespace, podName, task.TargetNode); err != nil {
			return err
		}
		return r.setState(task, model.PlacementWaitingVolumeAttach, nil)
	case model.PlacementWaitingVolumeAttach, model.PlacementWaitingReady:
		if err := r.setState(task, model.PlacementWaitingReady, nil); err != nil {
			return err
		}
		pod, err := r.k8s.WaitVTAReadyOnNode(ctx, namespace, session.ID, task.TargetNode, placementStepTimeout)
		if err != nil {
			return err
		}
		identity, err := r.k8s.InspectVTAStorage(ctx, namespace, session.ID)
		if err != nil {
			return err
		}
		if !matchesTaskStorage(task, identity) {
			return errors.New("storage identity changed after relocation")
		}
		if identity.LonghornState != "attached" || identity.LonghornRobustness == "faulted" {
			return fmt.Errorf("Longhorn volume is %s/%s", identity.LonghornState, identity.LonghornRobustness)
		}
		return r.complete(task, string(pod.UID))
	default:
		return fmt.Errorf("unsupported placement state %s", task.State)
	}
}

func (r *Runner) advanceProvision(ctx context.Context, task *model.PlacementTask, namespace string, placements []k8s.VTAPlacement) error {
	switch task.State {
	case model.PlacementRequested, model.PlacementValidating:
		for _, placement := range placements {
			if placement.SessionID == task.SessionID && placement.PodUID == task.PodUID && !placement.Gated && task.TargetNode != "" {
				return r.setState(task, model.PlacementWaitingReady, nil)
			}
		}
		return r.validateProvisionAndReserve(ctx, task, namespace, placements)
	case model.PlacementReserved, model.PlacementAssigningTargetNode:
		placement, err := r.gatedPlacement(placements, task.SessionID, task.PodUID)
		if err != nil {
			return err
		}
		if _, err := r.k8s.AssignGatedPod(ctx, placement.Namespace, placement.PodName, task.TargetNode); err != nil {
			return err
		}
		return r.setState(task, model.PlacementWaitingReady, nil)
	case model.PlacementWaitingVolumeAttach, model.PlacementWaitingReady:
		pod, err := r.k8s.WaitVTAReadyOnNode(ctx, namespace, task.SessionID, task.TargetNode, placementStepTimeout)
		if err != nil {
			return err
		}
		identity, err := r.k8s.InspectVTAStorage(ctx, namespace, task.SessionID)
		if err != nil {
			return err
		}
		if !matchesTaskStorage(task, identity) {
			return errors.New("storage identity changed during initial placement")
		}
		if identity.LonghornState != "attached" || identity.LonghornRobustness == "faulted" {
			return fmt.Errorf("Longhorn volume is %s/%s", identity.LonghornState, identity.LonghornRobustness)
		}
		return r.complete(task, string(pod.UID))
	default:
		return fmt.Errorf("unsupported initial placement state %s", task.State)
	}
}

func (r *Runner) validateProvisionAndReserve(ctx context.Context, task *model.PlacementTask, namespace string, placements []k8s.VTAPlacement) error {
	if err := r.setState(task, model.PlacementValidating, nil); err != nil {
		return err
	}
	placement, err := r.gatedPlacement(placements, task.SessionID, task.PodUID)
	if err != nil {
		return err
	}
	identity, err := r.k8s.InspectVTAStorage(ctx, namespace, task.SessionID)
	if err != nil {
		return err
	}
	if identity.LonghornRobustness == "faulted" {
		return errors.New("Longhorn volume is faulted")
	}
	target, err := r.chooseTarget(ctx, task, placements)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := r.acquireLockAndReservation(tx, task, target, now); err != nil {
			return err
		}
		return transitionTask(tx, task, model.PlacementReserved, map[string]any{
			"target_node": target, "namespace": namespace,
			"deployment_name": placement.DeploymentName, "deployment_uid": placement.DeploymentUID,
			"pod_uid": placement.PodUID, "pvc_name": identity.PVCName, "pvc_uid": identity.PVCUID,
			"pv_name": identity.PVName, "pv_uid": identity.PVUID, "csi_volume_handle": identity.CSIVolumeHandle,
			"longhorn_volume": identity.LonghornVolume, "last_backup_name": identity.LastBackupName,
			"last_backup_at": identity.LastBackupAt, "started_at": now,
		}, model.PlacementActorWorker, nil, "destination capacity reserved")
	})
}

func (r *Runner) gatedPlacement(placements []k8s.VTAPlacement, sessionID uint, podUID string) (*k8s.VTAPlacement, error) {
	for i := range placements {
		placement := &placements[i]
		if placement.SessionID == sessionID && placement.Gated && (podUID == "" || placement.PodUID == podUID) {
			return placement, nil
		}
	}
	return nil, errors.New("gated VTA pod disappeared or was replaced")
}

func (r *Runner) chooseTarget(ctx context.Context, task *model.PlacementTask, placements []k8s.VTAPlacement) (string, error) {
	reservations, err := r.service.reservations()
	if err != nil {
		return "", err
	}
	nodes, err := r.k8s.PlacementNodesFromPlacements(ctx, reservations, placements)
	if err != nil {
		return "", err
	}
	target := task.TargetNode
	if target == "" {
		target = safestNode(nodes, map[string]int64{}, task.P95MemoryBytes, r.cfg.HotAtPercent)
	}
	if target == "" {
		return "", errors.New("no eligible target node")
	}
	for _, node := range nodes {
		if node.Name != target || !node.Eligible || !node.Schedulable || !node.MetricsAvailable || node.MemAllocatable <= 0 {
			continue
		}
		projected := max(node.MemUsed, node.MemRequested) + node.MemReserved + task.P95MemoryBytes
		if projected*100/node.MemAllocatable < int64(r.cfg.HotAtPercent) {
			return target, nil
		}
	}
	return "", fmt.Errorf("target node %s is not safely eligible", target)
}

func (r *Runner) acquireLockAndReservation(tx *gorm.DB, task *model.PlacementTask, target string, now time.Time) error {
	var session model.SetupSession
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "status").First(&session, task.SessionID).Error; err != nil {
		return err
	}
	if session.Status == "deleting" || session.Status == "failed" {
		return fmt.Errorf("session is %s", session.Status)
	}
	var plan model.PlacementPlan
	if err := tx.Select("kind").First(&plan, task.PlanID).Error; err != nil {
		return err
	}
	var conflicting int64
	// A provisioning plan releases the gated Pod created by an upgrade or its
	// rollback. Blocking that plan on the upgrade would leave both workers
	// waiting for the other to finish.
	if plan.Kind != model.PlacementPlanProvision {
		if err := tx.Model(&model.UpgradeTask{}).Where("session_id = ? AND status IN ?", task.SessionID,
			[]string{model.UpgradeTaskPending, model.UpgradeTaskRunning, model.UpgradeTaskRollingBack}).Count(&conflicting).Error; err != nil {
			return err
		}
		if conflicting > 0 {
			return errors.New("session has an active upgrade")
		}
	}
	if err := tx.Model(&model.VtaAclSnapshot{}).Where("session_id = ? AND maintenance_started_at IS NOT NULL", task.SessionID).Count(&conflicting).Error; err != nil {
		return err
	}
	if conflicting > 0 {
		return errors.New("session has an active stack maintenance operation")
	}
	lock := model.PlacementMaintenanceLock{SessionID: task.SessionID, TaskID: task.ID, Operation: "placement", AcquiredAt: now}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&lock).Error; err != nil {
		return err
	}
	var count int64
	if err := tx.Model(&model.PlacementMaintenanceLock{}).Where("session_id = ? AND task_id = ?", task.SessionID, task.ID).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return errors.New("session already has an active maintenance operation")
	}
	reservation := model.PlacementReservation{TaskID: task.ID, NodeName: target, MemoryBytes: task.P95MemoryBytes, Status: "active"}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&reservation).Error
}

func (r *Runner) validateAndReserve(ctx context.Context, task *model.PlacementTask, namespace string, placements []k8s.VTAPlacement) error {
	if err := r.setState(task, model.PlacementValidating, nil); err != nil {
		return err
	}
	snapshot, replicas, deploymentUID, err := r.k8s.SnapshotVTADeployment(ctx, namespace, task.SessionID)
	if err != nil {
		return err
	}
	identity, err := r.k8s.InspectVTAStorage(ctx, namespace, task.SessionID)
	if err != nil {
		return err
	}
	if identity.LonghornRobustness != "healthy" {
		return fmt.Errorf("Longhorn volume is not healthy: %s", identity.LonghornRobustness)
	}
	if identity.LastBackupAt == "" {
		return errors.New("no completed Longhorn backup is recorded for this volume")
	}
	reservations, err := r.service.reservations()
	if err != nil {
		return err
	}
	nodes, err := r.k8s.PlacementNodesFromPlacements(ctx, reservations, placements)
	if err != nil {
		return err
	}
	target := task.TargetNode
	if target == "" {
		target = safestNode(nodes, map[string]int64{}, task.P95MemoryBytes, r.cfg.HotAtPercent)
	}
	var source string
	for _, placement := range placements {
		if placement.SessionID == task.SessionID {
			source = placement.NodeName
			break
		}
	}
	if target == "" {
		return errors.New("no eligible target node")
	}
	allowed := false
	for _, node := range nodes {
		if node.Name == target && node.Eligible && node.Schedulable && node.MetricsAvailable && node.MemAllocatable > 0 {
			projected := max(node.MemUsed, node.MemRequested) + node.MemReserved + task.P95MemoryBytes
			allowed = projected*100/node.MemAllocatable < int64(r.cfg.HotAtPercent)
		}
	}
	if !allowed {
		return fmt.Errorf("target node %s is not safely eligible", target)
	}
	now := time.Now().UTC()
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := r.acquireLockAndReservation(tx, task, target, now); err != nil {
			return err
		}
		return transitionTask(tx, task, model.PlacementReserved, map[string]any{
			"source_node": source, "target_node": target,
			"namespace": namespace, "deployment_name": k8s.VtaDeploymentName(task.SessionID),
			"deployment_uid": deploymentUID, "deployment_snapshot": snapshot, "original_replicas": replicas,
			"pvc_name": identity.PVCName, "pvc_uid": identity.PVCUID, "pv_name": identity.PVName,
			"pv_uid": identity.PVUID, "csi_volume_handle": identity.CSIVolumeHandle,
			"longhorn_volume": identity.LonghornVolume, "last_backup_name": identity.LastBackupName,
			"last_backup_at": identity.LastBackupAt, "started_at": now,
		}, model.PlacementActorWorker, nil, "source validated and destination capacity reserved")
	})
}

func matchesTaskStorage(task *model.PlacementTask, identity k8s.StorageIdentity) bool {
	return task.PVCUID == identity.PVCUID && task.PVUID == identity.PVUID &&
		task.CSIVolumeHandle == identity.CSIVolumeHandle && task.LonghornVolume == identity.LonghornVolume
}

func (r *Runner) setState(task *model.PlacementTask, state string, updates map[string]any) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		return transitionTask(tx, task, state, updates, model.PlacementActorWorker, nil, "")
	})
}

func (r *Runner) complete(task *model.PlacementTask, podUID string) error {
	now := time.Now().UTC()
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := transitionTask(tx, task, model.PlacementCompleted, map[string]any{"pod_uid": podUID, "completed_at": now, "error_msg": ""}, model.PlacementActorWorker, nil, "Pod is ready on the target node"); err != nil {
			return err
		}
		if err := tx.Model(&model.PlacementReservation{}).Where("task_id = ? AND status = ?", task.ID, "active").Updates(map[string]any{"status": "released", "released_at": now}).Error; err != nil {
			return err
		}
		return tx.Delete(&model.PlacementMaintenanceLock{}, "task_id = ?", task.ID).Error
	})
}

func (r *Runner) fail(plan *model.PlacementPlan, task *model.PlacementTask, cause error) {
	log.Printf("[placement] plan %d task %d failed: %v", plan.ID, task.ID, cause)
	if task.Namespace != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if plan.Kind != model.PlacementPlanProvision && len(task.DeploymentSnapshot) > 2 {
			if err := r.k8s.RestoreVTADeploymentSnapshot(ctx, task.Namespace, task.SessionID, task.DeploymentUID, task.DeploymentSnapshot); err != nil {
				cause = fmt.Errorf("%w; automatic Deployment rollback failed: %v", cause, err)
			}
		} else {
			_ = r.k8s.ScaleVTADeployment(ctx, task.Namespace, task.SessionID, max(task.OriginalReplicas, 1))
		}
		cancel()
	}
	now := time.Now().UTC()
	_ = r.db.Transaction(func(tx *gorm.DB) error {
		if err := transitionTask(tx, task, model.PlacementFailed, map[string]any{"error_msg": cause.Error()}, model.PlacementActorWorker, nil, cause.Error()); err != nil {
			return err
		}
		if err := transitionPlan(tx, plan, model.PlacementPlanPaused, map[string]any{"error_msg": cause.Error()}, model.PlacementActorWorker, nil, cause.Error()); err != nil {
			return err
		}
		if err := tx.Model(&model.PlacementReservation{}).Where("task_id = ? AND status = ?", task.ID, "active").Updates(map[string]any{"status": "released", "released_at": now}).Error; err != nil {
			return err
		}
		return tx.Delete(&model.PlacementMaintenanceLock{}, "task_id = ?", task.ID).Error
	})
}

func containsSession(ids []uint, id uint) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
