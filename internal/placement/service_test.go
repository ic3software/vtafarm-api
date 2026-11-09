package placement

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
	"gorm.io/gorm"
)

func TestActiveWorkloadViewsExcludeParkedPoolAndNonVTAOnlySessions(t *testing.T) {
	placements := []k8s.VTAPlacement{{SessionID: 41}, {SessionID: 42}, {SessionID: 43}, {SessionID: 44}}
	sessions := []model.SetupSession{
		{ID: 41, Mode: model.ModeVtaOnly, Status: "running", VtaName: "active-vta"},
		{ID: 42, Mode: model.ModeVtaOnly, Status: model.StatusPoolWaiting, VtaName: "parked-pool"},
		{ID: 43, Mode: model.ModeFullStack, Status: "running", VtaName: "full-stack-vta"},
	}

	got := activeWorkloadViews(placements, sessions)
	if len(got) != 1 || got[0].SessionID != 41 || got[0].VTAName != "active-vta" {
		t.Fatalf("activeWorkloadViews() = %#v, want only the running VTA-only session", got)
	}
}

func TestSafestNodeIncludesReservationsAndP95(t *testing.T) {
	nodes := []k8s.NodePlacement{
		{Name: "busy", Schedulable: true, Eligible: true, MetricsAvailable: true, MemAllocatable: 1000, MemUsed: 600},
		{Name: "safe", Schedulable: true, Eligible: true, MetricsAvailable: true, MemAllocatable: 1000, MemUsed: 400, MemRequested: 300},
	}

	got := safestNode(nodes, map[string]int64{"safe": 300}, 100, 90)
	if got != "busy" {
		t.Fatalf("safestNode = %q, want busy after reservation projection", got)
	}
}

func TestSafestNodeExceptDoesNotRecommendCurrentNode(t *testing.T) {
	nodes := []k8s.NodePlacement{
		{Name: "current", Schedulable: true, Eligible: true, MetricsAvailable: true, MemAllocatable: 1000, MemUsed: 100},
		{Name: "destination", Schedulable: true, Eligible: true, MetricsAvailable: true, MemAllocatable: 1000, MemUsed: 200},
	}

	got := safestNodeExcept(nodes, nil, 100, 90, "current")
	if got != "destination" {
		t.Fatalf("safestNodeExcept = %q, want destination", got)
	}
}

func TestSourceNeedsRelocationForHighOrUnsafeSources(t *testing.T) {
	nodes := map[string]k8s.NodePlacement{
		"healthy":       {Name: "healthy", Schedulable: true, MetricsAvailable: true, MemoryState: "green", MemAllocatable: 1000, MemUsed: 120},
		"elevated":      {Name: "elevated", Schedulable: true, MetricsAvailable: true, MemoryState: "yellow", MemAllocatable: 1000, MemUsed: 840},
		"high":          {Name: "high", Schedulable: true, MetricsAvailable: true, MemoryState: "orange", MemAllocatable: 1000, MemUsed: 850},
		"high-by-usage": {Name: "high-by-usage", Schedulable: true, MetricsAvailable: true, MemoryState: "yellow", MemAllocatable: 1000, MemUsed: 850},
		"hot":           {Name: "hot", Schedulable: true, MetricsAvailable: true, MemoryState: "hot", MemAllocatable: 1000, MemUsed: 900},
		"stale":         {Name: "stale", Schedulable: true, MetricsAvailable: false, MemAllocatable: 1000, MemUsed: 120},
	}

	for _, test := range []struct {
		name   string
		source string
		want   bool
	}{
		{name: "healthy source stays put", source: "healthy", want: false},
		{name: "elevated source stays put", source: "elevated", want: false},
		{name: "high source moves", source: "high", want: true},
		{name: "source at high threshold moves", source: "high-by-usage", want: true},
		{name: "hot source moves", source: "hot", want: true},
		{name: "stale metrics move", source: "stale", want: true},
		{name: "unscheduled workload moves", source: "", want: true},
		{name: "missing source moves", source: "missing", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sourceNeedsRelocation(test.source, nodes, 85); got != test.want {
				t.Fatalf("sourceNeedsRelocation(%q) = %v, want %v", test.source, got, test.want)
			}
		})
	}
}

func TestNewTaskEventCapturesTransitionActorAndMetadata(t *testing.T) {
	adminID := uint(17)
	task := model.PlacementTask{ID: 23, PlanID: 11}
	event, err := newTaskEvent(&task, model.PlacementFailed, model.PlacementRequested, model.PlacementActorAdmin, &adminID, "task retried", map[string]any{
		"state": model.PlacementRequested, "target_node": "worker-2", "error_msg": "",
	})
	if err != nil {
		t.Fatalf("newTaskEvent: %v", err)
	}
	if event.EventType != model.PlacementEventTaskState || event.FromState != model.PlacementFailed || event.State != model.PlacementRequested {
		t.Fatalf("transition fields = %+v", event)
	}
	if event.ActorType != model.PlacementActorAdmin || event.ActorID == nil || *event.ActorID != adminID || event.Message != "task retried" {
		t.Fatalf("actor fields = %+v", event)
	}
	var metadata map[string]any
	if err := json.Unmarshal(event.Metadata, &metadata); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if metadata["target_node"] != "worker-2" || len(metadata) != 1 {
		t.Fatalf("metadata = %#v, want only target_node", metadata)
	}
}

