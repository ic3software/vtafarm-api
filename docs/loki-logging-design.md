# Cluster Log Retention with Loki and Alloy — Design

Status: approved for infrastructure planning; not implemented

Observed: 2026-11-18

Scope: Kubernetes container logs in the dev, staging, and production VTA Farm
RKE2 clusters

## 1. Decision

Install Grafana Loki and Grafana Alloy as part of the optional VTA Farm
observability stack. Alloy runs once on every node, reads the node's Kubernetes
container log files, and sends them through the private Loki gateway. Loki
keeps recent unflushed data on persistent volumes and stores durable log chunks
and their TSDB index in Hetzner Object Storage. The existing Grafana instance
queries Loki.

The initial deployment uses Loki's monolithic mode. Distributed mode is a
future scaling option, not part of this delivery.

The environment shape is fixed as follows:

| Environment | Loki replicas | Replication factor | PVC per replica | Gateway replicas |
| --- | ---: | ---: | ---: | ---: |
| dev | 1 | 1 | 10 GiB | 1 |
| staging | 3 | 3 | 20 GiB | 2 |
| production | 3 | 3 | 20 GiB | 2 |

Staging deliberately matches production's high-availability topology even
though its current log volume does not require three replicas. It must expose
quorum, memberlist, anti-affinity, PodDisruptionBudget, and rolling-upgrade
problems before the same change reaches production.

## 2. Problem

Kubernetes does not provide durable cluster-level log retention. Container
stdout and stderr are written by the runtime beneath `/var/log/pods` on the
node. Kubelet rotates those files, and deleting or evicting a Pod eventually
removes its local log files. `kubectl logs --previous` can read the previous
container instance only while the Pod still exists; it cannot recover a Pod
that has been deleted.

This is especially important for this farm because Vault writes its audit
device to stdout. The current kubelet limit of approximately 10 MiB per file
and up to five files per container intentionally bounds node disk use, but it
also makes that audit history temporary.

The logging pipeline must therefore move each log line away from the node
before kubelet rotation or Pod deletion removes it:

```text
application stdout/stderr
        |
        v
node /var/log/pods
        |
        v
Alloy DaemonSet
        |
        v
private Loki gateway
        |
        v
Loki memory + WAL
        |
        v
Hetzner Object Storage
        |
        v
private Grafana data source
```

This design cannot recover logs that disappeared before Alloy was installed.

## 3. Goals

- Preserve container stdout and stderr after Pod replacement or deletion.
- Collect application, platform, control-plane static Pod, Vault audit, and
  cluster infrastructure logs from every node. Staging and production
  currently have six nodes each; dev has two.
- Keep Loki, its gateway, and Grafana private ClusterIP services.
- Keep the data path available while one production or staging Loki replica is
  restarting or unavailable.
- Store durable history in object storage and use retained Hetzner CSI Volumes
  only for Loki's WAL and working state.
- Bound storage growth with explicit Loki Compactor retention.
- Make collection gaps, rejected writes, WAL pressure, and failed S3 uploads
  observable through the existing Prometheus stack.
- Preserve a migration path from monolithic Loki to distributed Loki without
  changing applications or Alloy's destination.

## 4. Non-goals

- Distributed Loki in the first release.
- Promtail, the legacy `loki-stack` chart, or the legacy `loki-distributed`
  chart.
- A public Loki endpoint or public Grafana access.
- Arbitrary LogQL access for tenant browsers.
- A tenant-facing logs API in `vtafarm-api`. That requires a separate product
  and authorization design comparable to `tenant-metrics-design.md`.
- Capturing files written only inside an application container. Applications
  must write operational logs to stdout/stderr unless a deliberate additional
  Alloy source is designed for a mounted path.
- Treating Kubernetes Events as container logs. Events may be added later with
  a separate cluster-scoped collector that does not duplicate them once per
  node.

## 5. Ownership and Helm releases

