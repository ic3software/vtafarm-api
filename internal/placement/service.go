package placement

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
)

type Service struct {
	db  *gorm.DB
	k8s *k8s.Client
	cfg config.PlacementConfig
	p95 int64
}

type WorkloadView struct {
	VTAName string `json:"vta_name"`
	k8s.VTAPlacement
}

type Overview struct {
	Mode      string              `json:"mode"`
	Nodes     []k8s.NodePlacement `json:"nodes"`
	Workloads []WorkloadView      `json:"workloads"`
}

type PlanItem struct {
	SessionID  uint   `json:"session_id"`
	TargetNode string `json:"target_node"`
}

type PreviewItem struct {
	SessionID              uint   `json:"session_id"`
	VTAName                string `json:"vta_name"`
	SourceNode             string `json:"source_node"`
	TargetNode             string `json:"target_node"`
	P95MemoryBytes         int64  `json:"p95_memory_bytes"`
	ProjectedMemoryPercent int    `json:"projected_memory_percent"`
	Allowed                bool   `json:"allowed"`
	Reason                 string `json:"reason,omitempty"`
}

func NewService(db *gorm.DB, client *k8s.Client, cfg config.PlacementConfig) (*Service, error) {
	if cfg.GreenBelowPercent <= 0 || cfg.GreenBelowPercent >= cfg.YellowBelowPercent ||
		cfg.YellowBelowPercent >= cfg.HotAtPercent || cfg.HotAtPercent > 100 {
		return nil, errors.New("placement memory thresholds must satisfy 0 < green < yellow < hot <= 100")
	}
	if cfg.HotObservationMin < 0 || cfg.RecoveryObservationMin < 0 || cfg.MetricsStaleSeconds <= 0 || cfg.PollSeconds <= 0 {
		return nil, errors.New("placement observation, freshness, and polling values are invalid")
	}
	quantity, err := resource.ParseQuantity(cfg.VTAP95Memory)
	if err != nil || quantity.Value() <= 0 {
		return nil, fmt.Errorf("invalid PLACEMENT_VTA_P95_MEMORY %q", cfg.VTAP95Memory)
	}
	return &Service{db: db, k8s: client, cfg: cfg, p95: quantity.Value()}, nil
}

func (s *Service) Overview(ctx context.Context) (*Overview, error) {
	reservations, err := s.reservations()
	if err != nil {
		return nil, err
	}
	nodes, err := s.k8s.PlacementNodes(ctx, reservations)
	if err != nil {
		return nil, err
	}
	placements, err := s.k8s.ListVTAPlacements(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]uint, 0, len(placements))
	for _, placement := range placements {
		ids = append(ids, placement.SessionID)
	}
	var sessions []model.SetupSession
	if len(ids) > 0 {
		if err := s.db.Where("id IN ?", ids).Find(&sessions).Error; err != nil {
			return nil, err
		}
	}
	names := make(map[uint]string, len(sessions))
	for _, session := range sessions {
		names[session.ID] = session.VtaName
	}
	workloads := make([]WorkloadView, 0, len(placements))
	for _, placement := range placements {
		workloads = append(workloads, WorkloadView{VTAName: names[placement.SessionID], VTAPlacement: placement})
	}
	sort.Slice(workloads, func(i, j int) bool { return workloads[i].SessionID < workloads[j].SessionID })
	return &Overview{Mode: s.cfg.Mode, Nodes: nodes, Workloads: workloads}, nil
}

func (s *Service) reservations() (map[string]int64, error) {
	type row struct {
		NodeName string
		Total    int64
	}
	var rows []row
	err := s.db.Model(&model.PlacementReservation{}).Select("node_name, SUM(memory_bytes) AS total").
		Where("status = ?", "active").Group("node_name").Scan(&rows).Error
	result := map[string]int64{}
	for _, item := range rows {
		result[item.NodeName] = item.Total
	}
	return result, err
}

