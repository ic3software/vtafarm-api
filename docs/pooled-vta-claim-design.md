# Pooled VTA Creation and Claim Design

## 1. Product Model

An administrator creates a batch of pooled VTAs for an event. Each VTA is a
normal Farm resource from the beginning:

- it has its own real `users` row and database-generated user ID;
- it has its own `setup_sessions` row;
- its namespace and Vault paths are derived from that user and session;
- its DNS record, DID, PVC, configuration, Service, Ingress, and Deployment are
  prepared before the event;
- its Deployment is prebuilt and waiting at zero replicas, so it uses persistent
  storage but no VTA pod CPU or memory;
- its setup-session status is `pool_waiting` while it is available.

At the station, an administrator selects an available session and displays the
ordinary VTA claim QR. Keyring scans it and shows the existing comparison code.
The administrator confirms the matching code. Confirmation writes the
claimant's admin DID through the existing mobile-connection flow, runs the
ordinary provision operation, scales the prebuilt Deployment to one replica, and
finishes at `running`.

There is no Portal activation link, account transfer, resource-owner transfer,
or new Keyring protocol in this feature. A claimed VTA already belongs to the
user row created for it. Future Portal enrollment is a separate feature.

## 2. Design Rule: Do Not Duplicate Existing State

The pool schema records only batch membership. Existing tables remain the
authoritative source for everything else:

| Concern | Source of truth |
| --- | --- |
| Account and numeric owner | `users` and `setup_sessions.user_id` |
| VTA name, image, DID, DNS record ID, domain and error | `setup_sessions` |
| Namespace | Derived by the Kubernetes client from `setup_sessions.user_id` |
| Vault path | Derived from `user_id` and `setup_session.id` |
| Cloudflare zone and load-balancer IP | Current application configuration |
| QR, scan, claimant DID and confirmation time | `mobile_connections` |
| Creation, waiting, activation, failure and running state | `setup_sessions.status` |
| Pool grouping | `pool_batches` and `pool_sessions` |

Consequently, the pool does not store a second user ID, VTA name, namespace,
Vault target, DNS zone, ingress IP, claimant DID, claim timestamps, cleanup
cursor, or per-VTA lifecycle state. The batch keeps only a nullable timestamp
to make whole-batch cleanup durable across API restarts.

## 3. Schema

```sql
CREATE TABLE pool_batches (
    id bigserial PRIMARY KEY,
    created_by bigint NOT NULL REFERENCES admins(id) ON DELETE RESTRICT,
    label text NOT NULL,
    requested_count integer NOT NULL CHECK (requested_count > 0),
    idempotency_key text NOT NULL,
    request_digest text NOT NULL,
    cleanup_requested_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (created_by, idempotency_key)
);

CREATE TABLE pool_sessions (
    session_id bigint PRIMARY KEY REFERENCES setup_sessions(id) ON DELETE RESTRICT,
    batch_id bigint NOT NULL REFERENCES pool_batches(id) ON DELETE RESTRICT
);

CREATE INDEX pool_sessions_batch_id ON pool_sessions(batch_id);
```

### `pool_batches`

`id` identifies one batch creation request. It is not a user ID range and does
not reserve numeric user IDs.

`created_by` records the administrator that created the batch and scopes the
idempotency key. The foreign key is restrictive so audit attribution cannot be
silently erased.

`label` is the administrator-facing event or batch name. It has no effect on
resource identity.

`requested_count` records the original request. The live number of members is
always counted from `pool_sessions`; it can become smaller after cleanup.
The database constraint validates only that the request is positive. The
cluster's live VTA-only estimate is the actual upper bound.

`idempotency_key` makes a retried HTTP request return the original batch instead
of creating another 50 users and VTAs.

`request_digest` binds the idempotency key to the original request body. Reusing
one key with a different count or configuration is rejected instead of silently
returning an unrelated batch.

`cleanup_requested_at` is null during ordinary use. Whole-batch cleanup sets it
in the same transaction that marks every member as deleting. This is the
durable signal to delete the batch after its final member is removed.

`created_at` is the immutable creation timestamp. There is no general mutable
batch state, so the table needs no `updated_at`.

### `pool_sessions`

`session_id` is both the primary key and a restrictive foreign key. A setup
session can belong to at most one pool batch. Restricting direct deletion forces
cleanup to remove external resources and pool membership deliberately before it
deletes the session and user.

`batch_id` groups sessions for listing, selecting an available VTA, retaining
unused inventory for another event, and bulk cleanup. Its index supports those
operations.

The table intentionally has no synthetic row ID: the setup-session ID already
uniquely identifies the pooled VTA.

## 4. Lifecycle

The setup session owns the lifecycle:

| Session status | Meaning |
| --- | --- |
| `pool_creating` | User, session, and membership committed; preparation is pending |
| Existing setup stages | Namespace, Vault access, PVC, configuration and DID are being prepared |
| `pool_prepared` | DID publication completed; the VTA prebuild is being finalized |
| `pool_waiting` | The VTA is prebuilt and waiting at zero replicas, ready to claim |
| `provisioning` | A confirmed admin DID is being imported and the Deployment is starting |
| `running` | At least one Deployment replica passed readiness |
| `failed` | Preparation or activation failed; claim history determines whether cleanup is allowed |
| `deleting` | Explicit cleanup owns the session and no worker may restart it |

