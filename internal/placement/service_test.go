package placement

import (
	"encoding/json"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
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
