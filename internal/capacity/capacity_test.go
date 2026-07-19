package capacity

import "testing"

func TestPlanningHeadroom(t *testing.T) {
	tests := []struct {
		name                         string
		allocatable, requested, used int64
		want                         int64
	}{
		{name: "usage higher", allocatable: 7600, requested: 3400, used: 5100, want: 2500},
		{name: "requests higher", allocatable: 4000, requested: 2500, used: 800, want: 1500},
		{name: "floor at zero", allocatable: 1000, requested: 1100, used: 900, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PlanningHeadroom(tt.allocatable, tt.requested, tt.used)
			if got != tt.want {
				t.Fatalf("PlanningHeadroom() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestModeMemoryUsesLimits(t *testing.T) {
	var vtaMem, fullStackMem int64
	for _, comp := range VtaOnly.Components {
		vtaMem += comp.MemBytes
	}
	for _, comp := range FullStack.Components {
		fullStackMem += comp.MemBytes
	}
	if vtaMem != 64*mi {
		t.Fatalf("VtaOnly memory = %d, want 64Mi", vtaMem)
	}
	if fullStackMem != 512*mi {
		t.Fatalf("FullStack memory = %d, want 512Mi", fullStackMem)
	}
}

func TestEstimateModeCountsWholeSessions(t *testing.T) {
	// One node fits exactly 3 vta_only pods by CPU; storage allows more.
	nodes := []NodeFree{{CPUMillis: 30, MemBytes: 10 * gi}}
	disks := []DiskFree{{Bytes: 100 * gi}}

	est := EstimateMode(VtaOnly, nodes, disks, 1, true)
	if est.Count != 3 {
		t.Fatalf("Count = %d, want 3", est.Count)
	}
	if est.LimitingResource != "cpu" {
		t.Fatalf("LimitingResource = %q, want cpu", est.LimitingResource)
	}
}

func TestEstimateModeFragmentation(t *testing.T) {
	// Two nodes with 45m each: the naive total (90m) fits one full_stack
	// (80m), but no single node can hold the 50m mediator pod.
	nodes := []NodeFree{
		{CPUMillis: 45, MemBytes: 10 * gi},
		{CPUMillis: 45, MemBytes: 10 * gi},
	}
	disks := []DiskFree{{Bytes: 100 * gi}}

	est := EstimateMode(FullStack, nodes, disks, 1, true)
	if est.Count != 0 {
		t.Fatalf("Count = %d, want 0 (mediator's 50m fits on no node)", est.Count)
	}
	if est.ByCPU != 1 {
		t.Fatalf("ByCPU = %d, want 1 (naive total ignores fragmentation)", est.ByCPU)
	}
}

func TestEstimateModeStorageBound(t *testing.T) {
	nodes := []NodeFree{{CPUMillis: 10000, MemBytes: 100 * gi}}
	// Each full_stack session requests 1Gi + 3*200Mi = 1624Mi, so 9Gi fits
	// five whole sessions but not six.
	disks := []DiskFree{{Bytes: 9 * gi}}

	est := EstimateMode(FullStack, nodes, disks, 1, true)
	if est.Count != 5 {
		t.Fatalf("Count = %d, want 5", est.Count)
	}
	if est.LimitingResource != "storage" {
		t.Fatalf("LimitingResource = %q, want storage", est.LimitingResource)
	}
}

func TestEstimateModeReplicaAntiAffinity(t *testing.T) {
	nodes := []NodeFree{{CPUMillis: 10000, MemBytes: 100 * gi}}
	// Plenty of space but only one disk: 2 replicas need 2 distinct nodes.
	disks := []DiskFree{{Bytes: 100 * gi}}

	est := EstimateMode(VtaOnly, nodes, disks, 2, true)
	if est.Count != 0 {
		t.Fatalf("Count = %d, want 0 (second replica has no distinct disk)", est.Count)
	}

	disks = []DiskFree{{Bytes: 100 * gi}, {Bytes: 100 * gi}}
	est = EstimateMode(VtaOnly, nodes, disks, 2, true)
	if est.Count == 0 {
		t.Fatal("Count = 0, want > 0 with two disks")
	}
	// Each session consumes 200Mi on each of the two disks.
	if est.ByStorage != 512 {
		t.Fatalf("ByStorage = %d, want 512 (200Gi total / 400Mi per session)", est.ByStorage)
	}
}

func TestEstimateModeStorageUnknown(t *testing.T) {
	nodes := []NodeFree{{CPUMillis: 100, MemBytes: 10 * gi}}

	est := EstimateMode(VtaOnly, nodes, nil, 1, false)
	if est.Count != 10 {
		t.Fatalf("Count = %d, want 10 (storage constraint skipped)", est.Count)
	}
	if est.ByStorage != -1 {
		t.Fatalf("ByStorage = %d, want -1", est.ByStorage)
	}
}