The pool has no parallel state column. A list endpoint derives presentation
state by joining `pool_sessions`, `setup_sessions`, and the latest
`mobile_connections` row.

## 5. Claim Flow

1. Lock pool allocation and select the first session in `pool_waiting` with an
   empty `admin_did`.
2. Create or return its ordinary active `mobile_connections` claim request.
3. Keyring scans the QR and submits its DID through the existing Trust Task.
4. The station displays the existing six-character comparison code.
5. Administrator confirmation sets `mobile_connections.confirmed_at`, copies
   the DID into `setup_sessions.admin_did`, and advances the session to
   `provisioning`.
6. The provision job imports the DID into the prepared VTA data.
7. The existing zero-replica Deployment is scaled to one.
8. Readiness advances the session to `running`, and redeem returns the VTA DID.

The confirmed mobile connection is the durable handover record. A confirmed
activation that later fails remains claimed and cannot be allocated or cleaned
as unused inventory.

## 6. Cleanup and Retention

Unused inventory has no expiry. An administrator may keep it for another event,
explicitly select setup-session IDs for cleanup, or clean up an entirely unused
batch.

Cleanup is allowed only when all of the following remain true while rows are
locked:

- the session is a member of `pool_sessions`;
- the session is `pool_waiting` or is an unclaimed `failed` preparation;
- `setup_sessions.admin_did` is empty;
- no mobile connection is open, claimed, confirmed, provisioning, or released;
- no mobile connection has `confirmed_at` set;
- the user has no email, passkey, or full-stack access.

Cleanup removes the resources identified by the existing session data, then
deletes the `pool_sessions` membership, setup session, and dedicated user. It
never resets PostgreSQL sequences, so deleted numeric IDs are not reused and
later users continue from the database sequence.

Selected cleanup and `DELETE /api/v1/admin/pool-batches/{id}` use the same bulk
admission path. A session still being prepared is rejected immediately instead
of waiting for its worker; the UI disables cleanup until every selected VTA is
either prebuilt and waiting or failed. If any member fails the cleanup rules, no
member is changed. The API marks every accepted session as `deleting` and
returns `202 Accepted` without waiting for external cleanup. That session status
is the durable cleanup request for selected inventory. Whole-batch cleanup
additionally writes `cleanup_requested_at`; after background workers remove the
final member, that marker causes the empty `pool_batches` row to be deleted.

Claimed sessions are not affected by unused-inventory cleanup. They remain
ordinary running Farm sessions managed by an administrator until a separate
Portal enrollment design is implemented.

## 7. Failure and Retry Rules

- Batch creation is transactional for database rows. Either all requested users,
  sessions and memberships are admitted, or none are.
- External preparation is asynchronous and idempotent per setup session.
- A failure before confirmation can be retried or explicitly cleaned.
- A failure after confirmation can be retried, but never returned to inventory.
- Two station requests cannot allocate the same session: allocation serializes
  pool mutations and locks the chosen setup session.
- Deployment creation at zero replicas is idempotent. Claim activation scales
  that existing Deployment instead of creating a second one.

## 8. Capacity Admission

Batch admission uses the same strict VTA-only estimate shown by the Admin
Dashboard. A request is rejected before any batch, user, or setup-session row
is inserted when `requested_count` exceeds that estimate. For example, when
the Dashboard reports 47 additional VTA-only sessions, a request for 48 is
rejected and a request for 47 is allowed.

Capacity is checked after an idempotency lookup, so retrying a successful
request returns its original batch even if current capacity has fallen. New
admissions first collect one cluster snapshot outside the pool transaction.
The independent Kubernetes node/pod, metrics-server, and Longhorn reads run in
parallel. After the admission lock is acquired, a database query subtracts the
current non-running pool reservations from that snapshot. This prevents two
administrators from consuming the same capacity without holding the lock during
slow cluster calls. Capacity lookup fails closed when the Dashboard cannot
produce its estimate.

The accepted users, setup sessions, memberships, and workload-resource rows are
inserted in four bulk statements inside one transaction. Public account and VTA
identifiers are generated before their corresponding insert; a uniqueness
collision causes only the missing identifiers to be regenerated and inserted.
The committed setup-session rows supply the background runner IDs and the
`201 Created` representation, avoiding a batch lookup and detail reload on the
synchronous creation path.

Non-running pool members are deducted from the Dashboard's VTA-only estimate.
This reserves their future CPU and memory while their Deployments are prebuilt
and waiting at zero replicas; Kubernetes pod requests alone cannot represent
that future load. The reservation remains until the session is running, when
the live Deployment requests become part of cluster statistics.

There is intentionally no fixed product limit such as 50. The positive-value
database check protects data shape; current cluster resources decide the
operational limit.

Batch-detail polling uses one joined query for setup sessions and their latest
claim. It interprets expired open and claimed records as lapsed without issuing
expiry updates from a GET request. The browser schedules its next poll only
after the previous request finishes, so a slow response cannot accumulate
overlapping requests.

## 9. Migration Safety

Migration 0043 contains the two pool tables and whole-batch cleanup marker
described above. The earlier ownership design and its `users.account_kind` and
`setup_sessions.portal_user_id` columns were rolled back and are not part of
this schema.
