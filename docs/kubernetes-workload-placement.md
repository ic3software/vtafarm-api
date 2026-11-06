# Kubernetes workload placement

## Why this is needed

Kubernetes schedules a Pod when the Pod is created. It does not automatically
move a healthy Pod merely because a node that was drained has returned or
because another node has become less busy. After sequential node maintenance,
VTA Farm can therefore remain heavily skewed even though every node is Ready.

This policy is intended to:

- spread tenant workloads across nodes without making provisioning depend on a
  perfectly even cluster;
- keep system and stateful infrastructure out of routine rebalancing;
- make maintenance recovery predictable for workloads backed by Longhorn RWO
  volumes; and
- provide a bounded manual procedure until automated rebalancing is proven.

It is not intended to balance Longhorn replicas. Pod placement and replica
placement are related operationally, but they are separate scheduling systems.

## Production gap observed on 2026-11-02

After a three-node production maintenance rollout, most tenant workloads were
recreated while only a subset of the nodes was schedulable. When all three
nodes returned, the distribution remained skewed:

| Node | Running Pods | VTA-only Pods | Live memory | Memory requests |
| --- | ---: | ---: | ---: | ---: |
| server-1 | 74 | 30 | 99% | 89% |
| server-2 | 26 | 3 | 76% | 68% |
| server-3 | 14 | 0 | 61% | 44% |

The generated VTA Deployment had no placement policy:

```yaml
nodeSelector: null
affinity: null
topologySpreadConstraints: null
tolerations: null
```

Deleting a Pod is therefore not a reliable rebalance mechanism. The default
scheduler may consider requested resources when scoring feasible nodes, but it
does not promise to choose the node with the lowest live memory usage, and it
does not continuously move healthy Pods after a drained node returns. A
replacement can consequently land on the same overloaded node.

This is compounded by the current VTA request of `10m` CPU and `16Mi` memory.
The scheduler sees that declared request rather than the full observed runtime
cost, so an overloaded node can still appear capable of accepting another VTA.
Placement policy and realistic requests are both required; neither one replaces
the other.

## Common placement labels

Add stable labels to every tenant workload Deployment and Pod template. Do not
change existing Deployment selectors because selector fields are immutable.

```yaml
metadata:
  labels:
    vtafarm.ic3.dev/workload-class: tenant
    vtafarm.ic3.dev/mode: vta-only # or full-stack
    vtafarm.ic3.dev/component: vta # mediator, dids, vtc, trust-registry, etc.
```

The `auto-rebalance` label is not a general tenant label. Apply it to the
Deployment and Pod template only for VTA-only workloads that have passed the
controlled relocation test:

```yaml
metadata:
  labels:
    vtafarm.ic3.dev/auto-rebalance: "true"
```

Do not apply it to Vault, the VTA Farm API, databases, Rancher, Longhorn,
Kubernetes system workloads, or Full Stack components during the initial
rollout.

The relevant creation paths are `internal/k8s/vta_resources.go` and
`internal/k8s/component_resources.go`. The Full Stack orchestrators should
pass the mode and component values through those shared helpers.

## Recommended scheduling policy

### Live memory is the primary placement signal

Pod count is not a capacity signal. A node with few VTA Pods can still be full
because Vault, the VTA Farm API, Longhorn, the operating system, or other
infrastructure consumes its memory. Conversely, a node with many small tenant
Pods can still have safe headroom.

Each node reserves 2 GiB for the operating system and Kubernetes services. That
reservation must be reflected in `Node.status.allocatable.memory`, not only in
a dashboard calculation. The existing Health and Workload monitoring should
measure workload memory against that post-reservation Allocatable budget.

A controller should publish one of these node labels from the monitored memory
state:

| State | Allocatable memory used | Placement meaning |
| --- | ---: | --- |
| `green` | below 70% | preferred |
| `yellow` | 70% to below 85% | acceptable |
| `orange` | 85% to below 90% | last healthy choice |
| `hot` | 90% or above | no new tenant workload |

