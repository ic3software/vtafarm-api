# VTA Backup and Restore — Design

Status: proposed; not implemented.

This document defines per-session backup, shutdown, archive, restore, and permanent deletion for
VTA Farm. It covers both `vta_only` and `full_stack` sessions and makes the lifecycle of Vault
secrets explicit.

The central rule is:

> A session remains recoverable while at least one available backup exists. Its database
> tombstone and Vault secret trees must survive for the same period. Permanent deletion removes
> all three: volume backups, Vault secrets, and the tombstone.

This design deliberately does not grant the main API permission to read tenant secret values.

---

## 1. Goals

- Let a user create multiple named backups of a session.
- Let the user select one backup and restore that exact point in time.
- Support a backup followed by shutdown, so compute and live PVC usage can be released.
- Restore a stopped or archived session without changing its owner, public identity, session ID,
  Kubernetes resource names, or Vault paths.
- Support `vta_only` and the four PVCs in `full_stack` as one consistency group.
- Keep a recoverable backup after the live workloads and PVCs have been removed.
- Delete Vault secrets only when neither a live session nor a recoverable backup needs them.
- Make every long-running operation resumable after an API restart.
- Preserve the current security boundary: the API may list and delete Vault metadata, but may not
  read tenant secret values.
- Keep a signed copy of each restore manifest outside PostgreSQL so the backup catalog can be
  rebuilt after a database loss or point-in-time mismatch.

## 2. Non-goals

- A per-session backup is not a replacement for PostgreSQL, Vault Raft, cluster etcd, or Longhorn
  backup-target disaster recovery.
- A backup is not a container image. Images remain immutable external artifacts and their exact
  references are recorded in the backup manifest.
- Version 1 does not clone a backup into a different user or a new logical session.
- Version 1 does not export Vault plaintext into S3.
- Version 1 does not promise a live, application-consistent snapshot without stopping writes.
- Version 1 does not model provider/consumer session dependencies. A consumer may be unavailable
  while its provider session is stopped, archived, or restoring; that is an expected operational
  state rather than a reason to block the provider operation.

---

## 3. What must be backed up

A restorable session is the combination of four kinds of state. A PVC backup alone is
insufficient.

| State | Location | Backup treatment |
| --- | --- | --- |
| Component files and databases | Longhorn PVCs | Durable Longhorn backup to the configured S3 target |
| Tenant secrets | Vault KV v2 | Retain the existing immutable session secret trees in Vault |
| Session identity and desired state | PostgreSQL | Retain the `setup_sessions` row as a tombstone and store an allowlisted manifest |
| Public routing | DNS, Ingress, TLS, DID hosting | Reconcile from the tombstone and manifest during restore |

### 3.1 PVC set

`vta_only` has one PVC:

| Component | PVC |
| --- | --- |
| VTA | `vta-data-<session-id>` |

`full_stack` has four PVCs:

| Component | PVC |
| --- | --- |
| VTA | `fs-<session-id>-vta` |
| Mediator | `fs-<session-id>-mediator` |
| DID hosting | `fs-<session-id>-dids` |
| VTC | `fs-<session-id>-vtc` |

All PVCs in one session belong to one backup consistency group. A `full_stack` backup is
`available` only after all four Longhorn backups complete.

### 3.2 Vault secret set

Each session has an immutable, opaque secret-generation ID. The recoverable Vault prefixes are:

```text
vta/user-<user-id>/session-<session-id>/generation-<generation-id>/...
mediator/user-<user-id>/session-<session-id>/generation-<generation-id>/...
dids/user-<user-id>/session-<session-id>/generation-<generation-id>/...
vtc/user-<user-id>/session-<session-id>/generation-<generation-id>/...
```

Only the VTA prefix is required for `vta_only`; all four are required for `full_stack`.

The PVC snapshot cannot contain these values because the components read them from Vault at
runtime. The first implementation must therefore retain the referenced Vault generation while a
backup is recoverable. It must not copy secret values into the backup manifest. Existing
unversioned sessions are treated as generation `legacy`; they keep their current paths until an
explicit migration is designed and tested.

### 3.3 Restore manifest

The manifest is an allowlisted snapshot of the configuration needed to recreate workloads. It
includes:

- session ID, owner ID, mode, domain type, public names, and FQDNs;
- immutable component image digests, not mutable tags;
- PVC names, exact sizes in bytes, source volume IDs, and durable Longhorn backup URLs;
- the Longhorn version, data engine, replica count, filesystem type, backup block size, backup
  target name, StorageClass, and relevant topology or encryption references;
