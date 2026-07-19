// Package capacity estimates how many more VTA sessions the cluster can
// schedule. Estimates are placement simulations, not straight division:
// cluster-wide totals overstate capacity when free resources are fragmented
// across nodes, so each session's components are individually bin-packed onto
// the nodes that can actually hold them.
package capacity

import "slices"

// Component is one pod of a session mode plus its PVC (StorageBytes 0 = no
// volume). Components schedule independently — full_stack's four pods may
// land on four different nodes.
type Component struct {
	Name         string
	CPUMillis    int64
	MemBytes     int64
	StorageBytes int64
}

// Mode is the resource cost of one session of a given setup mode.
type Mode struct {
	Name       string
	Components []Component
}

const (
	mi = int64(1) << 20
	gi = int64(1) << 30
)

// Per-component planning costs mirror the provisioning code — keep in sync with:
//
//	vta_only:   internal/k8s/vta_resources.go   CreateVtaDeployment
//	full_stack: internal/setup/orchestrator_fullstack.go     (dids, mediator, vta)
//	            internal/setup/orchestrator_fullstack_vtc.go (vtc)
//
// and the per-component PVC sizes (internal/k8s/setup_jobs.go and
// internal/setup/orchestrator_fullstack.go).
// CPU uses requests (there are deliberately no CPU limits). Memory uses hard
// limits so a reported session count still fits if every new component grows
// to its configured ceiling.
var (
	VtaOnly = Mode{Name: "vta_only", Components: []Component{
		{Name: "vta", CPUMillis: 10, MemBytes: 64 * mi, StorageBytes: 200 * mi},
	}}

	// FullStack is the full_stack_with_vtc mode — the plain full_stack mode is
	// retired for new sessions, so capacity planning targets the VTC variant.
	FullStack = Mode{Name: "full_stack", Components: []Component{
		{Name: "dids", CPUMillis: 10, MemBytes: 128 * mi, StorageBytes: 200 * mi},
		{Name: "mediator", CPUMillis: 50, MemBytes: 256 * mi, StorageBytes: gi},
		{Name: "vta", CPUMillis: 10, MemBytes: 64 * mi, StorageBytes: 200 * mi},
		{Name: "vtc", CPUMillis: 10, MemBytes: 64 * mi, StorageBytes: 200 * mi},
	}}
)

// PlanningHeadroom returns the resource budget available for new sessions.
// Existing reservations and stable live consumption are both protected. The
// calculation is intentionally per node so placement simulation does not hide
// a hot node behind spare capacity elsewhere in the cluster.
func PlanningHeadroom(allocatable, requested, used int64) int64 {
	if allocatable <= 0 {
		return 0
	}

	occupied := max(requested, used)
	return max(allocatable-occupied, 0)
}

// NodeFree is a schedulable node's CPU/memory planning headroom after stable
// live usage and requests have been deducted.
type NodeFree struct {
	CPUMillis int64
	MemBytes  int64
}

// DiskFree is one Longhorn node's schedulable storage headroom.
type DiskFree struct {
	Bytes int64
}

// Estimate is the result for one mode. Count is the authoritative
// placement-simulated number; ByCPU/ByMemory/ByStorage are naive
// total-headroom divisions kept for display ("what's the bottleneck"), so
// Count can be lower than all three when free space is fragmented.
// ByStorage is -1 when storage stats were unavailable (storage then simply
// doesn't constrain Count).
type Estimate struct {
	Count            int
	ByCPU            int
	ByMemory         int
	ByStorage        int
	LimitingResource string
}

// maxSessions bounds the simulation loop; no realistic farm approaches it.
const maxSessions = 100000

