package placement

import (
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

func TestSourceNeedsRelocationOnlyForUnsafeSources(t *testing.T) {
	nodes := map[string]k8s.NodePlacement{
		"healthy": {Name: "healthy", Schedulable: true, MetricsAvailable: true, MemAllocatable: 1000, MemUsed: 120},
		"hot":     {Name: "hot", Schedulable: true, MetricsAvailable: true, MemoryState: "hot", MemAllocatable: 1000, MemUsed: 850},
		"stale":   {Name: "stale", Schedulable: true, MetricsAvailable: false, MemAllocatable: 1000, MemUsed: 120},
	}

	for _, test := range []struct {
		name   string
		source string
		want   bool
	}{
		{name: "healthy source stays put", source: "healthy", want: false},
		{name: "hot source moves", source: "hot", want: true},
		{name: "stale metrics move", source: "stale", want: true},
		{name: "unscheduled workload moves", source: "", want: true},
		{name: "missing source moves", source: "missing", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sourceNeedsRelocation(test.source, nodes, 90); got != test.want {
				t.Fatalf("sourceNeedsRelocation(%q) = %v, want %v", test.source, got, test.want)
			}
		})
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