The implementation belongs in `vtafarm-k8s` stack 06, alongside Prometheus,
Thanos, and Grafana. It does not belong in the application Helm chart and does
not require an API deployment change.

Add two releases:

| Responsibility | Chart |
| --- | --- |
| Store, index, retain, and query logs | `oci://ghcr.io/grafana-community/helm-charts/loki` |
| Collect node-local container logs | `oci://ghcr.io/grafana/helm-charts/alloy` |

The implementation must pin tested chart versions in OpenTofu. At the time of
this design, the registries returned Loki chart `21.4.0` with Loki `3.7.10`,
and Alloy chart `1.16.0` with Alloy `1.22.0`. Those observations are not an
instruction to upgrade blindly; staging must template and verify the exact
versions selected during implementation.

The existing `kube-prometheus-stack` continues to own Grafana and Prometheus.
No additional Grafana chart is installed. The Loki data source points to the
internal Loki gateway service.

## 6. Alloy collection model

### 6.1 One collector per node

Deploy Alloy as a DaemonSet with clustering disabled. A six-node cluster has
six Alloy Pods, but they are not six replicas of the same work:

```text
Alloy on server-1  -> server-1 files only
Alloy on server-2  -> server-2 files only
Alloy on server-3  -> server-3 files only
Alloy on worker-1  -> worker-1 files only
Alloy on worker-2  -> worker-2 files only
Alloy on worker-3  -> worker-3 files only
```

Alloy must tolerate the RKE2 control-plane taints so it runs on all three
server nodes as well as all three workers. Discovery must filter targets to the
current node; otherwise multiple DaemonSet members can read the same log.

Do not schedule two file-reading Alloy Pods on each node. Independent readers
would maintain independent offsets and can deliver duplicates without
protecting against loss of the node itself.

### 6.2 Host mounts and positions

Each Alloy Pod needs:

1. a read-only hostPath mount of `/var/log`, which exposes
   `/var/log/pods`; and
2. a small writable hostPath, for example `/var/lib/alloy`, used as Alloy's
   storage path for file positions.

The first mount exposes files already owned by the node; it is not a new disk
or PVC. The second stores offsets such as "this file has been read through byte
N." It does not retain the log content.

Do not leave Alloy's storage path at the chart default `/tmp/alloy`. Persisting
positions lets a replacement Alloy Pod continue from the prior offset. Losing
positions does not erase data already accepted by Loki, but it can cause a
file to be re-read or skipped depending on the recovery policy.

Alloy does not need Longhorn PVCs or S3 access.

### 6.3 Processing and delivery

The Alloy configuration must:

- discover Pod metadata through the Kubernetes API;
- limit file targets to Pods on the local node;
- read CRI-format files from `/var/log/pods`;
- parse CRI timestamps and stdout/stderr stream markers;
- attach the cluster and stable workload metadata defined in section 10;
- send to the private Loki gateway `/loki/api/v1/push` endpoint;
- batch and retry failed writes; and
- expose Alloy metrics to the existing Prometheus instance.

An Alloy Pod restart creates a collection delay, not an immediate loss: the
replacement can continue reading the node's existing files from its persisted
positions. A permanent node loss can still lose lines that had not yet reached
Loki. A second Alloy reader on that same node does not remove this failure
mode.

## 7. Loki high availability

### 7.1 Dev

Dev uses one monolithic Loki replica with replication factor one. It provides
durable history in S3 but not service high availability. During a Loki restart,
Alloy retries and resumes delivery when the gateway is writable again.

### 7.2 Staging and production

Staging and production use three monolithic replicas with replication factor
three. Loki sends a stream write to three ingesters and acknowledges it after a
quorum of two succeeds:

```text
quorum = floor(replication_factor / 2) + 1
       = floor(3 / 2) + 1
       = 2
```

