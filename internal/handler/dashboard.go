package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/capacity"
	"github.com/ic3software/vtafarm-api/internal/k8s"
)

type DashboardHandler struct {
	k8s   *k8s.Client
	usage *usageHighWater
}

func NewDashboardHandler(k8sClient *k8s.Client) *DashboardHandler {
	return &DashboardHandler{
		k8s:   k8sClient,
		usage: newUsageHighWater(capacityUsageWindow),
	}
}

type dashboardNode struct {
	Name                 string `json:"name"`
	Schedulable          bool   `json:"schedulable"`
	CPUAllocatableMillis int64  `json:"cpu_allocatable_millis"`
	CPURequestedMillis   int64  `json:"cpu_requested_millis"`
	CPUUsedMillis        int64  `json:"cpu_used_millis"`
	MemAllocatableBytes  int64  `json:"mem_allocatable_bytes"`
	MemRequestedBytes    int64  `json:"mem_requested_bytes"`
	MemUsedBytes         int64  `json:"mem_used_bytes"`
}

type dashboardStorageNode struct {
	Name             string `json:"name"`
	Schedulable      bool   `json:"schedulable"`
	MaximumBytes     int64  `json:"maximum_bytes"`
	ReservedBytes    int64  `json:"reserved_bytes"`
	ScheduledBytes   int64  `json:"scheduled_bytes"`
	AvailableBytes   int64  `json:"available_bytes"`
	SchedulableBytes int64  `json:"schedulable_bytes"`
}

type dashboardEstimate struct {
	Count            int    `json:"count"`
	ByCPU            int    `json:"by_cpu"`
	ByMemory         int    `json:"by_memory"`
	ByStorage        int    `json:"by_storage"` // -1 when storage stats unavailable
	LimitingResource string `json:"limiting_resource"`

	CPUMillisPerSession    int64 `json:"cpu_millis_per_session"`
	MemBytesPerSession     int64 `json:"mem_bytes_per_session"`     // sum of component memory limits
	StorageBytesPerSession int64 `json:"storage_bytes_per_session"` // includes replica factor
}

// Get — GET /api/v1/admin/dashboard. Whole-cluster resource totals, per-node
// breakdowns, and how many more sessions of each mode still fit.
func (h *DashboardHandler) Get(c *gin.Context) {
	if h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "kubernetes is not available"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	stats, err := h.k8s.ClusterResourceStats(ctx)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "cluster stats: " + err.Error()})
		return
	}
	planningUsage := h.usage.Observe(time.Now(), stats.Nodes, stats.MetricsAvailable)

	nodes := make([]dashboardNode, len(stats.Nodes))
	var freeNodes []capacity.NodeFree
	var totalCPU, totalCPUReq, totalCPUUsed, totalMem, totalMemReq, totalMemUsed int64
	for i, n := range stats.Nodes {
		nodes[i] = dashboardNode{
			Name:                 n.Name,
			Schedulable:          n.Schedulable,
			CPUAllocatableMillis: n.CPUAllocatableMillis,
			CPURequestedMillis:   n.CPURequestedMillis,
			CPUUsedMillis:        n.CPUUsedMillis,
			MemAllocatableBytes:  n.MemAllocatableBytes,
			MemRequestedBytes:    n.MemRequestedBytes,
			MemUsedBytes:         n.MemUsedBytes,
		}
		totalCPU += n.CPUAllocatableMillis
		totalCPUReq += n.CPURequestedMillis
		totalCPUUsed += n.CPUUsedMillis
		totalMem += n.MemAllocatableBytes
		totalMemReq += n.MemRequestedBytes
		totalMemUsed += n.MemUsedBytes

		// Estimates draw only on nodes new pods can land on.
		if n.Schedulable {
			usage, usageAvailable := planningUsage[n.Name]
			var cpuFree, memFree int64
			if usageAvailable {
				cpuFree = capacity.PlanningHeadroom(
					n.CPUAllocatableMillis,
					n.CPURequestedMillis,
					usage.CPUMillis,
				)
				memFree = capacity.PlanningHeadroom(
					n.MemAllocatableBytes,
					n.MemRequestedBytes,
					usage.MemBytes,
				)
			}
			freeNodes = append(freeNodes, capacity.NodeFree{
				CPUMillis: cpuFree,
				MemBytes:  memFree,
			})
		}
	}

	storageNodes := make([]dashboardStorageNode, len(stats.StorageNodes))
	var freeDisks []capacity.DiskFree
	var totalStorage, totalStorageReserved, totalStorageScheduled, totalStorageAvail, totalStorageSched int64
	for i, s := range stats.StorageNodes {
		storageNodes[i] = dashboardStorageNode{
			Name:             s.Name,
			Schedulable:      s.Schedulable,
			MaximumBytes:     s.MaximumBytes,
			ReservedBytes:    s.ReservedBytes,
			ScheduledBytes:   s.ScheduledBytes,
			AvailableBytes:   s.AvailableBytes,
			SchedulableBytes: s.SchedulableBytes,
		}
		totalStorage += s.MaximumBytes
		totalStorageReserved += s.ReservedBytes
		totalStorageScheduled += s.ScheduledBytes
		totalStorageAvail += s.AvailableBytes
		totalStorageSched += s.SchedulableBytes
		freeDisks = append(freeDisks, capacity.DiskFree{Bytes: s.SchedulableBytes})
	}

	replicas := stats.StorageReplicaCount
	estimates := gin.H{}
	for _, mode := range []capacity.Mode{capacity.VtaOnly, capacity.FullStack} {
		est := capacity.EstimateMode(mode, freeNodes, freeDisks, replicas, stats.StorageAvailable)
		var cpuCost, memCost, storageCost int64
		for _, comp := range mode.Components {
			cpuCost += comp.CPUMillis
			memCost += comp.MemBytes
			storageCost += comp.StorageBytes * int64(replicas)
		}
		estimates[mode.Name] = dashboardEstimate{
			Count:                  est.Count,
			ByCPU:                  est.ByCPU,
			ByMemory:               est.ByMemory,
			ByStorage:              est.ByStorage,
			LimitingResource:       est.LimitingResource,
			CPUMillisPerSession:    cpuCost,
			MemBytesPerSession:     memCost,
			StorageBytesPerSession: storageCost,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"cluster": gin.H{
			"cpu": gin.H{
				"allocatable_millis": totalCPU,
				"requested_millis":   totalCPUReq,
				"used_millis":        totalCPUUsed,
			},
			"memory": gin.H{
				"allocatable_bytes": totalMem,
				"requested_bytes":   totalMemReq,
				"used_bytes":        totalMemUsed,
			},
			"storage": gin.H{
				"maximum_bytes":      totalStorage,
				"reserved_bytes":     totalStorageReserved,
				"scheduled_bytes":    totalStorageScheduled,
				"available_bytes":    totalStorageAvail,
				"schedulable_bytes":  totalStorageSched,
				"data_written_bytes": stats.StorageDataWrittenBytes,
				"replica_count":      replicas,
			},
		},
		"nodes":             nodes,
		"storage_nodes":     storageNodes,
		"metrics_available": stats.MetricsAvailable,
		"storage_available": stats.StorageAvailable,
		"estimates":         estimates,
	})
}