```yaml
metadata:
  labels:
    vtafarm.ic3.dev/memory-state: green
    vtafarm.ic3.dev/vta-eligible: "true"
```

The percentage boundaries are initial policy values, not constants embedded in
multiple call sites. Keep them in one configuration source and validate them
against the production Health and Workload metrics.

A node enters `hot` only after remaining at or above 90% for five minutes.
It becomes eligible again only after remaining at or below 85% for ten minutes.
This hysteresis prevents Pods from moving repeatedly when memory oscillates
near 90%.

The controller should set `vta-eligible=true` only when metrics are fresh and
the node can accept one additional p95 VTA workload while remaining below 90%.
Missing or stale metrics fail closed: remove eligibility, alert, and do not
place a new VTA on that node.

Eligibility is not a reservation. Two concurrent provisioning requests could
otherwise consume the same reported headroom before metrics update. The first
rollout must allow only one unscheduled VTA placement at a time and refresh
eligibility before admitting the next request. A later dedicated scheduler or
controller must reserve the selected node's p95 VTA budget atomically and
release that reservation when the Pod becomes Ready or provisioning fails.

### Exclude hot nodes and prefer headroom

VTA-only Pods must require an eligible node. Among eligible nodes, they should
prefer the node with the safer memory band:

```yaml
spec:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: vtafarm.ic3.dev/vta-eligible
                operator: In
                values: ["true"]
              - key: vtafarm.ic3.dev/memory-state
                operator: In
                values: [green, yellow, orange]
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          preference:
            matchExpressions:
              - key: vtafarm.ic3.dev/memory-state
                operator: In
                values: [green]
        - weight: 50
          preference:
            matchExpressions:
              - key: vtafarm.ic3.dev/memory-state
                operator: In
                values: [yellow]
        - weight: 10
          preference:
            matchExpressions:
              - key: vtafarm.ic3.dev/memory-state
                operator: In
                values: [orange]
```

The required rule is the safety boundary: a `hot` node cannot receive a new
VTA even if it currently has the fewest tenant Pods. Preferred weights bias the
choice among eligible nodes, but the default scheduler combines them with its
other scoring plugins. A strict "always choose the lowest live-memory node"
policy requires a dedicated scheduler profile or controller and must not be
claimed from preferred affinity alone.

Updating a node label does not move an existing healthy Pod. It controls new
placement and gives the rebalancer an enforceable way to prevent a replacement
from returning to a hot node.

If every node is `hot` or ineligible, admission should reject or queue new
sessions before creating a Pending Pod. Rebalancing cannot create memory
capacity; the required response is a cluster-capacity alert and an operator
decision to add memory, add a node, or stop workloads.

Full Stack components should use the same `memory-state` exclusion only after
their own p95 memory profiles and eligibility calculation are defined. Do not
reuse the VTA p95 estimate for VTC, mediator, DIDS, or trust registry.

### Secondary topology signals

Use a soft hostname spread rule after the memory gate is in place:

```yaml
spec:
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: kubernetes.io/hostname
      whenUnsatisfiable: ScheduleAnyway
      labelSelector:
        matchLabels:
          vtafarm.ic3.dev/workload-class: tenant
```

`ScheduleAnyway` is deliberate. Topology spread is a secondary signal and
must not turn a temporary maintenance condition into a provisioning outage.

Do not enable the proposed farm-wide `podAntiAffinity` with a hard-coded
`weight: 100` in the first rollout. It counts matching Pods, not their memory
cost, and a Full Stack session contributes several Pods while a VTA-only
session contributes one. Evaluate cross-namespace anti-affinity later as a
staging-calibrated tie-breaker. It must never replace the live-memory eligibility
gate or accurate resource requests.

### Implementation requirements

- Drive node memory state from the existing Health and Workload monitoring,
  using the post-reservation Allocatable budget as the denominator.