- component resource profiles;
- DID and DID-hosting identifiers needed to reconcile external state;
- exact Vault prefix names and the secret-generation identifier, never secret values;
- manifest schema version and checksums;
- creation time and the application/API version that created the backup.

It must exclude reveal-once credentials, private keys, claim codes, Vault tokens, Kubernetes
credentials, Cloudflare credentials, and S3 credentials. An internal Longhorn backup URL is not
returned to a normal user; the API exposes only backup IDs and safe metadata.

### 3.4 Durable manifest catalog

PostgreSQL is the primary query index, but it must not be the only copy of the restore manifest.
After every component backup completes, serialize the allowlisted manifest, sign it, and write it
to an API-owned S3 bucket or prefix separate from Longhorn's backupstore, for example:

```text
s3://<catalog-bucket>/<cluster-id>/<user-id>/<session-id>/<backup-id>/manifest.json
```

The catalog object contains no secret values or credentials. Sign it with a cluster-scoped
asymmetric Vault Transit key and preserve the non-secret public verification key with the disaster
recovery configuration. Its checksum, signature, object key, and schema version are stored in the
database. A backup becomes `available` only after the object can be read back and its signature
and checksum verify. S3 versioning may protect this catalog, but retention and deletion semantics
must be explicit so a purge can distinguish immediate logical unavailability from delayed
physical removal.

This second copy allows a disaster-recovery tool to rebuild the database catalog and reconstruct
which one or four Longhorn backups form a consistency group. It does not replace PostgreSQL
backups and must not be placed under the Longhorn-managed backup target prefix.

---

## 4. Snapshot versus backup

Longhorn uses two related objects:

- a **snapshot** is local to the source Longhorn volume and is useful as an intermediate
  checkpoint;
- a **backup** uploads snapshot data to the configured backup target and survives deletion of the
  source PVC and volume.

User-visible backups must always be durable Longhorn backups. A local snapshot by itself must
never produce an `available` API response. The implementation may delete the temporary local
snapshot after the durable backup completes.

This distinction is what makes the following sequence safe:

```text
quiesce workloads
  -> attach every PVC to a no-write holder Pod
  -> snapshot every attached PVC
  -> start durable backups from those immutable snapshots
  -> resume normal workloads, or keep the holder for a stopped/archive operation
  -> upload every snapshot to the Longhorn backup target
  -> record and verify every backup URL and the S3 catalog manifest
  -> optionally remove live PVCs and leave the session stopped
```

Longhorn snapshots require a running volume engine. Creating a snapshot after every workload has
stopped and the volume has detached can leave the request retrying until something attaches the
volume again. The holder Pod provides that controlled attachment without restarting an
application that could modify the data.

---

## 5. Lifecycle semantics

Backup, stop, archive, delete, and purge are different operations and must not be represented by
one ambiguous Delete button.

| Operation | Workloads | Live PVCs | Durable backups | Vault secrets | DB session row |
| --- | --- | --- | --- | --- | --- |
| Back up | Restarted after backup | Kept | Added | Kept | Active |
| Stop | Scaled to zero | Kept | Unchanged | Kept | `stopped` |
| Archive | Scaled to zero | Removed after backup completes | At least one kept | Kept | Tombstone |
| Restore | Recreated and started | Restored from selected backup | All kept | Reused | Active |
| Purge | Removed | Removed | Removed | Removed | Removed after cleanup succeeds |

Recommended state transitions:

```text
running --backup--> backing_up ---------> running
                         |
                         +--stop_after--> stopped

running --archive--> backing_up --> archived
stopped/archived --restore backup N--> restoring --> running

running/stopped/archived --purge--> purging --> deleted
```

`archived` means the session is intentionally recoverable but has no live PVCs. Its session ID,
public name, DNS ownership, and Vault paths remain reserved.

### 5.1 Existing delete endpoint

The existing delete flow removes PVCs, Vault secrets, external records, and the database row. It
must not silently retain secrets under a response that users understand as permanent deletion.

After backup support exists, `DELETE /api/v1/setup/:id` should behave as follows:

1. If no recoverable backup exists, run the current permanent deletion flow.
2. If a recoverable backup exists, return `409 Conflict` and require the caller to choose:
   - archive the live instance while retaining backups; or
   - permanently purge the instance and every backup.