func (s *Service) Preview(ctx context.Context, items []PlanItem, all bool) ([]PreviewItem, error) {
	if s.k8s == nil {
		return nil, errors.New("kubernetes is unavailable")
	}
	overview, err := s.Overview(ctx)
	if err != nil {
		return nil, err
	}
	placementBySession := map[uint]WorkloadView{}
	for _, workload := range overview.Workloads {
		placementBySession[workload.SessionID] = workload
	}
	nodes := map[string]k8s.NodePlacement{}
	for _, node := range overview.Nodes {
		nodes[node.Name] = node
	}
	if all {
		var sessions []model.SetupSession
		if err := s.db.Where("mode = ? AND status = ?", model.ModeVtaOnly, "running").Order("id").Find(&sessions).Error; err != nil {
			return nil, err
		}
		items = items[:0]
		for _, session := range sessions {
			items = append(items, PlanItem{SessionID: session.ID})
		}
	}
	projected := map[string]int64{}
	result := make([]PreviewItem, 0, len(items))
	for _, item := range items {
		workload, exists := placementBySession[item.SessionID]
		preview := PreviewItem{SessionID: item.SessionID, VTAName: workload.VTAName, SourceNode: workload.NodeName, TargetNode: item.TargetNode, P95MemoryBytes: s.p95}
		if !exists {
			preview.Reason = "running VTA-only workload not found"
			result = append(result, preview)
			continue
		}
		if preview.TargetNode == "" {
			preview.TargetNode = safestNode(overview.Nodes, projected, s.p95, s.cfg.HotAtPercent)
		}
		node, ok := nodes[preview.TargetNode]
		if !ok {
			preview.Reason = "target node not found"
		} else if !node.Schedulable || !node.Eligible || !node.MetricsAvailable {
			preview.Reason = "target node is not eligible"
		} else if node.MemAllocatable <= 0 {
			preview.Reason = "target node has no allocatable memory"
		} else {
			used := max(node.MemUsed, node.MemRequested) + node.MemReserved + projected[node.Name] + s.p95
			preview.ProjectedMemoryPercent = int(used * 100 / node.MemAllocatable)
			if preview.ProjectedMemoryPercent >= s.cfg.HotAtPercent {
				preview.Reason = "target would reach the hot threshold"
			} else {
				preview.Allowed = true
				projected[node.Name] += s.p95
			}
		}
		result = append(result, preview)
	}
	return result, nil
}

func safestNode(nodes []k8s.NodePlacement, projected map[string]int64, p95 int64, hotAt int) string {
	best, bestUsed := "", int64(^uint64(0)>>1)
	for _, node := range nodes {
		if !node.Schedulable || !node.Eligible || !node.MetricsAvailable || node.MemAllocatable <= 0 {
			continue
		}
		used := max(node.MemUsed, node.MemRequested) + node.MemReserved + projected[node.Name] + p95
		if used*100/node.MemAllocatable >= int64(hotAt) {
			continue
		}
		if used < bestUsed {
			best, bestUsed = node.Name, used
		}
	}
	return best
}

func (s *Service) CreatePlan(ctx context.Context, adminID uint, kind string, items []PlanItem, all bool) (*model.PlacementPlan, []PreviewItem, error) {
	if kind != model.PlacementPlanReconcile && kind != model.PlacementPlanRelocate {
		return nil, nil, errors.New("unsupported placement plan kind")
	}
	if s.cfg.Mode == config.PlacementDisabled {
		return nil, nil, errors.New("placement activation mode is disabled")
	}
	if s.cfg.Mode == config.PlacementCanary {
		if all || kind == model.PlacementPlanReconcile {
			return nil, nil, errors.New("cluster-wide reconciliation requires enabled mode")
		}
		for _, item := range items {
			if !containsSession(s.cfg.CanarySessionIDs, item.SessionID) {
				return nil, nil, fmt.Errorf("session %d is not in the placement canary allowlist", item.SessionID)
			}
		}
	}
	preview, err := s.Preview(ctx, items, all)
	if err != nil {
		return nil, nil, err
	}
	for _, item := range preview {
		if !item.Allowed {
			return nil, preview, fmt.Errorf("session %d: %s", item.SessionID, item.Reason)
		}
	}
	if len(preview) == 0 {
		return nil, preview, errors.New("placement plan has no workloads")
	}
	plan := &model.PlacementPlan{AdminID: &adminID, Kind: kind, Status: model.PlacementPlanRunning}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		ids := make([]uint, len(preview))
		for i := range preview {
			ids[i] = preview[i].SessionID
		}
		var sessions []model.SetupSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", ids).Find(&sessions).Error; err != nil {
			return err
		}
		var active int64
		if err := tx.Model(&model.PlacementTask{}).Where("session_id IN ? AND state NOT IN ?", ids,
			[]string{model.PlacementCompleted, model.PlacementFailed, model.PlacementCancelled, model.PlacementRolledBack}).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return errors.New("one or more sessions already have active placement work")
		}
		if err := tx.Create(plan).Error; err != nil {
			return err
		}
		tasks := make([]model.PlacementTask, len(preview))
		for i, item := range preview {
			tasks[i] = model.PlacementTask{PlanID: plan.ID, SessionID: item.SessionID, Component: "vta", TargetNode: item.TargetNode, State: model.PlacementRequested, P95MemoryBytes: s.p95}
		}
		if err := tx.Create(&tasks).Error; err != nil {
			return err
		}
		events := make([]model.PlacementEvent, len(tasks))
		for i := range tasks {
			taskID := tasks[i].ID
			events[i] = model.PlacementEvent{PlanID: plan.ID, TaskID: &taskID, State: model.PlacementRequested}
		}
		return tx.Create(&events).Error
	})
	if err != nil {
		return nil, preview, err
	}
	return plan, preview, nil
}

