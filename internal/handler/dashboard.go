package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/capacity"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/resourceprofile"
	"gorm.io/gorm"
)

type DashboardHandler struct {
	db    *gorm.DB
	k8s   *k8s.Client
	usage *usageHighWater
}

func NewDashboardHandler(db *gorm.DB, k8sClient *k8s.Client) *DashboardHandler {
	return &DashboardHandler{
		db:    db,
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
	modes, calculated, err := h.capacityEstimates(ctx, stats)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not calculate capacity"})
		return
	}

	nodes := make([]dashboardNode, len(stats.Nodes))
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
	}

	storageNodes := make([]dashboardStorageNode, len(stats.StorageNodes))
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
	}

	replicas := stats.StorageReplicaCount
	estimates := gin.H{}
	for _, mode := range modes {
		est := calculated[mode.Name]
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

// AvailableVTAOnly returns the same strict capacity shown by the Admin
// Dashboard. Pool admission calls this while holding its database advisory
// lock and before inserting any batch, user, or setup-session row.
func (h *DashboardHandler) AvailableVTAOnly(ctx context.Context) (int, error) {
	if h.k8s == nil {
		return 0, errors.New("kubernetes is not available")
	}
	stats, err := h.k8s.ClusterResourceStats(ctx)
	if err != nil {
		return 0, err
	}
	_, estimates, err := h.capacityEstimates(ctx, stats)
	if err != nil {
		return 0, err
	}
	return estimates[capacity.VtaOnly.Name].Count, nil
}

func (h *DashboardHandler) capacityEstimates(ctx context.Context, stats *k8s.ClusterStats) ([]capacity.Mode, map[string]capacity.Estimate, error) {
	planningUsage := h.usage.Observe(time.Now(), stats.Nodes, stats.MetricsAvailable)
	var freeNodes []capacity.NodeFree
	for _, node := range stats.Nodes {
		if !node.Schedulable {
			continue
		}
		usage, ok := planningUsage[node.Name]
		var cpuFree, memoryFree int64
		if ok {
			cpuFree = capacity.PlanningHeadroom(node.CPUAllocatableMillis, node.CPURequestedMillis, usage.CPUMillis)
			memoryFree = capacity.PlanningHeadroom(node.MemAllocatableBytes, node.MemRequestedBytes, usage.MemBytes)
		}
		freeNodes = append(freeNodes, capacity.NodeFree{CPUMillis: cpuFree, MemBytes: memoryFree})
	}
	freeDisks := make([]capacity.DiskFree, 0, len(stats.StorageNodes))
	for _, disk := range stats.StorageNodes {
		freeDisks = append(freeDisks, capacity.DiskFree{Bytes: disk.SchedulableBytes})
	}
	profiles, err := resourceprofile.Load(ctx, h.db)
	if err != nil {
		return nil, nil, err
	}
	modes := capacity.Modes(profiles)
	estimates := make(map[string]capacity.Estimate, len(modes))
	for _, mode := range modes {
		estimates[mode.Name] = capacity.EstimateMode(
			mode, freeNodes, freeDisks, stats.StorageReplicaCount, stats.StorageAvailable,
		)
	}

	reserved, err := h.nonRunningPoolSessions(ctx)
	if err != nil {
		return nil, nil, err
	}
	if estimate, ok := estimates[capacity.VtaOnly.Name]; ok {
		estimates[capacity.VtaOnly.Name] = subtractReservedSessions(estimate, reserved)
	}
	return modes, estimates, nil
}

// A parked Deployment has zero replicas and therefore does not appear in pod
// CPU or memory requests. Reserve one future VTA footprint until the pooled
// session is running; otherwise repeated pool admissions could all consume the
// same capacity displayed by the Dashboard.
func (h *DashboardHandler) nonRunningPoolSessions(ctx context.Context) (int, error) {
	if !h.db.Migrator().HasTable(&model.PoolSession{}) {
		return 0, nil
	}
	var count int64
	err := h.db.WithContext(ctx).Table("pool_sessions ps").
		Joins("JOIN setup_sessions s ON s.id = ps.session_id").
		Where("s.status <> ?", "running").Count(&count).Error
	return int(count), err
}

func subtractReservedSessions(estimate capacity.Estimate, reserved int) capacity.Estimate {
	if reserved <= 0 {
		return estimate
	}
	subtract := func(value int) int {
		if value < 0 {
			return value
		}
		return max(value-reserved, 0)
	}
	estimate.Count = subtract(estimate.Count)
	estimate.ByCPU = subtract(estimate.ByCPU)
	estimate.ByMemory = subtract(estimate.ByMemory)
	estimate.ByStorage = subtract(estimate.ByStorage)
	estimate.LimitingResource = "cpu"
	minimum := estimate.ByCPU
	if estimate.ByMemory < minimum {
		estimate.LimitingResource, minimum = "memory", estimate.ByMemory
	}
	if estimate.ByStorage >= 0 && estimate.ByStorage < minimum {
		estimate.LimitingResource = "storage"
	}
	return estimate
}