3. Purge requires the existing typed-name confirmation and an explicit `purge_backups` flag.

This prevents an ordinary delete from either destroying recovery points or secretly retaining
data the user expected to be erased.

### 5.2 Account deletion

Database foreign-key cascade is not an external-resource cleanup mechanism. Account deletion must
first mark the user `deleting`, revoke interactive access, and reject new sessions and backups.
It then persists and reconciles a purge operation for every live or archived session, including
Kubernetes resources, retained Longhorn volumes, provider backups, S3 catalog objects, DNS state,
Vault secret generations, and the per-user Vault role and policy. Delete the user row only after
all child purges reach a verified terminal state.

The implementation must not allow `ON DELETE CASCADE` to erase session tombstones and backup
catalog rows before external cleanup finishes. A legal-hold or administrative-retention feature,
if ever required, is a separate explicit state and must not masquerade as user-requested account
deletion.

---

## 6. Creating a backup

Backup creation is asynchronous and returns `202 Accepted` with a backup ID.

### 6.1 Preconditions

- The authenticated user owns the session, or the caller is an administrator.
- The session is `running` or `stopped`.
- No setup, upgrade, backup, restore, config edit, ACL edit, or purge holds the session maintenance
  lock.
- Every expected PVC is bound and backed by Longhorn.
- The Longhorn backup target is available.
- All required Vault prefixes exist. This check lists metadata only; it does not read values.
- The user is below backup-count and storage quotas.
- The request passes the rate and concurrency limits in section 6.3.

### 6.2 Consistent backup sequence

1. Create a `session_backups` row in `queued` state.
2. Acquire the exclusive session maintenance lock in the database.
3. Move the backup to `quiescing` and reject new maintenance operations.
4. Block new external writes, then gracefully scale component Deployments to zero.
5. Wait for all application and maintenance Pods to terminate and for every RWO PVC to be free of
   consumers.
6. Create a short-lived holder Pod for each PVC and wait until every Longhorn volume is attached
   and healthy. Mount claims read-only; disable service-account token mounting; grant no Vault
   identity; and apply the namespace's restrictive network policy.
7. Capture the allowlisted restore manifest and exact Vault generation references.
8. Create a Longhorn snapshot for every attached PVC and wait until every snapshot is ready.
9. Create a Longhorn Backup object for every immutable snapshot.
10. For a normal backup, delete the holder Pods as soon as all snapshots exist, recreate or scale
    up Deployments, and wait for readiness while uploads continue. Application writes after this
    point cannot change the captured snapshots.
11. For `stop_after_backup` or archive, leave the no-write holder Pods attached until all uploads
    finish, then delete them so the volumes detach.
12. Wait for every Longhorn backup to complete and verify its URL, exact volume size, checksum,
    and compatibility metadata.
13. Persist the complete consistency group transactionally, then write and read back the signed
    catalog manifest described in section 3.4.
14. Mark the backup `available` only when both the Longhorn group and S3 catalog object are
    durable and verified.
15. Release the maintenance lock.

Stopping all components before the first snapshot makes sequential snapshots a consistent group:
no component can modify its PVC while another component is being captured. For `full_stack`, stop
clients first and start dependencies first:

```text
stop:  VTC -> VTA -> Mediator -> DID hosting
start: DID hosting -> Mediator -> VTA -> VTC
```

If quiescing, holder attachment, snapshot creation, or backup upload fails, delete every holder,
restart a previously running session, and mark the backup `failed`. A failed partial group must
never be selectable for restore. The reconciler must also remove abandoned holders.

### 6.3 Rate limits and concurrency

Backup creation needs throttling. The maintenance lock already prevents simultaneous operations
on one session, but it does not prevent a user from repeatedly consuming Longhorn, network, and
S3 capacity as soon as each request completes.

Recommended initial defaults are:

- one active backup, restore, archive, upgrade, or purge operation per session;
- a 15-minute cooldown between accepted manual backup requests for the same session;
- at most two active backup operations per user;
- a configurable cluster-wide limit based on measured backup throughput;
- normal count and byte quotas for retained backups.

Reject a request that exceeds a time-based limit with `429 Too Many Requests` and `Retry-After`;
return the existing operation for a repeated idempotency key. Archive and mandatory pre-restore
safety backups may bypass the manual cooldown, but never the session lock, quota reservation, or
cluster-wide capacity limit. A failed attempt receives bounded exponential retry rather than an
unlimited immediate retry loop.

