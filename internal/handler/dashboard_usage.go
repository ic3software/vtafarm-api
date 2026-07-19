package handler

import (
	"sync"
	"time"

	"github.com/ic3software/vtafarm-api/internal/k8s"
)

const capacityUsageWindow = 15 * time.Minute

type nodeUsage struct {
	CPUMillis int64
	MemBytes  int64
}

type nodeUsageSample struct {
	At time.Time
	nodeUsage
}

// usageHighWater keeps a short, in-memory usage history for capacity planning.
// The admin page polls every 15 seconds, so this prevents a single low sample
// from immediately inflating the reported session count. Live dashboard meters
// still show the latest metrics-server reading.
type usageHighWater struct {
	mu     sync.Mutex
	byNode map[string][]nodeUsageSample
	window time.Duration
}

func newUsageHighWater(window time.Duration) *usageHighWater {
	return &usageHighWater{
		byNode: make(map[string][]nodeUsageSample),
		window: window,
	}
}

// Observe records live readings when available and returns each node's maximum
// CPU and memory sample inside the window. Cached samples can bridge a brief
// metrics-server failure; a node with no sample is omitted so callers can fail
// closed instead of reporting optimistic capacity.
func (h *usageHighWater) Observe(now time.Time, nodes []k8s.NodeResourceStat, liveAvailable bool) map[string]nodeUsage {
	h.mu.Lock()
	defer h.mu.Unlock()

	cutoff := now.Add(-h.window)
	for name, samples := range h.byNode {
		keep := samples[:0]
		for _, sample := range samples {
			if !sample.At.Before(cutoff) {
				keep = append(keep, sample)
			}
		}
		if len(keep) == 0 {
			delete(h.byNode, name)
			continue
		}
		h.byNode[name] = keep
	}

	if liveAvailable {
		for _, node := range nodes {
			h.byNode[node.Name] = append(h.byNode[node.Name], nodeUsageSample{
				At: now,
				nodeUsage: nodeUsage{
					CPUMillis: node.CPUUsedMillis,
					MemBytes:  node.MemUsedBytes,
				},
			})
		}
	}

	out := make(map[string]nodeUsage, len(h.byNode))
	for name, samples := range h.byNode {
		var high nodeUsage
		for _, sample := range samples {
			high.CPUMillis = max(high.CPUMillis, sample.CPUMillis)
			high.MemBytes = max(high.MemBytes, sample.MemBytes)
		}
		out[name] = high
	}
	return out
}
