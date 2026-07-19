package handler

import (
	"testing"

	"github.com/ic3software/vtafarm-api/internal/k8s"
)

func TestCapacityIssues(t *testing.T) {
	healthy := func() *k8s.ClusterStats {
		return &k8s.ClusterStats{
			MetricsAvailable: true,
			StorageAvailable: true,
			Nodes: []k8s.NodeResourceStat{{
				Name:                 "node-1",
				CPUAllocatableMillis: 4000, CPUUsedMillis: 1000,
				MemAllocatableBytes: 8 << 30, MemUsedBytes: 2 << 30,
			}},
			StorageNodes: []k8s.StorageNodeStat{{
				Name:         "node-1",
				MaximumBytes: 100 << 30, AvailableBytes: 60 << 30,
			}},
		}
	}

	t.Run("all under thresholds", func(t *testing.T) {
		if got := capacityIssues(healthy(), 90, 90, 85); len(got) != 0 {
			t.Fatalf("expected no issues, got %+v", got)
		}
	})

	t.Run("memory over threshold", func(t *testing.T) {
		stats := healthy()
		stats.Nodes[0].MemUsedBytes = 7500 << 20 // ~91.5% of 8Gi
		got := capacityIssues(stats, 90, 90, 85)
		if len(got) != 1 || got[0].Resource != "memory" || got[0].Node != "node-1" {
			t.Fatalf("expected one memory issue, got %+v", got)
		}
		if got[0].Percent < 90 || got[0].Threshold != 90 {
			t.Fatalf("unexpected percent/threshold: %+v", got[0])
		}
	})

	t.Run("storage over threshold", func(t *testing.T) {
		stats := healthy()
		stats.StorageNodes[0].AvailableBytes = 10 << 30 // 90% used
		got := capacityIssues(stats, 90, 90, 85)
		if len(got) != 1 || got[0].Resource != "storage" {
			t.Fatalf("expected one storage issue, got %+v", got)
		}
	})

	t.Run("unreadable sources are issues themselves", func(t *testing.T) {
		stats := healthy()
		stats.MetricsAvailable = false
		stats.StorageAvailable = false
		got := capacityIssues(stats, 90, 90, 85)
		if len(got) != 2 {
			t.Fatalf("expected metrics + storage_stats issues, got %+v", got)
		}
		if got[0].Resource != "metrics" || got[1].Resource != "storage_stats" {
			t.Fatalf("unexpected resources: %+v", got)
		}
	})

	t.Run("zero allocatable never divides", func(t *testing.T) {
		stats := healthy()
		stats.Nodes[0].CPUAllocatableMillis = 0
		stats.StorageNodes[0].MaximumBytes = 0
		if got := capacityIssues(stats, 90, 90, 85); len(got) != 0 {
			t.Fatalf("expected no issues, got %+v", got)
		}
	})
}