This allows ingestion to continue while one Loki Pod, worker node, or Loki PVC
is unavailable. It also allows a rolling upgrade to keep two replicas ready.
Losing two replicas removes write quorum; Alloy must retry until quorum is
restored.

Replication protects recent in-memory and WAL data. The three replicas share
the same object-storage locations. They do not receive separate buckets. Loki's
content-addressed chunks avoid three independent long-term copies when the
ingesters flush identical data, although temporarily divergent replicas may
produce objects that queries later deduplicate.

Place the three Loki Pods on distinct worker nodes using soft or required Pod
anti-affinity keyed by `kubernetes.io/hostname`. Use a PodDisruptionBudget with
`minAvailable: 2`. Production and staging also need two gateway replicas and a
gateway PodDisruptionBudget with `minAvailable: 1`; otherwise a singleton
gateway remains the ingestion and query single point of failure.

Three replicas improve availability and query concurrency. They are not needed
for the observed ingestion throughput, and monolithic mode still cannot scale
read and write components independently.

## 8. Storage model

### 8.1 Loki PVCs

Each monolithic Loki replica receives one retained Hetzner CSI PVC. The PVC
holds working state, not the full retention window:

- the ingester write-ahead log;
- chunks that have not yet been flushed successfully;
- active index and query cache data;
- Compactor working files and retention markers; and
- restart/recovery headroom.

Set StatefulSet PVC retention to `Retain` for deletion and scale-down. A Helm
uninstall must not silently delete the WAL and working state.

The selected sizes are:

| Environment | PVC count | PVC size | Logical requested capacity |
| --- | ---: | ---: | ---: |
| dev | 1 | 10 GiB | 10 GiB |
| staging | 3 | 20 GiB | 60 GiB |
| production | 3 | 20 GiB | 60 GiB |

Each PVC is one Hetzner Cloud Volume. The logical totals above are not
object-storage usage.

### 8.2 Object-storage bucket and prefixes

Create one private bucket named `loki`. Each cluster receives separate chunks
and ruler storage prefixes:

```text
loki bucket
├── <cluster-name>-loki-chunks
└── <cluster-name>-loki-ruler
```

For example, dev uses `rke2-vtafarm-dev-loki-chunks` and
`rke2-vtafarm-dev-loki-ruler`. Loki 3.7 storage prefixes accept letters,
numbers, and hyphens but reject `/`, so the supported layout uses two flat
cluster-scoped prefixes instead of `loki/<cluster>/...` path segments.

The chunks prefix contains:

- compressed log chunks;
- the Loki TSDB index;
- Compactor and delete-request objects; and
- other object-store metadata required to query and retain logs.

The ruler prefix contains Loki alerting and recording rule definitions. It does
not contain normal Pod logs and should remain very small. OSS Loki does not
need the Grafana Enterprise Logs admin bucket.

Do not mix Loki objects into the Thanos bucket. Separate buckets prevent Loki
retention, lifecycle, permissions, or incident response from affecting metrics
history.

Inject S3 credentials through a Kubernetes Secret. Do not render credentials
into a ConfigMap, committed values file, log message, or documentation.

### 8.3 Loki TSDB is not local Prometheus retention

Loki's TSDB is the index format that maps time ranges and label sets to log
chunks. It does not create a Prometheus-like seven-day local database followed
by a separate 30-day archive.

The normal data path is:

```text
recent, unflushed lines -> ingester memory and WAL
completed chunks       -> S3 chunks prefix
TSDB index files       -> S3 chunks prefix
30-day history         -> S3 chunks prefix
```

Queries merge recent data from ingesters with flushed history from object
storage. The PVC is a recovery and working volume; S3 is the durable source of
truth for the retention window.

## 9. Capacity evidence and sizing

The following read-only observations were collected from each kubelet's stats
summary on 2026-11-18:

| Measurement | Staging | Production |
| --- | ---: | ---: |
| Nodes | 6 | 6 |
| Pods | 225 | 206 |
| Running Pods | 200 | 193 |
| Containers reported by kubelet | 244 | 237 |
| Current container log files | 308 | 295 |
| Current retained node log bytes | 0.405 GiB | 0.323 GiB |

A second read counted bytes returned for all running containers over a
15-minute interval:

| Measurement | Staging | Production |
| --- | ---: | ---: |
| Bytes readable for the last 15 minutes | 5.20 MiB | 4.23 MiB |
| Straight-line uncompressed estimate | 0.49 GiB/day | 0.40 GiB/day |
| Planning rate with 3x safety factor | 1.5 GiB/day | 1.2 GiB/day |

The interval estimate is a lower bound because it excludes deleted Pods,
already-rotated files, and traffic peaks. The three-times planning factor is
therefore used for capacity reasoning, not the raw extrapolation.

At those planning rates, a conservative local-volume model is:

```text
PVC per replica
  = 10 GiB base working space
  + planning ingest per day * tolerated S3 outage days * 2 safety factor
```

Three days of S3 disruption gives approximately 19 GiB for staging and
17.2 GiB for production. This supports using 20 GiB per replica in both
environments. Production currently generates less log volume than staging, so
it does not require a larger PVC merely because it is the production cluster.

For 30 days in object storage, the uncompressed planning envelope is
approximately 45 GiB for staging and 36 GiB for production before index and
metadata overhead. Actual compressed object use should be lower, but capacity
and cost decisions must not depend on an assumed compression ratio.

## 10. Labels and structured metadata

Loki index labels must remain small, bounded, and aligned with common query
selectors. Start with:

- `cluster`;
- `namespace`;
- `app`; and
- `container`.

Keep high-cardinality or ephemeral values as structured metadata rather than
index labels:

- Pod name;
- Pod UID;
- node name; and
- `session-id`.

Do not promote user IDs, request IDs, DIDs, connection identifiers, timestamps,
or arbitrary application fields into index labels. A new label value creates a
new stream, and unbounded labels fragment chunks and increase index cost.

The existing `session-id` remains useful for authorized session drill-down, but
it is expected to grow without a small fixed bound. Store it as structured
metadata unless measured query behavior proves that a bounded index label is
necessary.

## 11. Retention

Use TSDB schema v13 with a 24-hour index period. The initial global log
retention is 30 days.

Retention requires both:

- `limits_config.retention_period` set to 30 days; and
- Compactor retention enabled with an S3 delete-request store.

Setting only a period does not make old objects disappear. The Compactor must
remove index references before deleting chunks.

Do not apply a blanket 30-day Object Storage lifecycle rule to the entire Loki
bucket. An object-store lifecycle policy is unaware of the Loki index and can
delete a still-referenced object. If a lifecycle policy is later added as a
safety net, it must be scoped to the chunks prefix and longer than Loki
retention plus the Compactor delete delay.

## 12. Security and access

- Loki, its gateway, Grafana, and Alloy's UI remain private ClusterIP services.
- No public Ingress is created for Loki or Alloy.
- Grafana operator access continues through the existing private workflow.
- Network policy should allow Alloy to write to the gateway, Loki components to
  communicate with each other and S3, Grafana to query the gateway, and
  Prometheus to scrape metrics.
- Log content is sensitive. Vault audit lines, internal hostnames, identifiers,
  and accidental application secrets may be present even when the source
  intended otherwise.
- Tenant browsers must never receive a Loki URL or arbitrary LogQL access.
- A future tenant logs API must first authorize session ownership in PostgreSQL
  and construct bounded server-side queries. That work is not authorized by
  this document.

For the initial operator-only deployment, choose Loki authentication mode
deliberately. If Loki multi-tenancy is disabled, network isolation is the
security boundary. If it remains enabled, Alloy and Grafana must consistently
set the environment's tenant header. Do not accept arbitrary tenant headers
from public traffic.

