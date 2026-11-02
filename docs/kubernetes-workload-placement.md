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

Add stable labels to every tenant workload Pod template. Do not change existing
Deployment selectors because selector fields are immutable.

```yaml
metadata:
  labels:
    vtafarm.ic3.dev/workload-class: tenant
    vtafarm.ic3.dev/mode: vta-only # or full-stack
    vtafarm.ic3.dev/component: vta # mediator, dids, vtc, trust-registry, etc.
```

The relevant creation paths are `internal/k8s/vta_resources.go` and
`internal/k8s/component_resources.go`. The full-stack orchestrators should pass
the mode and component values through those shared helpers.

## Recommended scheduling policy

### Soft topology spread

Use a soft hostname spread rule on tenant workloads:

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

`ScheduleAnyway` is deliberate. A hard `DoNotSchedule` rule could prevent a
VTA from starting while a node is being maintained.

There is an important limitation: topology spread counts matching Pods in the
new Pod's namespace. VTA Farm uses one namespace per user, so this rule alone
does not produce farm-wide balance.

### Preferred farm-wide anti-affinity

Add a preferred anti-affinity rule with an empty `namespaceSelector`. This lets
the scheduler consider tenant workloads in all namespaces while still allowing
placement when no ideal node exists.

```yaml
spec:
  affinity:
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          podAffinityTerm:
            topologyKey: kubernetes.io/hostname
            namespaceSelector: {}
            labelSelector:
              matchLabels:
                vtafarm.ic3.dev/workload-class: tenant
```

Use the topology spread and preferred anti-affinity rules together. The first
keeps a user's related components spread where possible; the second gives the
scheduler a farm-wide signal.

### Implementation requirements

- Generate the same placement policy through the shared VTA and component
  Deployment helpers so VTA-only and Full Stack sessions cannot diverge.
- Apply the common tenant label to Pod templates without changing existing
  immutable Deployment selectors.
- Keep the rules preferred rather than hard so provisioning can continue while
  a node is under maintenance.
- Treat cross-namespace anti-affinity as the farm-wide signal. Do not assume
  topology spread alone counts Pods in other user namespaces.
- Calibrate memory requests from observed p50, p95, and peak usage before using
  requested memory as the Admin Panel's sole placement or capacity signal.
- Reconcile existing Deployments explicitly; changing only the creation path
  leaves all current sessions without the new policy.

## RWO volume update strategy

Long-running Deployments that mount a Longhorn ReadWriteOnce PVC should use the
`Recreate` strategy:

```yaml
spec:
  strategy:
    type: Recreate
```

This prevents a rolling update from starting a replacement Pod while the old
Pod still owns the volume. It does not make the workload highly available; it
makes the single-writer handoff explicit.

## Applying the policy to existing workloads

The current creation helpers treat an existing Deployment as success. Adding
placement fields to newly generated objects therefore affects new sessions but
does not update existing Deployments.

Implement an explicit reconciliation path that:

1. lists VTA Farm-managed Deployments by label;
2. patches only Pod-template labels, affinity, topology spread, and the
   Deployment strategy;
3. preserves selectors, images, environment variables, PVC references, and
   user-specific settings;
4. supports a dry-run report before mutation; and
5. rolls out in small batches with readiness and Longhorn health gates.

## Manual rebalancing runbook

Use this only after all nodes are Ready, Vault dependencies are available, and
all Longhorn volumes are healthy.

1. Record node readiness, Pod placement, live memory, requested memory, and
   Longhorn volume health.
2. Select at most five ordinary VTA-only Pods from an overloaded node. Exclude
   Vault, databases, Rancher agents, Longhorn components, and unhealthy Pods.
3. Prefer Pods whose only Longhorn replica is already healthy on the target
   node, reducing volume movement during the handoff.
4. Add a temporary, VTA Farm-specific `NoSchedule` taint to the non-target
   nodes so the replacements have one valid destination. Do not cordon nodes
   solely to rebalance Pods: the staging Longhorn drain policy begins evicting
   last replicas as soon as a node is cordoned, even when no drain follows.

   ```bash
   kubectl taint node <non-target-node> \
     vtafarm.ic3.dev/rebalance=blocked:NoSchedule
   ```
5. Delete only the selected Pods. Do not delete their Deployments, PVCs, PVs,
   or Longhorn volumes.
6. Wait until every replacement is Ready and every affected Longhorn volume is
   healthy and attached.
7. Remove the temporary taints from every node, including on failure. A shell
   runbook should install a trap before adding the first taint.

   ```bash
   kubectl taint node <non-target-node> vtafarm.ic3.dev/rebalance-
   ```
8. Recheck node pressure, API and etcd responsiveness, Pod restarts, and Vault
   availability before selecting another batch.

Stop immediately if a replacement remains Pending, a volume is degraded, a
node reports MemoryPressure, API or etcd requests time out, or a dependency
enters CrashLoopBackOff.

## Optional automated rebalancing

Kubernetes Descheduler can be evaluated after the placement policy and
requests are accurate. Start in staging with dry-run reporting, exclude system
and critical stateful workloads, and rate-limit evictions. It must not be used
as a substitute for correct resource requests or sufficient node capacity.

## Acceptance criteria

- New tenant Pods carry the common placement labels.
- Generated VTA-only and Full Stack Deployments contain the expected soft
  topology spread and cross-namespace preferred anti-affinity rules.
- Unit tests verify the complete generated scheduling fields, including
  `ScheduleAnyway`, `kubernetes.io/hostname`, `namespaceSelector: {}`, weight,
  and label selectors.
- A provisioning test distributes new sessions across available nodes without
  becoming blocked when one node is cordoned.
- A maintenance recovery test proves that sessions created after a node returns
  prefer the less occupied node; the test must not expect existing healthy Pods
  to move automatically.
- Existing Deployment selectors are unchanged.
- RWO-mounted Deployments use `Recreate`.
- A five-Pod staging rebalance completes with all replacements Ready and all
  Longhorn volumes healthy.
- Capacity and Admin Panel calculations continue to use Kubernetes
  `Allocatable`, requests, and live usage consistently.
