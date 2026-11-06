package handler

import (
	"testing"
	"time"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/resourceprofile"
)

func TestUsageHighWater(t *testing.T) {
	history := newUsageHighWater(15 * time.Minute)
	start := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	got := history.Observe(start, []k8s.NodeResourceStat{{
		Name: "node-1", CPUUsedMillis: 100, MemUsedBytes: 200,
	}}, true)
	assertNodeUsage(t, got, "node-1", 100, 200)

	got = history.Observe(start.Add(time.Minute), []k8s.NodeResourceStat{{
		Name: "node-1", CPUUsedMillis: 150, MemUsedBytes: 180,
	}}, true)
	assertNodeUsage(t, got, "node-1", 150, 200)

	// A brief metrics failure reuses the high-water sample instead of falling
	// back to request-only capacity.
	got = history.Observe(start.Add(2*time.Minute), nil, false)
	assertNodeUsage(t, got, "node-1", 150, 200)

	// Once every cached sample expires and no live reading exists, the node is
	// omitted so capacity calculation can fail closed.
	got = history.Observe(start.Add(17*time.Minute+time.Nanosecond), nil, false)
	if _, ok := got["node-1"]; ok {
		t.Fatal("expired node usage was retained")
	}
}

func TestUsageHighWaterWithoutMetrics(t *testing.T) {
	history := newUsageHighWater(15 * time.Minute)
	got := history.Observe(time.Now(), []k8s.NodeResourceStat{{Name: "node-1"}}, false)
	if len(got) != 0 {
		t.Fatalf("Observe() returned %v without a live or cached sample", got)
	}
}

func TestCapacityEstimatesReservePoolVTAResourcesForEveryMode(t *testing.T) {
	const mi = int64(1) << 20
	h := &DashboardHandler{usage: newUsageHighWater(15 * time.Minute)}
	stats := &k8s.ClusterStats{
		Nodes: []k8s.NodeResourceStat{{
			Name: "node-1", Schedulable: true,
			CPUAllocatableMillis: 3500, CPURequestedMillis: 2900, CPUUsedMillis: 700,
			MemAllocatableBytes: 6656 * mi, MemRequestedBytes: 3600 * mi, MemUsedBytes: 5376 * mi,
		}},
		StorageNodes:        []k8s.StorageNodeStat{{SchedulableBytes: 32800 * mi}},
		MetricsAvailable:    true,
		StorageAvailable:    true,
		StorageReplicaCount: 1,
	}

	_, estimates := h.capacityEstimatesWithProfilesAndReservations(stats, resourceprofile.Factory(), 7)
	vtaOnly := estimates["vta_only"]
	if vtaOnly.Count != 13 || vtaOnly.ByCPU != 53 || vtaOnly.ByMemory != 13 || vtaOnly.ByStorage != 157 {
		t.Fatalf("VTA-only estimate = %+v", vtaOnly)
	}
	fullStack := estimates["full_stack"]
	if fullStack.Count != 1 || fullStack.ByCPU != 6 || fullStack.ByMemory != 1 || fullStack.ByStorage != 19 {
		t.Fatalf("full-stack estimate = %+v", fullStack)
	}
	if fullStack.LimitingResource != "memory" {
		t.Fatalf("full-stack limiting resource = %q, want memory", fullStack.LimitingResource)
	}
}

func assertNodeUsage(t *testing.T, got map[string]nodeUsage, node string, cpu, memory int64) {
	t.Helper()
	usage, ok := got[node]
	if !ok {
		t.Fatalf("node %q missing from usage map", node)
	}
	if usage.CPUMillis != cpu || usage.MemBytes != memory {
		t.Fatalf("usage = %+v, want CPU=%d memory=%d", usage, cpu, memory)
	}
}