### 6.4 Archive sequence

Archive runs the same consistent backup sequence with `stop_after_backup=true`. After the backup
is `available`, it may remove the live PVCs and their Longhorn volumes. Services, Ingresses,
public names, the database tombstone, and Vault secrets remain so the same logical session can be
restored.

The implementation must never delete a live PVC before every component backup is complete and
recorded.

---

## 7. Restoring a selected backup

Every available backup has an immutable ID. Restoring an older backup does not delete newer
backups.

### 7.1 Preconditions

- The backup is `available`, belongs to the same user and session, and has a supported manifest
  version.
- No other maintenance operation is active.
- The session tombstone still exists.
- The session's required Vault secret generation still exists.
- The domain is still owned by the session and a custom domain still passes the required checks.
- Every component image remains pullable, unless an explicitly validated compatible override is
  supplied.

### 7.2 Restore sequence

1. Create a `session_restore_operations` row and acquire the maintenance lock.
2. If the session is running, quiesce it and create a mandatory pre-restore safety backup.
3. Mark the session `restoring`.
4. Ensure the user namespace, ServiceAccounts, Vault policy, and Kubernetes-auth role exist.
5. Confirm the required Vault prefixes without reading their values.
6. Resolve and persist every current PVC, PV, and Longhorn volume mapping. Patch each old PV to
   `Retain` and verify the observed policy before deleting any claim.
7. For each component, create a new Longhorn volume from the selected backup URL using the exact
   byte size and compatibility metadata recorded in the manifest.
8. Wait until every restored volume reports restore completion, is healthy, and is detached.
9. Confirm again that no Pod or Job uses the old claims, then delete the old PVCs and PV objects.
   The patched `Retain` policy leaves the old Longhorn volumes available for rollback.
10. Create new static PVs with `Retain` pointing at the restored Longhorn volumes and bind PVCs
    using the original deterministic claim names. Verify every claim is bound to the intended new
    volume.
11. Recreate Services, Ingresses, certificates, middleware, and Deployments from the tombstone and
   manifest.
12. Reconcile DNS and required DID-hosting state.
13. Start components in dependency order and wait for readiness.
14. Run restore verification checks.
15. Mark the restore complete and the session `running`.
16. Retain replaced volumes for a short rollback window, then remove them asynchronously.

Restore must create new volumes; it must not attempt to overwrite a mounted Longhorn volume in
place. The selected durable backup remains immutable and may be restored again.

The infrastructure defines two classes with intentionally different semantics. Session PVCs do
not set `storageClassName`, so they use the default `longhorn` class, whose reclaim policy is
`Delete`. The separately defined `longhorn-retain` class is currently selected for PostgreSQL,
not tenant session PVCs. Therefore the per-PV patch and observed-state check above are mandatory;
the existence of `longhorn-retain` alone does not protect an existing session volume.

If verification fails, stop the restored workloads, patch the new PVs to `Retain`, swap the
original claim names back to static PVs pointing at the retained old volumes, and verify the old
session before deleting anything. After a successful rollback window, patch the active restored
PVs back to the normal session policy of `Delete`, then delete the released old PV objects and old
Longhorn volumes explicitly. Purge uses the same explicit cleanup path so retained volumes cannot
become permanent orphans.

### 7.3 Verification

At minimum, a successful restore proves:

- all expected Deployments are available;
- all original PVC names are bound to the restored volumes;
- every public health endpoint responds;
- the VTA reports the expected DID and configuration identity;
- full-stack dependencies can reach each other;
- Vault authentication succeeds without exposing values to the API;
- data created before the selected backup exists; and
- a test marker created after that backup does not exist.

If verification fails, leave the session stopped, preserve both the selected backup and the
pre-restore safety backup, and expose a retryable failure. Do not continue into cleanup.

---

## 8. Vault lifecycle

### 8.1 Version 1 decision: retain, do not export

The current `vtafarm-api-admin` policy deliberately has no `read` permission on tenant secret
data. Backup must not broaden that long-lived AppRole to read every tenant's master seed and
component keys.

Version 1 therefore treats the retained Vault secret generation as a dependency of every backup:

```text
secret tree may be deleted only when:
  no live session uses it
  AND no queued/creating/available/restoring backup references it
  AND no restore operation can still roll back to it
```

