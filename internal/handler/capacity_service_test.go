package handler

import (
	"testing"

	"github.com/ic3software/vtafarm-api/internal/capacity"
	"github.com/ic3software/vtafarm-api/internal/k8s"
)

const gi = int64(1) << 30

// ampleStats is a cluster with plenty of room for both modes.
func ampleStats() *k8s.ClusterStats {
	return &k8s.ClusterStats{
		Nodes: []k8s.NodeResourceStat{{
			Name: "n1", Schedulable: true,
			CPUAllocatableMillis: 1000, CPURequestedMillis: 100, CPUUsedMillis: 50,
			MemAllocatableBytes: 10 * gi, MemRequestedBytes: gi, MemUsedBytes: gi,
		}},
		StorageNodes:        []k8s.StorageNodeStat{{Name: "n1", Schedulable: true, SchedulableBytes: 100 * gi}},
		StorageReplicaCount: 1,
		MetricsAvailable:    true,
		StorageAvailable:    true,
	}
}

func TestEstimatesFromAmpleAvailable(t *testing.T) {
	est := estimatesFrom(ampleStats())
	if est[capacity.VtaOnly.Name].Count < 1 {
		t.Fatalf("vta_only Count = %d, want >= 1", est[capacity.VtaOnly.Name].Count)
	}
	if est[capacity.FullStack.Name].Count < 1 {
		t.Fatalf("full_stack Count = %d, want >= 1", est[capacity.FullStack.Name].Count)
	}
}

func TestEstimatesFromExhaustedUnavailable(t *testing.T) {
	// Node fully reserved (requested == allocatable) → no room for even the
	// tiny vta_only pod, so the create gate should report unavailable.
	stats := ampleStats()
	stats.Nodes[0].CPURequestedMillis = stats.Nodes[0].CPUAllocatableMillis
	stats.Nodes[0].MemRequestedBytes = stats.Nodes[0].MemAllocatableBytes

	est := estimatesFrom(stats)
	if got := est[capacity.VtaOnly.Name].Count; got != 0 {
		t.Fatalf("vta_only Count = %d, want 0 (node fully reserved)", got)
	}
}

func TestFreeResourcesFailsOpenWhenMetricsUnavailable(t *testing.T) {
	// Live usage all but fills the node, but metrics are unavailable, so the
	// fail-open plan must ignore `used` and fall back to allocatable − requested.
	stats := &k8s.ClusterStats{
		Nodes: []k8s.NodeResourceStat{{
			Name: "n1", Schedulable: true,
			CPUAllocatableMillis: 1000, CPURequestedMillis: 100, CPUUsedMillis: 999,
			MemAllocatableBytes: 10 * gi, MemRequestedBytes: gi, MemUsedBytes: 9 * gi,
		}},
		MetricsAvailable: false,
	}

	nodes, _ := freeResources(stats)
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	if nodes[0].CPUMillis != 900 {
		t.Fatalf("CPU headroom = %d, want 900 (alloc − requested, used ignored)", nodes[0].CPUMillis)
	}
	if nodes[0].MemBytes != 9*gi {
		t.Fatalf("mem headroom = %d, want 9Gi (alloc − requested, used ignored)", nodes[0].MemBytes)
	}
}

func TestFreeResourcesCountsUsageWhenMetricsAvailable(t *testing.T) {
	// Same node, but with metrics available the high live usage is respected:
	// headroom = allocatable − max(requested, used).
	stats := &k8s.ClusterStats{
		Nodes: []k8s.NodeResourceStat{{
			Name: "n1", Schedulable: true,
			CPUAllocatableMillis: 1000, CPURequestedMillis: 100, CPUUsedMillis: 999,
			MemAllocatableBytes: 10 * gi, MemRequestedBytes: gi, MemUsedBytes: 9 * gi,
		}},
		MetricsAvailable: true,
	}

	nodes, _ := freeResources(stats)
	if nodes[0].CPUMillis != 1 {
		t.Fatalf("CPU headroom = %d, want 1 (alloc − used)", nodes[0].CPUMillis)
	}
	if nodes[0].MemBytes != gi {
		t.Fatalf("mem headroom = %d, want 1Gi (alloc − used)", nodes[0].MemBytes)
	}
}

func TestFreeResourcesSkipsUnschedulable(t *testing.T) {
	stats := &k8s.ClusterStats{
		Nodes: []k8s.NodeResourceStat{
			{Name: "cordoned", Schedulable: false, CPUAllocatableMillis: 1000, MemAllocatableBytes: 10 * gi},
			{Name: "ok", Schedulable: true, CPUAllocatableMillis: 1000, MemAllocatableBytes: 10 * gi},
		},
		StorageNodes: []k8s.StorageNodeStat{
			{Name: "cordoned", Schedulable: false, SchedulableBytes: 100 * gi},
			{Name: "ok", Schedulable: true, SchedulableBytes: 100 * gi},
		},
		MetricsAvailable: true,
	}

	nodes, disks := freeResources(stats)
	if len(nodes) != 1 || len(disks) != 1 {
		t.Fatalf("got %d nodes / %d disks, want 1 / 1 (unschedulable excluded)", len(nodes), len(disks))
	}
}
