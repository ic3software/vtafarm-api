package handler

import (
	"context"
	"sync"
	"time"

	"github.com/ic3software/vtafarm-api/internal/capacity"
	"github.com/ic3software/vtafarm-api/internal/k8s"
)

// capacityStatsTTL caches one ClusterResourceStats read briefly so many users
// opening the create screen at once don't each list every pod in the cluster.
const capacityStatsTTL = 10 * time.Second

// CapacityService answers "can the cluster still fit one more session of this
// mode?" for the user create flow. It shares the placement simulation
// (internal/capacity) and per-mode costs (capacity.VtaOnly / capacity.FullStack)
// with the admin dashboard, so both agree on what fits.
//
// Unlike the dashboard, which fails closed (a node with no live-usage sample
// contributes zero headroom), this service fails open: when metrics-server or
// Longhorn is unreachable it plans against reservations (allocatable −
// requested) instead of blocking creation. Only a positive "zero fit" result
// ever gates a create.
type CapacityService struct {
	k8s *k8s.Client

	mu       sync.Mutex
	cached   *k8s.ClusterStats
	cachedAt time.Time
}

func NewCapacityService(k8sClient *k8s.Client) *CapacityService {
	return &CapacityService{k8s: k8sClient}
}

// CapacityMeta flags whether live metrics and storage stats were available, so
// callers can tell "measured, and it's full" from "couldn't fully measure".
type CapacityMeta struct {
	MetricsAvailable bool
	StorageAvailable bool
}

// stats returns cluster stats, reusing a recent read within capacityStatsTTL.
func (s *CapacityService) stats(ctx context.Context) (*k8s.ClusterStats, error) {
	s.mu.Lock()
	if s.cached != nil && time.Since(s.cachedAt) < capacityStatsTTL {
		cached := s.cached
		s.mu.Unlock()
		return cached, nil
	}
	s.mu.Unlock()

	fresh, err := s.k8s.ClusterResourceStats(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.cached = fresh
	s.cachedAt = time.Now()
	s.mu.Unlock()
	return fresh, nil
}

// Estimates runs the placement simulation for every creatable mode. determinable
// is false when the cluster couldn't be read at all (no k8s client or the stats
// call failed) — callers should fail open in that case.
func (s *CapacityService) Estimates(ctx context.Context) (est map[string]capacity.Estimate, meta CapacityMeta, determinable bool) {
	if s.k8s == nil {
		return nil, CapacityMeta{}, false
	}
	stats, err := s.stats(ctx)
	if err != nil {
		return nil, CapacityMeta{}, false
	}

	return estimatesFrom(stats), CapacityMeta{
		MetricsAvailable: stats.MetricsAvailable,
		StorageAvailable: stats.StorageAvailable,
	}, true
}

// estimatesFrom runs the placement simulation for every creatable mode against
// one stats snapshot. Split out from Estimates so it can be unit-tested with a
// synthetic ClusterStats, no cluster required.
func estimatesFrom(stats *k8s.ClusterStats) map[string]capacity.Estimate {
	nodes, disks := freeResources(stats)
	out := make(map[string]capacity.Estimate, 2)
	for _, mode := range []capacity.Mode{capacity.VtaOnly, capacity.FullStack} {
		out[mode.Name] = capacity.EstimateMode(mode, nodes, disks, stats.StorageReplicaCount, stats.StorageAvailable)
	}
	return out
}

// ModeFits reports whether one more session of mode still fits. determinable is
// false when the cluster couldn't be read — callers fail open on that.
func (s *CapacityService) ModeFits(ctx context.Context, mode capacity.Mode) (fits, determinable bool) {
	est, _, ok := s.Estimates(ctx)
	if !ok {
		return false, false
	}
	return est[mode.Name].Count >= 1, true
}

// freeResources turns cluster stats into per-node/per-disk planning headroom.
// Fail-open: when live usage is unknown (metrics-server down) it plans against
// reservations only (allocatable − requested) rather than zeroing the node out.
func freeResources(stats *k8s.ClusterStats) ([]capacity.NodeFree, []capacity.DiskFree) {
	var nodes []capacity.NodeFree
	for _, n := range stats.Nodes {
		if !n.Schedulable {
			continue
		}
		var cpuUsed, memUsed int64
		if stats.MetricsAvailable {
			cpuUsed, memUsed = n.CPUUsedMillis, n.MemUsedBytes
		}
		nodes = append(nodes, capacity.NodeFree{
			CPUMillis: capacity.PlanningHeadroom(n.CPUAllocatableMillis, n.CPURequestedMillis, cpuUsed),
			MemBytes:  capacity.PlanningHeadroom(n.MemAllocatableBytes, n.MemRequestedBytes, memUsed),
		})
	}

	var disks []capacity.DiskFree
	for _, sn := range stats.StorageNodes {
		if !sn.Schedulable {
			continue
		}
		disks = append(disks, capacity.DiskFree{Bytes: sn.SchedulableBytes})
	}
	return nodes, disks
}