func TestNewServiceRejectsUnsafeThresholdOrder(t *testing.T) {
	cfg := config.PlacementConfig{
		GreenBelowPercent: 85, YellowBelowPercent: 70, HotAtPercent: 90,
		MetricsStaleSeconds: 90, PollSeconds: 15, VTAP95Memory: "128Mi",
	}
	if _, err := NewService(nil, nil, cfg); err == nil {
		t.Fatal("NewService accepted unordered thresholds")
	}
}

func TestCompletePlanIfDoneCompletesPlanImmediately(t *testing.T) {
	db := testutil.Postgres(t)
	session := testutil.SetupSession(t, db, model.ModeVtaOnly)
	plan := model.PlacementPlan{Kind: model.PlacementPlanReconcile, Status: model.PlacementPlanRunning}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	task := model.PlacementTask{PlanID: plan.ID, SessionID: session.ID, State: model.PlacementCompleted}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}

	runner := Runner{db: db}
	if err := runner.completePlanIfDone(plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&plan, plan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if plan.Status != model.PlacementPlanCompleted {
		t.Fatalf("plan status = %q, want completed", plan.Status)
	}
	var events int64
	if err := db.Model(&model.PlacementEvent{}).Where("plan_id = ? AND event_type = ? AND state = ?", plan.ID,
		model.PlacementEventPlanStatus, model.PlacementPlanCompleted).Count(&events).Error; err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("completion events = %d, want 1", events)
	}
}

func TestAcquireLockAndReservationAllowsProvisionPlanDuringUpgrade(t *testing.T) {
	db := testutil.Postgres(t)
	session := testutil.SetupSession(t, db, model.ModeVtaOnly)
	createActiveUpgrade(t, db, session)
	task := createPlacementTask(t, db, session.ID, model.PlacementPlanProvision)

	runner := Runner{db: db}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return runner.acquireLockAndReservation(tx, &task, "worker-2", time.Now().UTC())
	}); err != nil {
		t.Fatalf("provisioning reservation during upgrade: %v", err)
	}

	var reservations int64
	if err := db.Model(&model.PlacementReservation{}).Where("task_id = ? AND status = ?", task.ID, "active").Count(&reservations).Error; err != nil {
		t.Fatal(err)
	}
	if reservations != 1 {
		t.Fatalf("active reservations = %d, want 1", reservations)
	}
}

func TestAcquireLockAndReservationRejectsRelocationDuringUpgrade(t *testing.T) {
	db := testutil.Postgres(t)
	session := testutil.SetupSession(t, db, model.ModeVtaOnly)
	createActiveUpgrade(t, db, session)
	task := createPlacementTask(t, db, session.ID, model.PlacementPlanRelocate)

	runner := Runner{db: db}
	err := db.Transaction(func(tx *gorm.DB) error {
		return runner.acquireLockAndReservation(tx, &task, "worker-2", time.Now().UTC())
	})
	if err == nil || !strings.Contains(err.Error(), "active upgrade") {
		t.Fatalf("relocation reservation error = %v, want active upgrade rejection", err)
	}
}

func createActiveUpgrade(t *testing.T, db *gorm.DB, session model.SetupSession) {
	t.Helper()
	batch := model.UpgradeBatch{UserID: &session.UserID, Status: model.UpgradeBatchRunning}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	task := model.UpgradeTask{
		BatchID: batch.ID, SessionID: session.ID, Component: "vta",
		ToImage: "ghcr.io/ic3software/vta:test", Status: model.UpgradeTaskRunning,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
}

func createPlacementTask(t *testing.T, db *gorm.DB, sessionID uint, kind string) model.PlacementTask {
	t.Helper()
	plan := model.PlacementPlan{Kind: kind, Status: model.PlacementPlanRunning}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	task := model.PlacementTask{
		PlanID: plan.ID, SessionID: sessionID, State: model.PlacementRequested,
		P95MemoryBytes: 128 * 1024 * 1024,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	return task
}

func TestCancelPlanRejectsStaleRunningPlanWithoutQueuedTasks(t *testing.T) {
	db := testutil.Postgres(t)
	session := testutil.SetupSession(t, db, model.ModeVtaOnly)
	plan := model.PlacementPlan{Kind: model.PlacementPlanReconcile, Status: model.PlacementPlanRunning}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	task := model.PlacementTask{PlanID: plan.ID, SessionID: session.ID, State: model.PlacementCompleted}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}

	service := Service{db: db}
	err := service.CancelPlan(plan.ID, 7)
	if err == nil || !strings.Contains(err.Error(), "no queued tasks") {
		t.Fatalf("CancelPlan() error = %v, want no queued tasks", err)
	}
	if err := db.First(&plan, plan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if plan.Status != model.PlacementPlanRunning {
		t.Fatalf("plan status = %q, want unchanged running status", plan.Status)
	}
}