The backup row and S3 manifest record exact prefix names and a generation identifier, not secret
values. Restored workloads are configured to use the generation referenced by the selected
backup.

### 8.2 Secret immutability requirement

Provisioning creates one opaque generation ID. Backups do not copy the secrets; they only
increment the durable reference count to the existing generation. Before the first backup becomes
selectable, the implementation must prove that all secret values needed by restore are present and
then seal the generation: no key may be created, overwritten, or destroyed while a backup
references it. If a component requires mutable runtime secrets, those values need their own
versioned-generation design before that component can claim point-in-time backup compatibility.

Standard Vault KV v2 has no per-secret server-side copy operation. Metadata `list` can discover
paths and versions but cannot reproduce their values. Copying a generation to another prefix
requires an actor with `read` on the source and `create` on the destination; the values necessarily
pass through that actor's memory even if they are never shown to a person, logged, or written to
disk. The long-lived API AppRole must not receive those permissions.

Secret rotation is out of scope for version 1. Before it ships, it must create a new immutable
generation and keep the old generation until no backup references it. If unchanged values must be
carried forward, use a short-lived, single-session worker with source-read and destination-write
permissions, audit every path copied, prohibit stdout/stderr value logging, and revoke the token
when it exits. A whole-Vault Raft snapshot protects disaster recovery but is not a substitute for
this per-session generation workflow.

### 8.3 Reference counting and final cleanup

Vault cleanup must be reconciliation-driven, not best-effort fire-and-forget:

1. Mark the session tombstone `purging`.
2. Delete all Longhorn backup objects and confirm removal from the backup target.
3. Confirm that no backup or restore row references the session secret generation.
4. Recursively delete the VTA, mediator, DIDs, and VTC Vault trees as applicable.
5. Remove the per-user Kubernetes-auth role and policy when the user has no running,
   provisioning, or restoring workload. Archived secret data remains in Vault without workload
   access; restore recreates access before starting Pods.
6. Remove the database tombstone only after Vault cleanup succeeds.

Failures remain in `purging` and are retried. Removing the tombstone before Vault deletion would
recreate the orphan-secret bug because the API would lose the IDs needed to find the paths.

### 8.4 What a Vault outage means

A volume backup is not independently restorable if its Vault secret generation is lost. The
platform must separately back up and restore Farm Vault storage and preserve the Transit Vault
and unseal material. Per-session restore should report `vault_dependency_missing`, not start a
workload with a new seed.

### 8.5 Future portable secret export

If backups must survive total loss of the Farm Vault independently, implement a separate backup
worker rather than granting secret reads to the API AppRole. The worker should:

- receive a short-lived, single-session policy;
- list and read only the requested session prefixes;
- stream values directly through a dedicated Vault Transit encryption key;
- store only authenticated ciphertext in the backup object;
- never write plaintext to disk, logs, database rows, or API responses;
- use a separate restore-only path to decrypt and write a new secret generation;
- revoke its token when the Job completes.

That extension needs its own threat model, key-loss procedure, audit events, and restore drill.

---

## 9. Data model

Names are illustrative; migrations remain the implementation source of truth.

### 9.1 `session_backups`

| Column | Purpose |
| --- | --- |
| `id` | Opaque public backup ID, preferably UUID |
| `setup_session_id` | FK to the retained session/tombstone |
| `user_id` | Authorization and audit copy of the owner |
| `label` | Optional user-visible name |
| `kind` | `manual`, `scheduled`, `archive`, or `pre_restore` |
| `status` | `queued`, `quiescing`, `snapshotting`, `uploading`, `available`, `failed`, `deleting`, `deleted` |
| `manifest_version` | Decoder version for the JSON manifest |
| `manifest` | Allowlisted restore metadata, never plaintext secrets |
| `vault_generation` | Reference to the retained secret generation |
| `expires_at` | Nullable retention deadline |
| `error_code` / `error_message` | Safe failure details |
| timestamps | Creation, start, completion, and deletion times |

The FK must prevent hard deletion of a session while a recoverable backup exists.

### 9.2 `session_backup_volumes`

One row per component:

| Column | Purpose |
| --- | --- |
| `backup_id` / `component` | Unique backup component |
| `pvc_name` | Deterministic claim name required during restore |
| `source_volume` | Original Longhorn volume ID for audit |
| `snapshot_name` | Temporary/local snapshot identifier |
| `backup_url` | Internal durable Longhorn backup URL |
| `size_bytes` | Logical size used for quota and restore validation |
| `checksum` | Provider checksum when available |
| `status` / `error` | Per-volume progress |

