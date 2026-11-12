# Kubernetes workload placement

## Why this is needed

Kubernetes schedules a Pod when the Pod is created. It does not automatically
move a healthy Pod merely because a node that was drained has returned or
because another node has become less busy. After sequential node maintenance,
VTA Farm can therefore remain heavily skewed even though every node is Ready.

This policy is intended to:

- give an administrator a bounded manual correction when tenant workloads have
  become materially unbalanced;
- keep system and stateful infrastructure out of routine rebalancing;
- make maintenance recovery predictable for workloads backed by Longhorn RWO
  volumes;
- let an administrator inspect cluster load and move a selected VTA to an
  exact eligible node from the Admin Panel; and
- return scheduling control to Kubernetes immediately after that selected move
  completes.

It is not intended to balance Longhorn replicas. Pod placement and replica
placement are related operationally, but they are separate scheduling systems.
It is also not a continuous rebalancer, an alternative scheduler, or durable
node pinning. Normal imbalance is observed; substantial imbalance may produce
an administrator-facing recommendation; only an administrator's explicit
exact-node relocation may change a workload's placement.

## Scheduling ownership invariant

The Kubernetes default scheduler is the sole placement authority unless an
administrator explicitly submits an exact-node relocation for a selected
VTA-only workload. This is a product invariant, not an implementation detail.

An approved relocation grants VTA Farm one narrow, one-shot authority: place
the replacement Pod created for that relocation on the administrator-selected
eligible node. That authority ends when the relocation reaches a terminal
state. It must not become sticky Deployment desired state and must not carry
forward to any later Pod replacement.

Consequently, initial provisioning, image upgrades and rollbacks, resource or
configuration updates, ordinary restarts, node-failure recovery, maintenance
recovery, and every other non-relocation workflow must leave destination choice
to the Kubernetes default scheduler. These workflows must not create or retain
a placement scheduling gate, exact-hostname constraint, relocation reservation,
or automatic placement plan merely because the workload was relocated before.

Recommendations, node health labels, topology spread, and placement previews
may inform an administrator or the default scheduler, but they do not authorize
VTA Farm to choose a node. Reconciliation may normalize safe workload metadata
and strategy; it is not relocation and must not silently acquire relocation
authority.

The purpose of relocation is therefore manual correction, not lasting
optimization. When observed distribution becomes materially unbalanced, an
administrator may choose a bounded set of corrective moves. VTA Farm executes
only those reviewed mappings and then becomes observational again. It must not
use the completed correction as permission to manage later scheduling.

### Known implementation violation observed on 2026-11-12

Release `v0.20.0` persists the relocation scheduling gate and placement affinity
in the VTA Deployment template. Although the worker removes the gate from the
reviewed Pod and releases its reservation and maintenance lock, a later image
upgrade inherits the template gate. The worker then creates an automatic
`provision` placement plan for that unrelated replacement.

That behavior violates the scheduling ownership invariant above. A completed
relocation must not grant authority over a later image rollout or any other Pod
replacement. The implementation and its tests must be changed to make the gate
one-shot; this document must not be weakened to preserve the current behavior.

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
Deployment and Pod template only for VTA-only workloads that are allowed to use
the managed relocation workflow:

```yaml
metadata:
  labels:
    vtafarm.ic3.dev/auto-rebalance: "true"
```

Despite its name, this label marks the permitted relocation scope; it does not
authorize an unattended move by itself. Do not apply it to Vault, the VTA Farm
API, databases, Rancher, Longhorn, Kubernetes system workloads, or Full Stack
components.

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
Missing or stale metrics fail closed for reviewed relocations: remove
eligibility, alert, and do not select that node as a relocation destination.
Initial VTA Pods do not use this label as a scheduling requirement.

Eligibility is not a reservation. Two concurrent relocations could otherwise
consume the same reported headroom before metrics update. Every reviewed
relocation must atomically reserve the selected node's p95 VTA budget before
releasing its Pod to the scheduler. Include active reservations in projected
node utilization, and release them when the Pod becomes Ready, the placement
fails, or recovery proves that the Pod no longer exists.

### Exclude hot relocation targets and prefer headroom

Pods created by a reviewed relocation must require an eligible node. Among
eligible relocation targets, they should prefer the node with the safer memory
band:

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

The required rule is the relocation safety boundary: a `hot` node cannot be an
approved destination even if it currently has the fewest tenant Pods.
Preferred weights do not guarantee the destination. The placement controller
described below tightens a gated Pod's required node affinity when an exact
node has been selected.

