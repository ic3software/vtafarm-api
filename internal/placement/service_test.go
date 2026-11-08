package placement

import (
	"testing"

	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/k8s"
)

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

func TestNewServiceRejectsUnsafeThresholdOrder(t *testing.T) {
	cfg := config.PlacementConfig{
		GreenBelowPercent: 85, YellowBelowPercent: 70, HotAtPercent: 90,
		MetricsStaleSeconds: 90, PollSeconds: 15, VTAP95Memory: "128Mi",
	}
	if _, err := NewService(nil, nil, cfg); err == nil {
		t.Fatal("NewService accepted unordered thresholds")
	}
}