### 9.3 `session_restore_operations`

Restore is a repeatable operation, not a mutation of the immutable backup row. Record the selected
backup, status, pre-restore safety backup, restored volume IDs, error, actor, and timestamps.

### 9.4 State and locking

Do not rely only on an in-process mutex. Backup and restore can outlive an API process. Use a
database-backed exclusive session operation or advisory lock plus persisted states. Startup and a
periodic reconciler resume incomplete operations idempotently.

---

## 10. API surface

Exact response envelopes should follow existing setup API conventions.

```text
POST   /api/v1/setup/:id/backups
GET    /api/v1/setup/:id/backups
GET    /api/v1/setup/:id/backups/:backup_id
DELETE /api/v1/setup/:id/backups/:backup_id
POST   /api/v1/setup/:id/backups/:backup_id/restore
POST   /api/v1/setup/:id/stop
POST   /api/v1/setup/:id/start
POST   /api/v1/setup/:id/archive
```

Example create request:

```json
{
  "label": "before-upgrade",
  "stop_after_backup": false
}
```

Example safe list item:

```json
{
  "id": "01K...",
  "label": "before-upgrade",
  "status": "available",
  "mode": "full_stack",
  "component_count": 4,
  "created_at": "2026-10-08T20:00:00Z",
  "completed_at": "2026-10-08T20:03:14Z",
  "expires_at": null
}
```

Create, restore, archive, and purge requests are idempotent by idempotency key. Polling a backup or
restore returns progress without exposing provider URLs or secret paths.

---

## 11. Authorization, audit, and security

- Every user route loads the backup through both `user_id` and `setup_session_id`; another user's
  backup returns `404`, not `403`.
- Admin actions identify both the actor and session owner in the audit log.
- Backup manifests and API logs never contain secret values, Vault tokens, S3 credentials, claim
  codes, or reveal-once credentials.
- Backup IDs are opaque and unguessable.
- The Longhorn backup target uses encryption, private network access, and least-privilege
  credentials. Longhorn owns its backupstore layout and deletion; do not apply bucket lifecycle
  or Object Lock rules directly to that prefix.
- The separate API manifest catalog may use versioning or retention, with documented purge
  semantics and credentials that cannot access tenant secret values.
- Provider backup URLs remain server-side.
- Restore validates manifest schema, component set, size, and checksum before attaching volumes.
- Purge is a high-impact operation and requires typed-name confirmation plus explicit backup
  deletion confirmation.
- All backup, restore, archive, retention, and purge transitions emit durable audit events.

PVC data itself is sensitive even when Vault secrets are separate. It can contain configuration,
DID documents, message data, and application state and must be protected accordingly.

---

## 12. Retention and quotas

Recommended defaults:

- manual backups: retained until deleted, subject to a per-user count quota;
- scheduled backups: fixed count and age policy;
- pre-restore safety backups: short retention, for example seven days;
- failed partial backups: cleaned automatically after diagnostic metadata is retained;
- archived sessions: at least one non-expired available backup is mandatory.

Deleting a backup is asynchronous. The row remains `deleting` until the Longhorn backup is gone.
If it is the last backup of an archived session, the API must require either a successful restore
or explicit permanent purge; it must not leave an archive with no recovery point.

Quota calculations use the provider-reported backup size where available. The API must reserve a
slot before starting work so concurrent requests cannot exceed the quota.

---

## 13. Failure handling and reconciliation

Every external step is idempotent and is followed by an observed-state check. The database state
records intent; Longhorn, Kubernetes, Vault, DNS, and the backup target provide observed state.

The reconciler handles at least:

- API restart while workloads are scaled down;
- one of four full-stack uploads failing;
- completed provider backup before the database update commits;
- deleted source PVC after a completed backup;
- restore volume created but PVC not yet bound;
- DNS or certificate reconciliation failure;
- Vault temporarily sealed or unavailable;
- purge backup deletion succeeding while Vault deletion fails;
- abandoned temporary snapshots and restore volumes.

Cleanup never deletes the only known-good source volume or backup until its replacement is ready
and verified.

---

## 14. Implementation plan

### Phase 1 — Models and read API

- Add backup, volume, restore-operation, and audit models plus migrations.
- Add manifest versioning and allowlisted serialization.
- Add list/detail endpoints and ownership tests.
- Add database-backed session maintenance operations.