## 13. Monitoring and alerts

Enable Loki and Alloy ServiceMonitors for the existing Prometheus instance.
At minimum alert on:

- Alloy write failures, dropped entries, and sustained retry backlog;
- Alloy targets that stop advancing;
- Loki requests rejected for rate, ordering, or size limits;
- Loki ingester WAL disk use above 60%, 75%, and 85%;
- a Loki ring with fewer than three healthy members in staging or production;
- loss of write quorum;
- failed chunk or index uploads to S3;
- Compactor failures or retention falling behind;
- gateway availability; and
- Loki PVC usage above 60%, 75%, and 85%.

The first deployment should record actual compressed S3 growth and peak
ingestion for at least 30 days. Revisit PVCs, resources, and retention using
those measurements rather than the pre-installation snapshot alone.

## 14. Rollout

1. Create the private `loki` bucket for cluster-scoped chunks and ruler
   prefixes.
2. Install one dev Loki replica with a 10 GiB retained PVC.
3. Add the internal Loki data source to the existing dev Grafana.
4. Install Alloy as a DaemonSet on every dev node with persistent positions.
5. Emit a unique marker from a short-lived Pod, delete the Pod, and verify the
   marker remains queryable.
6. Restart Alloy on one node and verify positions resume without a gap or a
   large duplicate replay.
7. Restart Loki and verify WAL recovery plus historical S3 queries.
8. Install the three-replica topology in staging with 20 GiB PVCs, two gateway
   replicas, anti-affinity, and PodDisruptionBudgets.
9. Remove one Loki replica and verify writes continue with quorum two; restore
   it and verify ring health.
10. Perform a staging rolling upgrade and confirm continuous ingestion and
    query availability.
11. Install production with three 20 GiB PVCs only after staging passes.

## 15. Verification criteria

The logging layer is ready when all of the following are true:

- one Alloy Pod is ready on every server and worker node;
- each Alloy instance reads only its local node's files;
- a deleted Pod's marker remains queryable for the configured retention;
- Vault audit stdout is present without exposing Loki publicly;
- recent queries include data still held by ingesters and older queries read
  object storage;
- object storage contains both chunks and TSDB index objects;
- one unavailable Loki replica does not interrupt staging or production
  writes;
- two unavailable replicas cause a visible quorum failure and Alloy retries;
- a rolling upgrade preserves ingestion;
- retention removes data older than 30 days without corrupting recent queries;
- an Alloy restart resumes from persisted positions; and
- dashboards and alerts expose gaps, rejects, WAL pressure, and S3 failures.

## 16. Future distributed migration

Move from monolithic to distributed Loki only when measured ingestion or query
load requires independent component scaling. The current volume is far below
that threshold.

The migration remains practical because:

- Alloy always writes to a stable gateway Service rather than a Pod;
- Grafana always queries that gateway;
- TSDB v13 and object storage are used from the first day; and
- durable history is already outside the monolithic Pods.

It is still an infrastructure migration, not a one-click replica change. It
requires a supported Helm migration mode or staged cutover, component-specific
resources, ring verification, and rollback testing. No application should need
to change its logging behavior.

## 17. References

- [Kubernetes logging architecture](https://kubernetes.io/docs/concepts/cluster-administration/logging/)
- [Loki Helm installation](https://grafana.com/docs/loki/latest/setup/install/helm/)
- [Loki monolithic Helm installation](https://grafana.com/docs/loki/latest/setup/install/helm/install-monolithic/)
- [Loki components and replication](https://grafana.com/docs/loki/latest/get-started/components/)
- [Loki retention](https://grafana.com/docs/loki/latest/operations/storage/retention/)
- [Alloy Kubernetes log collection](https://grafana.com/docs/alloy/latest/collect/logs-in-kubernetes/)
- [Alloy file source and positions](https://grafana.com/docs/alloy/latest/reference/components/loki/loki.source.file/)