- Keep the 70%, 85%, and 90% thresholds, observation windows, and p95 VTA
  estimate in one reviewed configuration source.
- Remove VTA eligibility when metrics are stale or the projected p95 VTA would
  take the node to 90% or above.
- Serialize initial VTA placement, then replace serialization with atomic
  per-node p95 reservations before allowing concurrent provisioning.
- Reject or queue new sessions when no eligible node exists; do not silently
  bypass the memory gate.
- Generate the same stable Pod labels through the shared VTA and component
  Deployment helpers without changing immutable Deployment selectors.
- Apply `auto-rebalance=true` only to the explicitly approved VTA-only scope.
- Calibrate CPU and memory requests from observed p50, p95, and peak usage.
  Scheduler requests remain a safety input even when live metrics drive the
  memory-state labels.
- Reconcile existing Deployments explicitly; changing only the creation path
  leaves current sessions without the new labels and placement policy.

## RWO volume update strategy

Long-running, single-replica Deployments that mount a Longhorn ReadWriteOnce
PVC should use the `Recreate` strategy:

```yaml
spec:
  strategy:
    type: Recreate
```

This prevents a template rollout from intentionally starting a replacement Pod
while the old Pod still owns the volume. It trades availability for an explicit
single-writer handoff: every rollout has a short outage, and a failed new image
requires a manual or controller-driven rollback.

Do not apply `Recreate` to stateless Deployments merely for consistency.
Retain `RollingUpdate` where old and new replicas can safely overlap.

`Recreate` governs Deployment template rollouts; it does not make an ordinary
Pod deletion or Descheduler eviction Longhorn-aware. A controlled relocation
must scale the Deployment to zero, wait for the old Pod to terminate and the
volume to detach cleanly, and only then scale the Deployment back to one.

Before a rollout, record the current image digest and Deployment revision.
Rollback recreates the previous version; it does not resurrect the terminated
Pod, restore external ConfigMap or Secret contents, or undo an incompatible
on-volume data migration.

## Applying the policy to existing workloads

The current creation helpers treat an existing Deployment as success. Adding
placement fields to newly generated objects therefore affects new sessions but
does not update existing Deployments.

Implement an explicit reconciliation path that:

1. lists VTA Farm-managed Deployments by stable label;
2. reports which workloads qualify for the VTA-only memory gate and controlled
   rebalancing;
3. patches only Pod-template labels, affinity, topology spread, and the
   Deployment strategy;
4. preserves selectors, images, environment variables, PVC references, and
   user-specific settings;
5. supports a dry-run report before mutation; and
6. rolls out in small batches with readiness and Longhorn health gates.

Changing only the Deployment strategy does not restart a Pod. Any Pod-template
change does, so label and affinity reconciliation must follow the same RWO
handoff and rollback gates as an image rollout.

## Memory-driven rebalancing

Rebalancing is threshold recovery, not continuous optimization. Do not move
healthy Pods merely to make node percentages equal while every node remains
below 90%. New placements should consume safer headroom and gradually improve
the distribution without disruption.

When any node remains `hot` for five minutes:

1. mark it ineligible for new VTA placement and emit a node-hot alert;
2. confirm that metrics are fresh, every node is Ready, Vault is available,
   Kubernetes API and etcd latency are healthy, and all affected Longhorn
   volumes are healthy;
3. identify another eligible node where the selected VTA's p95 memory would
   remain below 90%;
4. select exactly one healthy VTA-only Deployment carrying
   `vtafarm.ic3.dev/auto-rebalance=true`;
5. scale that Deployment to zero and wait until its Pod is gone;
6. wait for its Longhorn volume to be healthy and fully detached;
7. scale the Deployment back to one; required node affinity prevents the
   replacement from returning to the hot source;
8. wait for the replacement to become Ready, verify its application health and
   volume attachment, and observe both nodes through a cooldown window; and
9. repeat with one more VTA only if the source remains hot and a safe
   destination still exists.