### Phase 2 — Backup and stop

- Add Longhorn snapshot/backup client methods and RBAC.
- Implement quiesce, no-write holder Pods, snapshot group, upload, early resume, and
  `stop_after_backup`.
- Add progress reconciliation and retention cleanup.
- Add stop/start without deleting PVCs.
- Add cooldown, per-user concurrency, and cluster-wide capacity enforcement.
- Write, sign, verify, and reconcile the S3 manifest catalog.

### Phase 3 — Restore and archive

- Restore Longhorn backups into new volumes.
- Patch old PVs to `Retain` and implement a verified static-PV volume swap and rollback.
- Rebind the original deterministic PVC names.
- Recreate workloads and reconcile DNS/TLS/external state.
- Add pre-restore safety backups and rollback windows.
- Delete live PVCs only after archive backup verification.

### Phase 4 — Deletion and Vault retention

- Add backup-aware delete conflict and explicit purge.
- Replace account-row cascade deletion with a persisted user purge that waits for every external
  resource cleanup.
- Replace best-effort Vault deletion with persisted, retryable cleanup.
- Retain archived secret generations while revoking unused workload access.
- Add orphan backup, volume, and secret reconciliation.

### Phase 5 — Scheduled backups and UI

- Add schedules, quotas, expiry, protected/manual backups, and storage reporting.
- Add backup selection, restore confirmation, progress, archive, and purge UI.

### Phase 6 — Optional portable encrypted secrets

- Design and review the isolated backup-worker threat model.
- Add Transit-encrypted secret export only if independent Farm Vault loss recovery is required.

---

## 15. Required tests and restore drills

### Automated tests

- `vta_only` backup contains exactly one completed volume.
- `full_stack` becomes available only with all four completed volumes.
- Backup quiesces writes and restarts a previously running session.
- Snapshot creation succeeds through no-write holder Pods and removes every holder afterward.
- A normal backup resumes workloads after snapshots are ready rather than waiting for S3 upload.
- `stop_after_backup` leaves all Deployments at zero.
- Failed upload restarts the session and cannot be selected for restore.
- Cooldown returns `429` and `Retry-After`; retrying an idempotency key returns the original
  operation.
- Restore binds the original PVC names and uses the selected backup, not the newest one.
- Restore patches old PVs to `Retain`, preserves old volumes during rollback, and explicitly
  removes them only after verification.
- Restoring backup A leaves backups B and C unchanged.
- Delete returns conflict while backups exist.
- Account deletion retains user and session tombstones until every external purge completes.
- Purge deletes provider backups before Vault secrets and the tombstone.
- Vault deletion failure retains the tombstone and retries.
- The API never requests Vault secret data during the version 1 backup path.
- Manifest catalog recovery rebuilds a complete one- or four-volume consistency group without
  reading Vault secret values.
- A user cannot enumerate or restore another user's backup.

### Staging drill

For each mode:

1. Create a marker before backup A.
2. Create backup A and wait for `available`.
3. Change the marker and create backup B.
4. Archive the session and confirm live PVCs are gone.
5. Restore backup A explicitly.
6. Confirm the original marker exists and the later change does not.
7. Confirm public identity, DIDs, Vault authentication, and component health.
8. Restore backup B and repeat verification.
9. Purge the test session and confirm backups, Vault prefixes, PVCs, and the tombstone are gone.

A feature is not production-ready until this drill succeeds from the real staging backup target.

---

## 16. Operational dependencies

Per-session recovery depends on all of the following being operated and tested separately:

- PostgreSQL backup containing session tombstones and backup metadata;
- Longhorn backup-target durability and credentials;
- Farm Vault Raft backups;
- Transit Vault availability and retained unseal material;
- downstream-cluster etcd recovery for Kubernetes state;
- container image retention for every image recorded in a manifest;
- DNS and certificate credentials.

Provider/consumer availability is intentionally not enforced by the backup API. Stopping,
archiving, or restoring a full-stack provider may temporarily break sessions that use its DID
hosting or mediator. Those consumers keep their own lifecycle state and recover when the provider
returns; the UI and audit log should make this expected impact visible.

Losing only Kubernetes objects is recoverable from the tombstone, manifest, volumes, and Vault.
Losing the Vault secret generation is not recoverable from a version 1 per-session backup. The UI
and API documentation must state this boundary plainly rather than describing a PVC backup as a
standalone export.
