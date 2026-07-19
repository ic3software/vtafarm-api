package handler

import (
	"testing"
	"time"

	"github.com/ic3software/vtafarm-api/internal/k8s"
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