Stop when the source remains at or below 85% for ten minutes. Stop immediately
if a replacement remains Pending, a volume is degraded or rebuilding, a node
reports MemoryPressure, API or etcd requests time out, Vault is unavailable, or
a dependency enters CrashLoopBackOff.

Never evict Vault, the VTA Farm API, databases, Rancher, Longhorn, Kubernetes
system workloads, or an unlabeled tenant workload through this policy. Do not
automatically rebalance Full Stack components in the first rollout.

If all nodes are hot, or no destination can accept the VTA while remaining
below 90%, do not evict anything. Emit a cluster-capacity-exhausted alert,
reject or queue new sessions, and require an operator capacity decision.
Moving a Pod between full nodes only transfers the failure risk.

## Manual fallback runbook

Use the automated policy in reporting mode first. Until controlled relocation
has passed staging, an operator should execute the same gates manually:

1. record node readiness, live memory against Allocatable, Pod placement,
   requests, and Longhorn volume health;
2. choose one approved VTA-only Deployment from the hot node;
3. confirm at least one eligible destination remains below 90% after adding the
   VTA's p95 memory;
4. scale the Deployment to zero instead of deleting its PVC, PV, namespace, or
   Longhorn volume;
5. wait for the old Pod to disappear and the volume to detach cleanly;
6. scale the Deployment back to one and wait for the replacement to become
   Ready on an eligible node;
7. verify VTA health, volume health, node memory, Vault, API, and etcd; and
8. stop for a cooldown period before considering another VTA.

Do not cordon nodes solely for memory rebalancing. The staging Longhorn drain
policy begins moving last replicas when a node is cordoned, which would combine
Pod relocation and storage relocation in the same operation.

## Controlled automation rollout

A generic Descheduler may use actual memory metrics to identify an overloaded
node, but it does not provide the required Longhorn detach/attach handshake or
guarantee the replacement's live-memory destination. It may be used for
reporting and comparison, not as the first production eviction mechanism.

Roll out the VTA-specific controller in stages:

1. **Report only:** publish node states, eligibility, the proposed VTA, projected
   destination utilization, and every reason that blocks a move.
2. **Operator approval:** execute one relocation only after explicit approval.
3. **Staging automatic:** relocate one VTA per cycle with all health gates,
   cooldown, and rollback behavior enabled.
4. **Production limited automatic:** retain one-at-a-time relocation, per-hour
   and per-day limits, complete audit events, and an immediate kill switch.

## Acceptance criteria

- The 2 GiB node reservation is visible in
  `Node.status.allocatable.memory`, and monitoring uses the same workload
  budget.
- Tests cover the `green`, `yellow`, `orange`, and `hot` boundaries,
  five-minute hot observation, ten-minute recovery, and stale metrics.
- A hot or ineligible node cannot receive a new VTA.
- Green, yellow, and orange preferences are generated exactly as configured,
  without claiming a strict lowest-memory choice from scheduler preferences.
- Admission rejects or queues new sessions when all nodes are hot or
  ineligible.
- Concurrent provisioning cannot reserve the same node headroom twice; the
  initial implementation proves serialization and the later implementation
  proves atomic reservation and release.
- New tenant Pods carry the common placement labels; only approved VTA-only
  Pods carry `auto-rebalance=true`.
- Generated VTA-only Deployments contain the required memory gate and soft
  topology spread policy.
- Existing Deployment selectors are unchanged.
- Single-replica RWO Deployments use `Recreate`; stateless Deployments retain
  `RollingUpdate`.
- A staging relocation scales one VTA to zero, observes clean volume detach,
  recreates it on an eligible node, and completes with the replacement Ready
  and the volume healthy.
- No infrastructure or unlabeled workload is eligible for automated eviction.
- Rebalancing stops at the recovery threshold and performs no eviction when all
  nodes are hot or no safe destination exists.
- Capacity and Admin Panel calculations use Kubernetes Allocatable, requests,
  and the same live-memory metric consistently.