Updating a node label does not move an existing healthy Pod. It gives the
reviewed relocation workflow an enforceable destination policy.

If every node is `hot` or ineligible, reviewed relocation must stop because it
cannot create memory capacity. Initial VTA Pods still use the default scheduler
and their declared resource requests; capacity admission and alerts remain a
separate concern.

Full Stack components should use the same `memory-state` exclusion only after
their own p95 memory profiles and eligibility calculation are defined. Do not
reuse the VTA p95 estimate for VTC, mediator, DIDS, or trust registry.

### Gate reviewed VTA relocation only

Initial VTA-only Pods are created without a scheduling gate or placement node
affinity. The default Kubernetes scheduler places them immediately using their
resource requests and the soft tenant topology spread rule.

After an administrator approves relocation of an existing VTA, its replacement
Pod is created with a Kubernetes scheduling gate:

```yaml
spec:
  schedulingGates:
    - name: vtafarm.ic3.dev/placement-ready
```

[Pod scheduling readiness](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-scheduling-readiness/)
keeps that replacement out of the scheduler queue while VTA Farm reserves the
reviewed destination. While a Pod is gated, Kubernetes permits a controller to
tighten its node selector or required node affinity. The placement controller
must:

1. identify the VTA from stable workload labels and the owning Deployment;
2. load the exact destination recorded in the administrator-reviewed
   relocation; a system recommendation remains advisory until the administrator
   explicitly approves that source-to-destination mapping;
3. lock the placement records, recalculate live and reserved headroom, and
   atomically reserve the VTA's p95 memory on the destination;
4. revalidate node readiness, taints, MemoryPressure, metric freshness, Vault,
   Kubernetes API and etcd health, and Longhorn volume health;
5. add a required `kubernetes.io/hostname` match for the exact destination to
   the gated Pod without changing the Deployment template;
6. remove `vtafarm.ic3.dev/placement-ready`; and
7. wait for the default Kubernetes scheduler, volume attachment, and Pod
   readiness before committing the placement and releasing the reservation.

Both the per-Pod hostname constraint and the scheduling gate are one-shot
relocation controls. They must not remain in the Deployment's durable desired
state after the reviewed replacement is created. Completing, failing,
cancelling, or rolling back the relocation revokes the relocation authority.
Any later Pod replacement must be gate-free and must return destination choice
to the Kubernetes default scheduler.

If validation fails, leave the Pod gated, release any unusable reservation,
and expose the blocking reason. Do not remove the gate merely to make the Pod
start somewhere. If a node becomes ineligible after the gate is removed but
before the Pod is bound, delete the still-unbound Pod and let the Deployment
create a newly gated replacement.

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

Do not enable farm-wide `podAntiAffinity` with a hard-coded `weight: 100`. It
counts matching Pods, not their memory cost, and a Full Stack session
contributes several Pods while a VTA-only session contributes one. Add it only
if measured scheduling results prove that it improves choices as a low-weight
tie-breaker. It must never replace the live-memory eligibility gate, atomic
reservations, or accurate resource requests.

### Implementation requirements

- Drive node memory state from the existing Health and Workload monitoring,
  using the post-reservation Allocatable budget as the denominator.
- Keep the 70%, 85%, and 90% thresholds, observation windows, and p95 VTA
  estimate in one reviewed configuration source.
- Remove VTA eligibility when metrics are stale or the projected p95 VTA would
  take the node to 90% or above.
- Gate only replacement Pods created by an approved relocation and use atomic
  per-node p95 reservations for those relocations.
- Persist placement and relocation state so an API restart resumes an
  interrupted scale-down, detach, scheduling, attach, or readiness wait.
- Let initial sessions and every non-relocation replacement use the default
  scheduler without waiting for placement metrics or a placement-worker
  reservation.
- Scope every scheduling gate to an active, administrator-reviewed relocation
  task. A gated Pod without that task is an invariant violation to surface and
  stop, not a request to create an automatic provisioning or placement plan.
- Ensure image upgrades and rollbacks, resource or configuration updates,
  restarts, and recovery never inherit relocation-only gates, affinity, exact
  hostnames, or reservations from an earlier relocation.
- Generate the same stable Pod labels through the shared VTA and component
  Deployment helpers without changing immutable Deployment selectors.