func (s *Service) GetPlan(id uint) (*model.PlacementPlan, error) {
	var plan model.PlacementPlan
	if err := s.db.Preload("Tasks", func(db *gorm.DB) *gorm.DB { return db.Order("id") }).
		Preload("Events", func(db *gorm.DB) *gorm.DB { return db.Order("id") }).First(&plan, id).Error; err != nil {
		return nil, err
	}
	return &plan, nil
}

func (s *Service) ListPlans() ([]model.PlacementPlan, error) {
	var plans []model.PlacementPlan
	err := s.db.Preload("Tasks", func(db *gorm.DB) *gorm.DB { return db.Order("id") }).Order("id DESC").Limit(50).Find(&plans).Error
	return plans, err
}

func (s *Service) CancelPlan(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var plan model.PlacementPlan
		if err := tx.First(&plan, id).Error; err != nil {
			return err
		}
		if plan.Kind == model.PlacementPlanProvision {
			return errors.New("automatic provisioning placement cannot be cancelled")
		}
		var active int64
		if err := tx.Model(&model.PlacementTask{}).Where("plan_id = ? AND state NOT IN ?", id,
			[]string{model.PlacementRequested, model.PlacementCompleted, model.PlacementFailed, model.PlacementCancelled, model.PlacementRolledBack}).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return errors.New("cannot cancel while a placement step is active")
		}
		result := tx.Model(&model.PlacementPlan{}).Where("id = ? AND status = ?", id, model.PlacementPlanRunning).
			Update("status", model.PlacementPlanCancelled)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errors.New("placement plan is not running")
		}
		return tx.Model(&model.PlacementTask{}).Where("plan_id = ? AND state = ?", id, model.PlacementRequested).
			Update("state", model.PlacementCancelled).Error
	})
}

func (s *Service) RetryTask(planID, taskID uint, targetNode string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var task model.PlacementTask
		if err := tx.Where("id = ? AND plan_id = ?", taskID, planID).First(&task).Error; err != nil {
			return err
		}
		if task.State != model.PlacementFailed {
			return errors.New("only failed placement tasks can be retried")
		}
		updates := map[string]any{"state": model.PlacementRequested, "error_msg": ""}
		if targetNode != "" {
			updates["target_node"] = targetNode
		}
		if err := tx.Model(&task).Updates(updates).Error; err != nil {
			return err
		}
		return tx.Model(&model.PlacementPlan{}).Where("id = ?", planID).Updates(map[string]any{"status": model.PlacementPlanRunning, "error_msg": ""}).Error
	})
}

func (s *Service) RollbackTask(ctx context.Context, planID, taskID uint) error {
	var task model.PlacementTask
	if err := s.db.Where("id = ? AND plan_id = ?", taskID, planID).First(&task).Error; err != nil {
		return err
	}
	if task.State != model.PlacementFailed || len(task.DeploymentSnapshot) <= 2 || task.Namespace == "" {
		return errors.New("failed task has no restorable Deployment snapshot")
	}
	if err := s.db.Model(&task).Update("state", model.PlacementRollingBack).Error; err != nil {
		return err
	}
	if err := s.k8s.RestoreVTADeploymentSnapshot(ctx, task.Namespace, task.SessionID, task.DeploymentUID, task.DeploymentSnapshot); err != nil {
		_ = s.db.Model(&task).Updates(map[string]any{"state": model.PlacementFailed, "error_msg": err.Error()}).Error
		return err
	}
	now := time.Now().UTC()
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&task).Updates(map[string]any{"state": model.PlacementRolledBack, "completed_at": now, "error_msg": ""}).Error; err != nil {
			return err
		}
		taskID := task.ID
		if err := tx.Create(&model.PlacementEvent{PlanID: task.PlanID, TaskID: &taskID, State: model.PlacementRolledBack}).Error; err != nil {
			return err
		}
		return tx.Model(&model.PlacementPlan{}).Where("id = ?", planID).Updates(map[string]any{
			"status": model.PlacementPlanCancelled, "error_msg": "",
		}).Error
	})
}
