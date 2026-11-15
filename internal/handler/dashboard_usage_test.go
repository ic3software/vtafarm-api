package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/metrics"
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

func TestNodeMetricValues(t *testing.T) {
	values, err := nodeMetricValues([]metrics.Series{
		{Labels: map[string]string{"node": "node-1"}, Points: []metrics.Point{{Value: 1.25}}},
		{Labels: map[string]string{"node": "node-2"}, Points: []metrics.Point{{Value: 2.5}}},
	})
	if err != nil {
		t.Fatalf("nodeMetricValues() error = %v", err)
	}
	if values["node-1"] != 1.25 || values["node-2"] != 2.5 {
		t.Fatalf("nodeMetricValues() = %v", values)
	}
}

func TestNodeMetricValuesRejectsEmptySamples(t *testing.T) {
	if _, err := nodeMetricValues(nil); err == nil {
		t.Fatal("nodeMetricValues() accepted empty samples")
	}
}

func TestApplyPrometheusUsage(t *testing.T) {
	server := dashboardMetricsServer(t, true)
	defer server.Close()
	h := &DashboardHandler{metrics: metrics.NewClient(server.URL, time.Second)}
	stats := &k8s.ClusterStats{Nodes: []k8s.NodeResourceStat{{Name: "node-1"}, {Name: "node-2"}}}
	if err := h.applyPrometheusUsage(t.Context(), stats); err != nil {
		t.Fatalf("applyPrometheusUsage() error = %v", err)
	}
	if stats.Nodes[0].CPUUsedMillis != 1250 || stats.Nodes[0].MemUsedBytes != 1024 {
		t.Fatalf("node-1 usage = %+v", stats.Nodes[0])
	}
	if stats.Nodes[1].CPUUsedMillis != 2500 || stats.Nodes[1].MemUsedBytes != 2048 {
		t.Fatalf("node-2 usage = %+v", stats.Nodes[1])
	}
}

func TestApplyPrometheusUsageDoesNotPartiallyReplaceFallback(t *testing.T) {
	server := dashboardMetricsServer(t, false)
	defer server.Close()
	h := &DashboardHandler{metrics: metrics.NewClient(server.URL, time.Second)}
	stats := &k8s.ClusterStats{Nodes: []k8s.NodeResourceStat{
		{Name: "node-1", CPUUsedMillis: 100, MemUsedBytes: 200},
		{Name: "node-2", CPUUsedMillis: 300, MemUsedBytes: 400},
	}}
	if err := h.applyPrometheusUsage(t.Context(), stats); err == nil {
		t.Fatal("applyPrometheusUsage() accepted incomplete samples")
	}
	if stats.Nodes[0].CPUUsedMillis != 100 || stats.Nodes[0].MemUsedBytes != 200 {
		t.Fatalf("fallback usage was partially replaced: %+v", stats.Nodes[0])
	}
}

func dashboardMetricsServer(t *testing.T, includeNode2 bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		values := []string{`{"metric":{"node":"node-1"},"value":[1,"1.25"]}`}
		if strings.Contains(r.Form.Get("query"), "memory_working_set_bytes") {
			values = []string{`{"metric":{"node":"node-1"},"value":[1,"1024"]}`}
		}
		if includeNode2 {
			if strings.Contains(r.Form.Get("query"), "memory_working_set_bytes") {
				values = append(values, `{"metric":{"node":"node-2"},"value":[1,"2048"]}`)
			} else {
				values = append(values, `{"metric":{"node":"node-2"},"value":[1,"2.5"]}`)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, strings.Join(values, ","))
	}))
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
