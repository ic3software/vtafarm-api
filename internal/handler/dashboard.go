package handler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/capacity"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/metrics"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/resourceprofile"
	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"
)

type DashboardHandler struct {
	db      *gorm.DB
	k8s     *k8s.Client
	metrics *metrics.Client
	usage   *usageHighWater
}

func NewDashboardHandler(db *gorm.DB, k8sClient *k8s.Client, metricsClient *metrics.Client) *DashboardHandler {
	return &DashboardHandler{
		db:      db,
		k8s:     k8sClient,
		metrics: metricsClient,
		usage:   newUsageHighWater(capacityUsageWindow),
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
	requestedSource := c.DefaultQuery("source", "auto")
	if requestedSource != "auto" && requestedSource != "prometheus" && requestedSource != "kubernetes" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source must be auto, prometheus, or kubernetes"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	stats, err := h.k8s.ClusterResourceStats(ctx)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "cluster stats: " + err.Error()})
		return
	}
	kubernetesMetricsAvailable := stats.MetricsAvailable
	metricsSource := "unavailable"
	metricsWarning := ""
	if requestedSource != "kubernetes" && h.metrics != nil {
		if err := h.applyPrometheusUsage(ctx, stats); err == nil {
			metricsSource = "prometheus"
			stats.MetricsAvailable = true
		} else {
			metricsWarning = "Prometheus metrics are unavailable; using Kubernetes metrics when available."
		}
	}
	if metricsSource != "prometheus" {
		stats.MetricsAvailable = kubernetesMetricsAvailable
		if kubernetesMetricsAvailable {
			metricsSource = "kubernetes"
		}
		if requestedSource == "prometheus" && h.metrics == nil {
			metricsWarning = "Prometheus is not configured; using Kubernetes metrics when available."
		}
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
		"metrics_source":    metricsSource,
		"metrics_warning":   metricsWarning,
		"storage_available": stats.StorageAvailable,
		"estimates":         estimates,
	})
}

const dashboardCPUQuery = `sum by (node) (max by (node, namespace, pod, container) (rate(container_cpu_usage_seconds_total{container!="",container!="POD",image!="",node!=""}[1m])))`
const dashboardMemoryQuery = `sum by (node) (max by (node, namespace, pod, container) (container_memory_working_set_bytes{container!="",container!="POD",image!="",node!=""}))`

func (h *DashboardHandler) applyPrometheusUsage(ctx context.Context, stats *k8s.ClusterStats) error {
	if h.metrics == nil {
		return errors.New("prometheus is not configured")
	}
	var cpuSeries, memorySeries []metrics.Series
	group, queryCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var err error
		cpuSeries, _, err = h.metrics.Instant(queryCtx, "node_cpu_usage_cores", dashboardCPUQuery, time.Now().UTC())
		return err
	})
	group.Go(func() error {
		var err error
		memorySeries, _, err = h.metrics.Instant(queryCtx, "node_memory_working_set_bytes", dashboardMemoryQuery, time.Now().UTC())
		return err
	})
	if err := group.Wait(); err != nil {
		return err
	}
	cpuByNode, err := nodeMetricValues(cpuSeries)
	if err != nil {
		return fmt.Errorf("prometheus CPU usage: %w", err)
	}
	memoryByNode, err := nodeMetricValues(memorySeries)
	if err != nil {
		return fmt.Errorf("prometheus memory usage: %w", err)
	}
	for _, node := range stats.Nodes {
		_, cpuOK := cpuByNode[node.Name]
		_, memoryOK := memoryByNode[node.Name]
		if !cpuOK || !memoryOK {
			return fmt.Errorf("prometheus has no complete sample for node %s", node.Name)
		}
	}
	for i := range stats.Nodes {
		stats.Nodes[i].CPUUsedMillis = int64(math.Round(cpuByNode[stats.Nodes[i].Name] * 1000))
		stats.Nodes[i].MemUsedBytes = int64(math.Round(memoryByNode[stats.Nodes[i].Name]))
	}
	return nil
}

func nodeMetricValues(series []metrics.Series) (map[string]float64, error) {
	values := make(map[string]float64, len(series))
	for _, item := range series {
		node := item.Labels["node"]
		if node == "" || len(item.Points) == 0 {
			continue
		}
		value := item.Points[len(item.Points)-1].Value
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("invalid sample for node %s", node)
		}
		values[node] = value
	}
	if len(values) == 0 {
		return nil, errors.New("no node samples")
	}
	return values, nil
}

// clusterCapacitySnapshot reads the slow cluster state without holding the pool
// mutation lock. Callers can load database-backed profiles concurrently.
func (h *DashboardHandler) clusterCapacitySnapshot(ctx context.Context) (*k8s.ClusterStats, error) {
	if h.k8s == nil {
		return nil, errors.New("kubernetes is not available")
	}
	return h.k8s.ClusterResourceStats(ctx)
}

func (h *DashboardHandler) vtaOnlyCapacityFromSnapshot(stats *k8s.ClusterStats, profiles resourceprofile.Profiles) int {
	_, estimates := h.capacityEstimatesWithProfiles(stats, profiles)
	return estimates[capacity.VtaOnly.Name].Count
}

func (h *DashboardHandler) capacityEstimates(ctx context.Context, stats *k8s.ClusterStats) ([]capacity.Mode, map[string]capacity.Estimate, error) {
	profiles, err := resourceprofile.Load(ctx, h.db)
	if err != nil {
		return nil, nil, err
	}
	reserved, err := h.nonRunningPoolSessions(ctx)
	if err != nil {
		return nil, nil, err
	}
	modes, estimates := h.capacityEstimatesWithProfilesAndReservations(stats, profiles, reserved)
	return modes, estimates, nil
}

func (h *DashboardHandler) capacityEstimatesWithProfiles(stats *k8s.ClusterStats, profiles resourceprofile.Profiles) ([]capacity.Mode, map[string]capacity.Estimate) {
	return h.capacityEstimatesWithProfilesAndReservations(stats, profiles, 0)
}

func (h *DashboardHandler) capacityEstimatesWithProfilesAndReservations(stats *k8s.ClusterStats, profiles resourceprofile.Profiles, reserved int) ([]capacity.Mode, map[string]capacity.Estimate) {
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
	modes := capacity.Modes(profiles)
	if reserved > 0 {
		reservationMode := capacity.VtaOnly
		for _, mode := range modes {
			if mode.Name == capacity.VtaOnly.Name {
				reservationMode = mode
				break
			}
		}
		var ok bool
		freeNodes, freeDisks, ok = capacity.ReserveMode(
			reservationMode, reserved, freeNodes, freeDisks,
			stats.StorageReplicaCount, stats.StorageAvailable,
		)
		if !ok {
			freeNodes, freeDisks = nil, nil
		}
	}
	estimates := make(map[string]capacity.Estimate, len(modes))
	for _, mode := range modes {
		estimates[mode.Name] = capacity.EstimateMode(
			mode, freeNodes, freeDisks, stats.StorageReplicaCount, stats.StorageAvailable,
		)
	}

	return modes, estimates
}

// A prebuilt, waiting Deployment has zero replicas and therefore does not appear in pod
// CPU or memory requests. Reserve one future VTA footprint until the pooled
// session is running; otherwise the Dashboard would offer that same capacity
// to both VTA-only and full-stack sessions.
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
