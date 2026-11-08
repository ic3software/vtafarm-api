# VTA Farm optimization checklist

This backlog follows the staging incident in which memory pressure caused page
cache thrashing, high disk I/O, etcd and API timeouts, and an RKE2 restart. The
restart then exposed a Longhorn remount issue and left tenant workloads skewed
after maintenance.

## P0: prevent another node stall

- [ ] Add common tenant placement labels, soft topology spread, and preferred
  cross-namespace anti-affinity as described in
  [Kubernetes workload placement](kubernetes-workload-placement.md).
- [ ] Add generated-object tests for VTA-only and every Full Stack component so
  scheduling policy cannot silently disappear from one creation path.
- [ ] Add a maintenance recovery test that returns a drained node and verifies
  subsequent sessions prefer available headroom without assuming Kubernetes
  will move existing healthy Pods.
- [ ] Set `Recreate` on every long-running Deployment that mounts a Longhorn
  ReadWriteOnce PVC.
- [ ] Add a dry-run reconciliation command for applying placement policy to
  existing Deployments without changing immutable selectors.
- [ ] Measure p50, p95, and peak memory for every workload class, then calibrate
  memory requests to observed usage instead of relying on minimal defaults.
- [ ] Treat requests and limits separately: requests must express scheduling
  cost; limits should be added only where the workload has a tested failure
  mode at that boundary.
- [ ] Add an Admin Panel warning when tenant Pod counts, requested memory, or
  live memory are materially skewed across schedulable nodes.
- [ ] Add a Transit Vault dependency and PriorityClass review to `vtafarm-k8s`
  so Vault cannot be starved behind ordinary tenant workloads during recovery.
- [ ] Document the maintenance order: dependencies first, one node at a time,
  readiness and storage gates after every node.

## P1: storage and maintenance safety

- [ ] Upgrade Longhorn to a stable release containing the terminal-Pod remount
  fix after validating the RKE2, Kubernetes, and Longhorn compatibility matrix.
- [ ] Add `ttlSecondsAfterFinished` to completed provisioning and setup Jobs so
  terminal Pods cannot accumulate in Longhorn workload tracking.
- [ ] Keep Rancher drain timeout at least 30 minutes so replica relocation can
  finish before maintenance is declared failed.
- [ ] Enable `block-for-eviction-if-contains-last-replica` only for planned
  maintenance and restore the normal policy afterward. It begins replica
  eviction on cordon, so ordinary Pod rebalancing must use a dedicated
  `NoSchedule` taint instead. Do not bypass the policy with forced drain.
- [ ] Alert on degraded, faulted, detached-unexpectedly, and rebuilding volumes,
  and on nodes that become unschedulable for replicas.
- [ ] Write and test a one-volume relocation runbook before doing bulk replica
  changes.
- [ ] Revisit the single-replica policy. Document which data may be restored
  from backup and which services require multiple replicas.
- [ ] Perform recurring restore tests; backup success alone is not proof that a
  volume or etcd snapshot is recoverable.

## P1: application recovery

- [ ] Give services that depend on Vault a bounded startup retry with useful
  status reporting instead of an unbounded CrashLoopBackOff.
- [ ] Retry transient DID endpoint HTTP 503 responses with exponential backoff
  and a bounded deadline.
- [ ] Detect and alert on nonexistent container image tags before rollout.
- [ ] Review startup, readiness, and liveness probe budgets for slow volume
  reattachment and post-maintenance recovery.
- [ ] Surface dependency failures in the Admin Panel so operators can
  distinguish capacity, storage, Vault, and external-service failures.

## P1: observability and capacity

- [ ] Monitor per-node `memory.available`, major page faults, disk I/O wait,
  MemoryPressure, evictions, OOM kills, and API and etcd latency.
- [ ] Alert before the known failure zone: warn when available memory repeatedly
  falls below 768 MiB and page faults or I/O wait rise; page immediately near
  512 MiB or when API and etcd requests time out.
- [ ] Show per-node Pod counts, requested CPU and memory, limits, and live usage
  together. No single measure is a sufficient capacity signal.
- [ ] Separate infrastructure consumption from tenant workload consumption in
  dashboards and capacity calculations.
- [ ] Keep a maintenance timeline containing cordons, drains, node restarts,
  Vault unseal events, Longhorn rebuilds, and workload recovery.
- [ ] Validate the Admin Panel's additional-session estimate against real
  provisioning tests after every request or reservation change.

## P2: controlled automated balancing

- [ ] Evaluate Kubernetes Descheduler in staging with reporting or dry-run
  behavior before enabling evictions.
- [ ] Exclude system namespaces, Vault, databases, and other critical stateful
  workloads from balancing policies.
- [ ] Rate-limit evictions and prove safe RWO detach and attach behavior before
  increasing batch size.
- [ ] Produce a planned-moves report showing source, destination, PVC, replica
  location, and reason before each automated run.
- [ ] Choose an explicit optimization goal: even tenant count, even requested
  memory, or live-memory headroom. These can produce different placements.

## P2: infrastructure lifecycle

- [ ] Establish recurring OS and kernel maintenance windows with one-node-at-a-
  time rollout and rollback criteria.
- [ ] Record and test the supported RKE2, Kubernetes, Longhorn, Rancher, kernel,
  and container runtime version combinations before upgrades.
- [ ] Define the capacity trigger for moving from 8 GiB nodes to 16 GiB nodes or
  adding dedicated workers.
- [ ] Evaluate separating the control plane and etcd from tenant workloads so a
  tenant memory spike cannot starve cluster coordination.
- [ ] Run a full node-loss recovery exercise, including etcd restore, Longhorn
  restore, Vault unseal, and dependency-order verification.

## Rollout gates

Every scheduling, reservation, storage, or upgrade change should pass these
gates in staging before production:

1. configuration renders as expected in Terraform, Helm, or generated
   Kubernetes objects;
2. one-node canary remains Ready with healthy etcd and Longhorn;
3. Vault and Transit Vault are available and unsealed;
4. no new Pending, Evicted, OOMKilled, or CrashLoopBackOff workloads appear;
5. Admin Panel capacity agrees with Kubernetes Allocatable and requests; and
6. the cluster remains healthy through an observation window and a tested
   rollback.
