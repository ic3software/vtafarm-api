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

## 2. Non-goals

- A per-session backup is not a replacement for PostgreSQL, Vault Raft, cluster etcd, or Longhorn
  backup-target disaster recovery.
- A backup is not a container image. Images remain immutable external artifacts and their exact
  references are recorded in the backup manifest.
- Version 1 does not clone a backup into a different user or a new logical session.
- Version 1 does not export Vault plaintext into S3.
- Version 1 does not promise a live, application-consistent snapshot without stopping writes.

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

The recoverable Vault prefixes are:

```text
vta/user-<user-id>/session-<session-id>/...
mediator/user-<user-id>/session-<session-id>/...
dids/user-<user-id>/session-<session-id>/...
vtc/user-<user-id>/session-<session-id>/...
```

Only the VTA prefix is required for `vta_only`; all four are required for `full_stack`.

The PVC snapshot cannot contain these values because the components read them from Vault at
runtime. The first implementation must therefore retain the original Vault trees while a backup
is recoverable. It must not copy secret values into the backup manifest.

### 3.3 Restore manifest

The manifest is an allowlisted snapshot of the configuration needed to recreate workloads. It
includes:

- session ID, owner ID, mode, domain type, public names, and FQDNs;
- exact component image references;
- PVC names, requested sizes, source volume IDs, and durable Longhorn backup URLs;
- component resource profiles;
- DID and DID-hosting identifiers needed to reconcile external state;
- Vault prefix names and a secret-generation identifier, never secret values;
- manifest schema version and checksums;
- creation time and the application/API version that created the backup.

It must exclude reveal-once credentials, private keys, claim codes, Vault tokens, Kubernetes
credentials, Cloudflare credentials, and S3 credentials. An internal Longhorn backup URL is not
returned to a normal user; the API exposes only backup IDs and safe metadata.

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
  -> snapshot every PVC
  -> upload every snapshot to the Longhorn backup target
  -> record and verify every backup URL
  -> optionally remove live PVCs and leave the session stopped
```

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

### 6.2 Consistent backup sequence

1. Create a `session_backups` row in `queued` state.
2. Acquire the exclusive session maintenance lock in the database.
3. Move the backup to `quiescing` and reject new maintenance operations.
4. Block new external writes, then gracefully scale component Deployments to zero.
5. Wait for all pods to terminate and all RWO PVCs to detach.
6. Capture the allowlisted restore manifest and Vault prefix references.
7. Create Longhorn snapshots for every PVC.
8. Create Longhorn backups from those snapshots and wait for every backup to complete.
9. Persist backup URLs, sizes, checksums, and completion timestamps transactionally.
10. Mark the backup `available` only when the complete consistency group is durable.
11. For a normal backup, recreate or scale up Deployments and wait for readiness.
12. For `stop_after_backup`, leave Deployments at zero.
13. Release the maintenance lock.

Stopping all components before the first snapshot makes sequential snapshots a consistent group:
no component can modify its PVC while another component is being captured. For `full_stack`, stop
clients first and start dependencies first:

```text
stop:  VTC -> VTA -> Mediator -> DID hosting
start: DID hosting -> Mediator -> VTA -> VTC
```

If quiescing or backup upload fails, restart a previously running session and mark the backup
`failed`. A failed partial group must never be selectable for restore.

### 6.3 Archive sequence

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
6. For each component, create a new Longhorn volume from the selected backup URL.
7. Wait until every restored volume is ready and detached.
8. Bind PVs and PVCs using the original deterministic PVC names.
9. Recreate Services, Ingresses, certificates, middleware, and Deployments from the tombstone and
   manifest.
10. Reconcile DNS and required DID-hosting state.
11. Start components in dependency order and wait for readiness.
12. Run restore verification checks.
13. Mark the restore complete and the session `running`.
14. Retain replaced volumes for a short rollback window, then remove them asynchronously.

Restore must create new volumes; it must not attempt to overwrite a mounted Longhorn volume in
place. The selected durable backup remains immutable and may be restored again.

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

Version 1 therefore treats the retained Vault session trees as a dependency of every backup:

```text
secret tree may be deleted only when:
  no live session uses it
  AND no queued/creating/available/restoring backup references it
  AND no restore operation can still roll back to it
```

The backup row records prefix names and a generation identifier, not secret values. The restored
workloads use the same session ID and therefore the same Vault paths.

### 8.2 Secret immutability requirement

This approach requires session secrets to remain compatible with all retained backups. Today the
master seed and setup-generated component keys are treated as immutable after provisioning.

Any future secret-rotation feature must do one of the following before it ships:

- keep the old secret generation at a versioned prefix until all backups that reference it expire;
- create a new backup after rotation and invalidate older incompatible recovery points; or
- implement the encrypted secret export described in section 8.5.

It must not overwrite a secret generation still referenced by a backup.

### 8.3 Reference counting and final cleanup

Vault cleanup must be reconciliation-driven, not best-effort fire-and-forget:

1. Mark the session tombstone `purging`.
2. Delete all Longhorn backup objects and confirm removal from the backup target.
3. Confirm that no backup or restore row references the session secret generation.
4. Recursively delete the VTA, mediator, DIDs, and VTC Vault trees as applicable.
5. Remove per-user Vault access only if the user has no live or archived sessions.
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
- The Longhorn backup target uses encryption, private network access, least-privilege credentials,
  object retention, and lifecycle rules.
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
- Implement quiesce, snapshot group, upload, resume, and `stop_after_backup`.
- Add progress reconciliation and retention cleanup.
- Add stop/start without deleting PVCs.

### Phase 3 — Restore and archive

- Restore Longhorn backups into new volumes.
- Rebind the original deterministic PVC names.
- Recreate workloads and reconcile DNS/TLS/external state.
- Add pre-restore safety backups and rollback windows.
- Delete live PVCs only after archive backup verification.

### Phase 4 — Deletion and Vault retention

- Add backup-aware delete conflict and explicit purge.
- Replace best-effort Vault deletion with persisted, retryable cleanup.
- Keep per-user Vault access while any archived session exists.
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
- `stop_after_backup` leaves all Deployments at zero.
- Failed upload restarts the session and cannot be selected for restore.
- Restore binds the original PVC names and uses the selected backup, not the newest one.
- Restoring backup A leaves backups B and C unchanged.
- Delete returns conflict while backups exist.
- Purge deletes provider backups before Vault secrets and the tombstone.
- Vault deletion failure retains the tombstone and retries.
- The API never requests Vault secret data during the version 1 backup path.
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

Losing only Kubernetes objects is recoverable from the tombstone, manifest, volumes, and Vault.
Losing the Vault secret generation is not recoverable from a version 1 per-session backup. The UI
and API documentation must state this boundary plainly rather than describing a PVC backup as a
standalone export.