- Apply `auto-rebalance=true` only to the explicitly approved VTA-only scope.
- Calibrate CPU and memory requests from observed p50, p95, and peak usage.
  Scheduler requests remain a safety input even when live metrics drive the
  memory-state labels.
- Reconcile existing Deployments explicitly; changing only the creation path
  leaves current sessions without the new labels and placement policy.
- Expose node load, reservations, VTA placement, eligible destinations,
  projected utilization, workflow progress, and blocking reasons in the Admin
  Panel.

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

## Data preservation invariants

Relocation changes Pod placement; it must never migrate, replace, restore,
delete, or recreate VTA data. The PVC, PV, Longhorn volume, and Vault secret
paths remain the same before and after a successful move.

The default Longhorn StorageClass uses `Delete` reclaim policy, and the current
platform may use a single Longhorn replica. Deleting a VTA PVC can therefore
delete the underlying volume, while loss of the only healthy replica can leave
no in-cluster copy. These are the primary data-loss risks; adding labels or
changing a Deployment strategy is not.

Enforce these invariants in code and RBAC:

- run the placement and relocation worker under a dedicated ServiceAccount
  that can read PVCs, PVs, Kubernetes and Longhorn VolumeAttachments, and
  Longhorn volumes, but cannot delete or create PVCs, PVs, Longhorn volumes,
  namespaces, or Secrets; do not provide this worker with Vault mutation
  credentials;
- keep the existing user-authorized session deletion path separate from every
  placement and reconciliation path;
- take a durable per-session maintenance lock so deletion, image upgrade,
  configuration update, resource update, reconciliation, and relocation cannot
  modify the same Deployment or its storage concurrently;
- before disruption, persist the namespace, Deployment UID, PVC name and UID,
  PV name and UID, CSI volume handle, Longhorn volume name, Vault path
  identifiers, source node, and desired replica count;
- after attachment, prove that the same PVC UID, PV UID, CSI volume handle, and
  Longhorn volume are still in use;
- never treat a missing or mismatched storage identity as a reason to create an
  empty replacement PVC; stop and require recovery instead;
- never force-delete a Pod, remove PVC-protection finalizers, or manually
  remove an active Longhorn attachment ticket; and
- test that reconciliation and relocation issue no delete or create operation
  against PVCs, PVs, Longhorn volumes, namespaces, Secrets, or Vault data.