// EstimateMode simulates creating sessions of mode until one no longer fits.
// replicas multiplies each PVC's disk cost; replicas of one volume must land
// on distinct nodes (Longhorn's default hard anti-affinity). storageKnown
// false skips the storage constraint entirely (Longhorn unreachable) rather
// than reporting zero capacity.
func EstimateMode(mode Mode, nodes []NodeFree, disks []DiskFree, replicas int, storageKnown bool) Estimate {
	if replicas < 1 {
		replicas = 1
	}

	nodesLeft := make([]NodeFree, len(nodes))
	copy(nodesLeft, nodes)
	disksLeft := make([]DiskFree, len(disks))
	copy(disksLeft, disks)

	count := 0
	for count < maxSessions && placeSession(mode, nodesLeft, disksLeft, replicas, storageKnown) {
		count++
	}

	est := Estimate{Count: count}
	est.ByCPU, est.ByMemory, est.ByStorage = naiveCounts(mode, nodes, disks, replicas, storageKnown)
	est.LimitingResource = limitingResource(est)
	return est
}

// placeSession tries to fit every component of one session, mutating the
// headroom slices on success. A partial placement is not rolled back: if any
// component fails, the whole estimate loop stops, so the leftover deductions
// are never read again.
func placeSession(mode Mode, nodes []NodeFree, disks []DiskFree, replicas int, storageKnown bool) bool {
	for _, comp := range mode.Components {
		if !placePod(nodes, comp) {
			return false
		}
		if storageKnown && comp.StorageBytes > 0 && !placeVolume(disks, comp.StorageBytes, replicas) {
			return false
		}
	}
	return true
}

// placePod worst-fits the component onto the fitting node with the most free
// memory — spreading keeps large nodes open for later components.
func placePod(nodes []NodeFree, comp Component) bool {
	best := -1
	for i := range nodes {
		if nodes[i].CPUMillis < comp.CPUMillis || nodes[i].MemBytes < comp.MemBytes {
			continue
		}
		if best == -1 || nodes[i].MemBytes > nodes[best].MemBytes {
			best = i
		}
	}
	if best == -1 {
		return false
	}
	nodes[best].CPUMillis -= comp.CPUMillis
	nodes[best].MemBytes -= comp.MemBytes
	return true
}

// placeVolume puts one replica of size bytes on each of `replicas` distinct
// disks, worst-fit. All replicas must fit or the volume doesn't schedule.
func placeVolume(disks []DiskFree, bytes int64, replicas int) bool {
	chosen := make([]int, 0, replicas)
	for range replicas {
		best := -1
		for i := range disks {
			if disks[i].Bytes < bytes || slices.Contains(chosen, i) {
				continue
			}
			if best == -1 || disks[i].Bytes > disks[best].Bytes {
				best = i
			}
		}
		if best == -1 {
			return false
		}
		chosen = append(chosen, best)
	}
	for _, i := range chosen {
		disks[i].Bytes -= bytes
	}
	return true
}

// naiveCounts divides cluster-wide free totals by per-session cost, ignoring
// fragmentation and replica anti-affinity. Display-only.
func naiveCounts(mode Mode, nodes []NodeFree, disks []DiskFree, replicas int, storageKnown bool) (byCPU, byMem, byStorage int) {
	var cpuCost, memCost, storageCost int64
	for _, comp := range mode.Components {
		cpuCost += comp.CPUMillis
		memCost += comp.MemBytes
		storageCost += comp.StorageBytes * int64(replicas)
	}

	var cpuFree, memFree, storageFree int64
	for _, n := range nodes {
		cpuFree += n.CPUMillis
		memFree += n.MemBytes
	}
	for _, d := range disks {
		storageFree += d.Bytes
	}

	byCPU = divCount(cpuFree, cpuCost)
	byMem = divCount(memFree, memCost)
	byStorage = -1
	if storageKnown {
		byStorage = divCount(storageFree, storageCost)
	}
	return byCPU, byMem, byStorage
}

// divCount returns free/cost, treating a zero-cost resource as unconstraining.
func divCount(free, cost int64) int {
	if cost <= 0 {
		return maxSessions
	}
	if free < 0 {
		return 0
	}
	return int(free / cost)
}

// limitingResource names the scarcest resource by naive count (ties: cpu >
// memory > storage). With storage unknown it only compares cpu and memory.
func limitingResource(e Estimate) string {
	name, min := "cpu", e.ByCPU
	if e.ByMemory < min {
		name, min = "memory", e.ByMemory
	}
	if e.ByStorage >= 0 && e.ByStorage < min {
		name = "storage"
	}
	return name
}