Wait for both the Kubernetes `VolumeAttachment` and Longhorn attachment ticket
to disappear, and for the Longhorn volume to report healthy and detached,
before creating the replacement Pod. The
[Longhorn attachment workflow](https://longhorn.io/docs/1.12.1/advanced-resources/volumeattachment/)
is asynchronous; scaling a Deployment to zero is not proof that detach has
completed.

Longhorn snapshots live with the volume replicas and are not sufficient for
this migration gate. A Longhorn backup is stored outside the cluster. Before
reconciling existing workloads in production:

1. enable and verify the off-cluster Longhorn backup target;
2. confirm a successful recent backup for Vault and every VTA volume;
3. gracefully stop the canary VTA and wait for its filesystem to unmount;
4. create an explicit pre-migration Longhorn backup and wait for state
   `Completed`;
5. restore that backup to a separate test volume and verify application data;
   and
6. retain the backup URL and storage identity with the reconciliation audit
   record until the migration has passed its observation window.

Longhorn is crash-consistent, and unmounting flushes operating-system caches;
the application must still be verified to handle normal SIGTERM shutdown
without corrupting its store. Do not use forced termination to meet a rollout
deadline. The platform's tested backup and restore procedure remains
`vtafarm-k8s/docs/backup-restore.md`.

## Applying the policy to existing workloads

The current creation helpers treat an existing Deployment as success. Adding
placement fields to newly generated objects therefore affects new sessions but
does not update existing Deployments.

Implement one explicit reconciliation operation that discovers both legacy and
new workloads. Existing objects do not yet have the stable labels, so discovery
must use the managed namespace prefix plus the current `app` and `session-id`
labels, then verify every candidate against the VTA Farm session record.

The Admin Panel must preview the complete reconciliation plan and allow an
administrator to apply it to all managed workloads in one operation. For every
Deployment, the operation must:

1. record the original Deployment metadata, Pod template, strategy, replica
   count, image digest, and revision for audit and rollback;
2. classify mode and component, and report whether the workload qualifies for
   VTA-only managed placement;
3. preserve selectors, images, environment variables, PVC references, Secrets,
   ConfigMaps, and user-specific settings;
4. scale an RWO workload to zero, wait for its Pod to terminate, and verify that
   the Longhorn volume is healthy and detached;
5. patch only Deployment metadata labels, Pod-template labels, topology spread,
   and Deployment strategy, without adding relocation-only scheduling gates,
   affinity, or exact-hostname constraints;
6. restore the desired replica count and let the Kubernetes default scheduler
   place the replacement Pod; and
7. wait for volume attachment, Pod readiness, and application health before
   marking that workload complete.

"Apply all" is one administrator operation and one durable reconciliation job,
not an untracked collection of patches. The backend may sequence RWO handoffs
to avoid an attach/detach storm, but it must show the state and outcome of every
workload. Restarting the API must resume unfinished entries. A failed entry must
retain its original snapshot and expose retry and rollback actions without
silently undoing successful entries.

Changing only the Deployment strategy does not restart a Pod. Any Pod-template
change does, so label and topology reconciliation must use the same RWO handoff
and rollback guarantees as an image rollout. Reconciliation must not use that
restart as an implicit exact-node relocation.

## Admin-controlled load management

The Admin Panel is the control surface for placement, not merely a report. It
must show these values together for every node:

- Ready, schedulable, taint, and MemoryPressure state;
- Allocatable, requested, live, and atomically reserved CPU and memory;
- `memory-state`, VTA eligibility, tenant Pod count, and VTA-only Pod count; and
- the age and freshness of the metrics used for the calculation.

For every VTA-only workload it must show the current node, resource requests
and limits, live usage, p95 placement cost, PVC and Longhorn health, eligible
destinations, and projected destination utilization. An administrator can
build a placement plan containing one or more exact mappings:

```text
VTA 12 -> server-2
VTA 18 -> server-3
VTA 25 -> server-2
```

The preview must reject a plan whose combined reservations would take a
destination to 90% or above. Submitting a plan creates durable relocation
records and capacity reservations in one transaction. The backend may execute
the entries sequentially, but the administrator submits and observes one plan.
Each entry must be idempotent and have a complete audit trail containing the
requester, source, destination, projected utilization, timestamps, state
transitions, and final result.

Node `memory-state` and eligibility are calculated from cluster state and must
not be directly editable in the Admin Panel. Administrators control placement
and resource requests or limits; they do not override live metrics by manually
marking a full node green.

Recommended placement is corrective rather than a general rebalance. Keep a
scheduled workload on its current node while that node remains schedulable,
has fresh metrics, and stays below the high threshold. Recommend a relocation
for an unscheduled workload or a source node in the high or hot band; routine
balancing between healthy nodes requires an explicit administrator-selected
placement.

## Exact-node relocation workflow

For each approved VTA-only relocation:

1. lock the relocation and destination reservation, then revalidate fresh
   metrics and all health gates immediately before disruption;
2. scale the Deployment to zero and wait until its Pod is gone;
3. wait until the Longhorn volume is healthy and fully detached;
4. scale the Deployment back to one so it creates a gated Pod;
5. verify that the gated Pod belongs to the expected Deployment revision;
6. tighten that Pod's required node affinity to the reserved
   `kubernetes.io/hostname` and remove the scheduling gate;
7. wait for the default scheduler to bind it to the exact destination;
8. wait for volume attachment, Pod readiness, and VTA application health;
9. prove that durable Deployment desired state contains no relocation-only
   gate, affinity, or exact-hostname constraint, without causing another Pod
   replacement;
10. commit the new placement, release the reservation, and observe source and
    destination through a cooldown window; and
11. continue with the next entry in the administrator's placement plan.

Stop the plan before the next entry if the destination becomes ineligible, the
replacement remains Pending, a volume is degraded or rebuilding, any node
reports MemoryPressure, API or etcd requests time out, Vault is unavailable, or
a dependency enters CrashLoopBackOff. Preserve completed entries and expose
retry, retarget, and rollback actions for the failed entry.

The workflow state must distinguish at least `requested`, `validating`,
`reserved`, `scaling_down`, `waiting_for_pod_termination`,
`waiting_for_volume_detach`, `creating_gated_pod`, `assigning_target_node`,
`waiting_for_volume_attach`, `waiting_for_ready`, `completed`, `failed`, and
`rolling_back`. Recovery after an API restart must inspect Kubernetes and
Longhorn before advancing; it must never assume that the last requested action
completed.

Never relocate Vault, the VTA Farm API, databases, Rancher, Longhorn,
Kubernetes system workloads, Full Stack components, or an unlabeled workload
through this workflow. If every node is hot or no destination can accept the
VTA while remaining below 90%, reject the plan and emit a
cluster-capacity-exhausted alert. Moving a Pod between full nodes only transfers
the failure risk.

Do not cordon nodes solely for load management. The Longhorn drain policy may
begin moving replicas when a node is cordoned, which would combine Pod and
storage relocation in the same operation. Do not use a generic Descheduler for
this workflow: it cannot provide the required Longhorn handoff, atomic capacity
reservation, exact destination, durable recovery, or administrator audit trail.

## Production activation and verification

Implement and release the complete controller, Admin Panel, reconciliation,
relocation, recovery, backup gates, and tests together. Production activation
must still be progressive because the current volumes may have one replica and
the first reconciliation changes every Pod template. This is an operational
data-safety gate, not a sequence of incomplete product versions.

### Gate 1: prove recovery before migration

- Verify that Longhorn off-cluster backups are enabled and current for every
  VTA volume and Vault.
- Complete one restore drill from the production backup target into an isolated
  test volume and verify its VTA data through the application, not only by
  checking that the volume attaches.
- Verify graceful VTA shutdown under SIGTERM and confirm the filesystem
  unmounts before Longhorn detach.
- Exercise API/controller restart recovery at every relocation state, including
  after scale-down and before scale-up.
- Prove through RBAC and automated client-action assertions that the placement
  worker cannot delete persistent data.

Do not migrate production workloads if any of these checks fail.

### Gate 2: one production canary

Choose one non-critical VTA with known, read-only verification data. Before the
move, record its storage identities and application-level inventory such as
known DID, context, and server identifiers without logging secrets. Stop it
gracefully, create and complete an explicit off-cluster backup, reconcile its
Deployment, and relocate it to an exact eligible node.

After the move, verify:

- the PVC UID, PV UID, CSI volume handle, and Longhorn volume are unchanged;
- the Pod is Ready on the requested node and the volume is healthy and attached
  only there;
- the expected VTA application data is readable and unchanged;
- Vault is available and the VTA can access its existing secret paths;
- no filesystem, Longhorn, Kubernetes API, etcd, or VTA errors appear during
  the observation window; and
- a second relocation of the same canary succeeds and preserves the same data.

Stop production activation and use the retained volume and backup recovery
procedure if any identity or application-data check fails.

### Gate 3: bounded batches, then apply all

Migrate a small bounded set of VTAs and repeat the same storage-identity and
application checks. Increase the batch only after every member passes. The
administrator may then submit the remaining workloads as one `Apply all` job;
the backend continues to perform and verify each RWO handoff independently and
stops before the next entry on the first failure.

At no point may an observation failure be waived by deleting and recreating a
PVC. Recovery must preserve the original volume or restore an explicitly
selected completed backup to a new volume following the tested restore runbook.

## Implementation plan

Deliver the following as one complete capability. The order below expresses
technical dependencies and review boundaries; it does not define reduced
product versions. Do not activate production reconciliation until every item
and the production gates above are complete.

### 1. Prove the storage safety prerequisites

Repository: `vtafarm-k8s` and the VTA runtime.

- Verify the production Longhorn version against the supported RKE2 and
  Kubernetes versions.
- Enable the off-cluster Longhorn backup target and recurring backup job when
  they are not already enabled.
- Inventory every VTA PVC, PV, CSI volume handle, Longhorn volume, replica
  count, last successful backup, and restore point.
- Verify that Vault volumes are included in recurring backups and that the
  Vault unseal material needed after a restore is available outside the
  cluster.
- Run the documented Longhorn restore procedure into an isolated test volume
  and record the result.
- Prove that the VTA handles SIGTERM, closes its Fjall store, and unmounts its
  filesystem cleanly without forced deletion.
- Define the read-only application checks used before and after a move, such as
  known DID, context, ACL, and server identifiers, without recording secret
  values.

This work is a production prerequisite. A missing backup, untested restore, or
unproven shutdown blocks activation of reconciliation and relocation.

### 2. Add durable placement state

Repository: `vtafarm-api`.

- Add migrations and models for placement plans, per-VTA relocation entries,
  node capacity reservations, saved Deployment/storage identities, audit
  events, and durable per-session maintenance locks.
- Enforce uniqueness so one active maintenance operation exists per session
  and one Pod UID owns a reservation.
- Store the source, exact destination, p95 cost, pre-move utilization, original
  replica count, original Deployment template, PVC/PV/volume identities,
  workflow state, error, requester, and timestamps.
- Make every transition transactional and idempotent. Recovery must derive the
  next action from both database state and observed Kubernetes/Longhorn state.
- Expire only reservations proven to have no live or gated Pod; never release
  capacity based only on elapsed time.

### 3. Deploy the least-privilege placement controller

Repositories: `vtafarm-api` and `vtafarm-k8s`.

- Run the placement/reconciliation worker separately from the HTTP API under a
  dedicated ServiceAccount and leader election.
- Grant only the Kubernetes and Longhorn read/watch/patch/update permissions
  required for node labels, gated Pods, Deployment scale/template updates, and
  attachment observation.
- Explicitly deny the worker a code path and credentials for PVC, PV, Longhorn
  volume, namespace, Secret, or Vault-data creation and deletion.
- Publish `memory-state` and `vta-eligible` node labels with threshold
  hysteresis and stale-metric fail-closed behavior.
- Reuse one lightweight VTA placement inventory per poll for node refresh and
  active-relocation recovery. Keep PVC/PV/Longhorn enrichment in the Admin
  overview, and read latest-backup metadata from `BackupVolume` summaries
  rather than loading the complete backup history.
- Process a gated VTA Pod only when its identity matches an active,
  administrator-reviewed relocation task. Acquire its atomic reservation,
  tighten that Pod to the reviewed hostname, and remove the gate only after all
  health checks pass. Never infer a new plan from a gated Pod alone.
- Resume every non-terminal workflow after worker restart or leader change and
  expose worker health, queue depth, gated Pod age, reservation age, and
  workflow metrics.
- Provide an immediate kill switch that stops new disruption without deleting
  workflow or reservation records.

### 4. Generate the complete workload policy

Repository: `vtafarm-api`.

- Centralize placement label keys, memory thresholds, observation windows,
  p95 costs, scheduling gate name, and affinity/topology builders.
- Update VTA-only and parked VTA creation to include stable labels, soft
  topology spread, and `Recreate`, without delaying initial Pods behind the
  managed-placement scheduling gate.
- Update every Full Stack component to include its common labels, soft topology
  spread, and the correct RWO `Recreate` strategy without opting it into VTA
  relocation.
- Keep existing Service and Deployment selectors unchanged and separate from
  the new descriptive placement labels.
- Add generated-object tests for every creation path and negative tests proving
  that infrastructure and Full Stack workloads cannot enter managed
  relocation.

### 5. Implement existing-workload reconciliation

Repository: `vtafarm-api`.

- Discover legacy workloads through managed namespaces and current
  `app`/`session-id` labels, then verify them against database sessions.
- Build preview and apply operations backed by the durable reconciliation job
  described above.
- Capture storage and Deployment identities before any scale operation.
- For RWO workloads, perform graceful scale-down, full detach verification,
  template reconciliation, default-scheduled restart, attachment, readiness,
  application verification, and identity comparison.
- Preserve completed entries, stop on the first unsafe result, and support
  retry and rollback from the saved original template.
- Assert at the Kubernetes fake-client and integration-test levels that this
  path never creates or deletes persistent data.

### 6. Implement Admin APIs and control surface

Repositories: `vtafarm-api` and `vtafarm`.

- Add read APIs for node load, metric freshness, reservations, current VTA
  placement, storage health, eligible destinations, and projected utilization.
- Add APIs to preview, submit, cancel-before-disruption, retry, retarget, and
  roll back reconciliation and exact-node placement plans.
- Require an authenticated administrator and record the requester and every
  transition in the audit log.
- Build Admin Panel node and VTA views, a multi-entry placement-plan editor,
  preflight errors, explicit confirmation, per-entry progress, failure details,
  and recovery actions.
- Keep computed node state read-only. Resource request/limit changes use the
  existing resource controls and the same per-session maintenance lock.

### 7. Complete automated and recovery verification

Repositories: all three.

- Unit-test thresholds, hysteresis, stale metrics, reservation races,
  idempotent state transitions, recovery decisions, and storage-identity
  mismatches.
- Integration-test exact-node scheduling through a gated Pod and the default
  scheduler.
- Integration-test that a successfully relocated workload later replaced by an
  image upgrade, rollback, resource update, restart, or recovery has no gate,
  creates no automatic placement plan, and is placed by the default scheduler.
- Test controller termination at every workflow state, including replica zero,
  detach complete, gated Pod created, gate removed, volume attaching, and Pod
  Ready before the database commit.
- Test node failure, API timeout, Vault unavailability, metrics loss, hot target,
  Pending Pod, Longhorn degraded/rebuilding volume, stale attachment ticket,
  and application health failure.
- Test that concurrent relocations cannot double-reserve a node and that
  session deletion or upgrade cannot overlap relocation.
- Render and verify Helm/RBAC changes, run each repository's format, lint, and
  test entry points, and complete the restore drill before production approval.

### 8. Activate using the production gates

Deploy the complete feature set, then follow Gate 1, Gate 2, and Gate 3 above.
The controller and Admin Panel are not considered production-ready until the
canary has been relocated twice with unchanged storage identities and verified
application data. Only then apply the policy to bounded batches and finally the
remaining workloads through the single Admin Panel `Apply all` operation.

## Acceptance criteria

- The 2 GiB node reservation is visible in
  `Node.status.allocatable.memory`, and monitoring uses the same workload
  budget.
- Tests cover the `green`, `yellow`, `orange`, and `hot` boundaries,
  five-minute hot observation, ten-minute recovery, and stale metrics.
- A hot or ineligible node cannot be selected as a reviewed relocation
  destination.
- Green, yellow, and orange preferences are generated exactly as configured,
  without claiming a strict lowest-memory choice from scheduler preferences.
- Initial sessions and all non-relocation replacements proceed through the
  default Kubernetes scheduler without a placement reservation or durable
  provisioning plan.
- Concurrent relocations cannot reserve the same node headroom twice;
  reservations are atomic, durable, included in projected utilization, and
  recovered after controller restarts.
- New tenant Pods carry the common placement labels; only approved VTA-only
  Pods carry `auto-rebalance=true`.
- Generated VTA-only Deployments contain soft topology spread but no initial
  placement scheduling gate or eligibility affinity.
- A gated Pod is assigned to the exact administrator-selected eligible node by
  tightening only that Pod's affinity; the Deployment retains no gate,
  relocation affinity, or exact-hostname constraint after the one-shot move.
- A later image upgrade, rollback, resource or configuration update, restart,
  node-failure recovery, or maintenance recovery of a relocated workload does
  not create an automatic placement plan and leaves destination choice to the
  Kubernetes default scheduler.
- The placement worker rejects and reports a gated Pod that is not backed by an
  active, administrator-reviewed relocation task; it never treats that Pod as
  implicit authorization to place the workload.
- Existing Deployment selectors are unchanged.
- Single-replica RWO Deployments use `Recreate`; stateless Deployments retain
  `RollingUpdate`.
- Reconciliation discovers legacy workloads without relying on labels that do
  not exist yet, preserves their immutable selectors and user configuration,
  and can resume after an API or worker restart.
- The placement worker has no permission to create or delete PVCs, PVs,
  Longhorn volumes, namespaces, or Secrets, receives no Vault mutation
  credentials, and tests assert that relocation never performs those
  operations.
- A durable maintenance lock prevents session deletion, upgrade,
  configuration/resource update, reconciliation, and relocation from running
  concurrently for the same VTA.
- Every relocation preserves the PVC UID, PV UID, CSI volume handle, Longhorn
  volume identity, and Vault path identifiers.
- A relocation scales one VTA to zero, observes Kubernetes and Longhorn detach,
  recreates it on the exact eligible node, and completes with the replacement
  Ready, application data verified, and the volume healthy.
- A completed off-cluster backup and a tested restore exist before production
  reconciliation begins; a snapshot alone does not satisfy this criterion.
- Controller restarts at every workflow state resume idempotently without
  deleting data, starting a second writer, or leaving an untracked replica-zero
  Deployment.
- No infrastructure, Full Stack component, or unlabeled workload is eligible
  for managed relocation.
- A placement plan performs no relocation when all nodes are hot or no safe
  destination exists.
- The Admin Panel supports previewing and submitting exact-node placement
  plans, shows per-entry progress and blocking reasons, and exposes retry,
  retarget, and rollback actions.
- Capacity and Admin Panel calculations use Kubernetes Allocatable, requests,
  reservations, and the same live-memory metric consistently.
