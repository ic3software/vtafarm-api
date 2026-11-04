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
Vault target, DNS record, ingress IP, claimant DID, claim timestamps, cleanup
cursor, or per-VTA lifecycle state. The batch keeps a non-secret replenishment
image plus a nullable timestamp that makes whole-batch cleanup durable across
API restarts.

## 3. Schema

```sql
CREATE TABLE pool_batches (
    id bigserial PRIMARY KEY,
    created_by bigint NOT NULL REFERENCES admins(id) ON DELETE RESTRICT,
    label text NOT NULL,
    requested_count integer NOT NULL CHECK (requested_count > 0),
    vta_image text NOT NULL CHECK (vta_image <> ''),
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

`vta_image` is the only replenishment input retained on the batch. Mode,
portability and pre-rotation are fixed for pooled VTAs; managed-domain and
shared DID-hosting inputs come from the current platform configuration when
inventory is added. This allows selected cleanup to remove the final member
while keeping the empty batch usable without retaining stale infrastructure
endpoints. Migration 49 backfills the image from the oldest member of each
existing batch and deletes legacy batches that already have no `pool_sessions`
members.

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
4. The Admin claim station or public user page displays the existing
   six-character comparison code.
5. Confirmation from either entry point sets
   `mobile_connections.confirmed_at`, copies the DID into
   `setup_sessions.admin_did`, and advances the session to `provisioning`.
6. The existing zero-replica Deployment starts with a one-shot activation init
   container that imports the DID into the prepared VTA data.
7. The same Pod starts the VTA application after activation succeeds.
8. Readiness advances the session to `running`, and redeem returns the VTA DID.

The confirmed mobile connection is the durable handover record. A confirmed
activation that later fails remains claimed and cannot be allocated or cleaned
as unused inventory.

## 6. Fast Activation for All Pool Claims

Fast activation is a property of pooled VTA inventory, not of one UI. Both
entry points use it:

| Entry point | Confirmation route | Shared activation |
| --- | --- | --- |
| Admin claim station | `POST /api/v1/admin/pool-claims/:request_id/confirm` | Confirm the mobile connection, then kick the pooled activation worker |
| Public user flow | `POST /api/v1/public/pool-claims/:request_id/confirm` | Authorize the browser capability, confirm the same mobile connection, then kick the same pooled activation worker |

Allocation, confirmation, recovery, failure handling, and release must not fork
by entry point. Once a pooled connection is confirmed, the worker operates only
from durable session and connection state. It does not need to know which UI
confirmed it.

### 6.1 Decision

Do not create a per-claim Kubernetes provision Job for a pooled VTA. After
confirmation, patch the already-created zero-replica Deployment with a one-shot
activation init container and set its replica count to one in the same
Kubernetes update. The resulting Pod imports the claimant's administrator DID,
adds the DID-hosting control server when required, and then starts the ordinary
VTA container.

The external claim protocol and APIs remain unchanged. A VTA is still assigned
atomically from `pool_waiting`, Keyring still proves possession of the proposed
administrator key, and the person at either UI still confirms the comparison
value before the Farm mutates the VTA. The optimization changes only how a
confirmed pooled VTA crosses `provisioning -> running`.

This removes one Pod scheduling/startup cycle and the provision Job's
five-second completion poll without keeping every unclaimed VTA running.

### 6.2 Current latency and measurement

Pool preparation already creates the PVC, VTA configuration and DID, publishes
the DID, and creates the Service, Ingress, and final Deployment. The Deployment
has zero replicas while the session is `pool_waiting`.

Before fast activation, the implementation took this path after either UI
confirmed:

```text
confirm claim
  -> commit admin_did and provisioning state
  -> create vta-provision-{sessionID} Job
  -> schedule and start a Job Pod
  -> run vta import-did
  -> optionally run vta did-mgmt servers add
  -> observe Job completion (polled every 5 seconds)
  -> scale the parked Deployment to one
  -> schedule and start a second Pod
  -> wait for /health readiness
  -> mark running and release the claim
```

The confirmation request does not wait synchronously for this work, but Keyring
and the browser cannot receive the VTA until it finishes. The fixed avoidable
costs are the separate Job Pod lifecycle and up to one `WaitForJob` polling
interval. Image pull, volume attachment, scheduling, and VTA initialization
determine the remaining time.

The fast-activation worker emits safe stage timings. During canary validation,
collect at least 30 activations on the target cluster and include both Admin and
public claims, because they converge on the same worker. Record p50, p95, and
maximum for:

- confirmation commit to provision Job Pod started;
- CLI start to provision Job completion;
- Job completion to Deployment Pod started;
- Deployment Pod started to readiness;
- confirmation commit to claim release.

The product target is p95 confirmation-to-release below 15 seconds with a warm
image cache, and at least a 40 percent p95 reduction from the measured baseline.
These are acceptance targets, not an assertion that every cluster can meet
them; a cold image pull remains outside this change.

### 6.3 Activation path

Both claim entry points converge on this path:

```text
confirm pooled claim
  -> atomically commit admin_did and provisioning state
  -> ensure the VTA DID has its hosting ACL entry
  -> patch the parked Deployment template and replicas=1 together
       init: import administrator DID
       init: add DID-hosting control server when configured
       app:  start vta
  -> wait for /health readiness
  -> mark running and release the claim
```

The existing provision advisory lock remains the single cross-replica owner for
activation. The database transaction remains the durable handover boundary;
Kubernetes work starts only after it commits.

The Deployment patch must update both `spec.template` and `spec.replicas` in one
API call. This prevents a replica from starting with the old template between a
separate template update and scale operation. Use optimistic concurrency or an
idempotent server-side patch and verify the resulting template carries the
expected activation identity.

The init container uses the same pinned VTA image, service account, working
directory, and `/work/vta` PVC as the application container. Its command is the
current provision command:

```text
vta import-did --did <adminDid> --role admin --label "mobile integration"
vta did-mgmt servers add --id control --did <controlDid> \
  --label "DID Hosting Control Plane"  # managed hosting only
```

No private key is passed to Kubernetes. The administrator DID and hosting
control DID are public identifiers and already appear in the current Job spec.
Shell arguments must continue to use the existing quoting helper.

Store an activation identity in Deployment annotations. Derive it from the
session and confirmed connection, not from a secret, so recovery can
distinguish the intended claim from a stale template. Never put the browser
capability, claim handle, comparison value, or claimant private material in an
annotation, environment variable, command line, event, or log.

### 6.4 Durable step receipts

An init container runs again whenever Kubernetes recreates the Pod. It cannot
blindly repeat mutating CLI commands. Write per-step receipts on the session PVC
under a Farm-owned directory after each command succeeds:

```text
.vtafarm/activation/<activation-id>/admin-imported
.vtafarm/activation/<activation-id>/control-server-added
```

On restart, validate that a receipt belongs to the same activation identity and
skip only that completed step. Write each receipt through a temporary file plus
atomic rename. Never treat the presence of the directory alone as success.

There is still a narrow crash window between a CLI mutation and receipt write.
Before shipping, establish one of these recovery contracts:

1. prove both CLI operations are idempotent for identical arguments;
2. add a read-before-write check using supported VTA commands; or
3. change the VTA CLI to expose an idempotent `ensure` operation.

Do not ship a marker-only workaround if repeating either command can corrupt or
reject a valid activation. This is the principal technical prerequisite.

### 6.5 State, recovery, and failure handling

No schema or claim/confirm API change is required. The existing Admin pool
retry route also resumes failed confirmed activations. `setup_sessions`,
`mobile_connections`, and the existing statuses remain the source of truth:

| State | Fast-activation behavior |
| --- | --- |
| `pool_waiting` | Deployment exists at zero replicas with no claim-specific activation |
| `confirmed` / `provisioning` | Existing worker lock patches or reconciles activation and waits for readiness |
| `running` / `released` | At least one VTA replica is ready; no activation work is repeated |
| `failed` after confirmation | VTA remains consumed; operator retry resumes the same activation identity |
| `deleting` | Heartbeat cancellation stops reconciliation and cleanup owns the resources |

Recovery is reconcile-based rather than create-once:

1. Load the confirmed connection and session under the existing provision
   worker lock.
2. Ensure the hosting ACL operation is complete; its current API call remains
   idempotent or must be treated as such explicitly.
3. Read the Deployment.
4. If its activation annotation or init container does not match, patch the
   expected template and replica count.
5. If it matches but replicas are zero, scale it to one.
6. Inspect Pod init status and Deployment readiness until success or a bounded
   failure.
7. On readiness, perform the existing database transition to `running` and
   mark the mobile connection `released`.

Do not wait for the full two-minute readiness timeout when the activation init
container has terminated with a non-zero exit code. Capture its bounded logs,
record a safe error on the session, and use the existing post-confirmation
failure behavior. Never copy command lines containing identifiers into a public
error response.

A process crash after Kubernetes becomes ready but before the database update
is safe: recovery observes the ready Deployment and completes `running` and
`released`. A crash before the patch is safe because the committed
`provisioning` row is found by the existing recovery queue.

### 6.6 Reservation and authorization boundary

Selecting **Get my VTA** creates a temporary, reclaimable reservation. Do not
start a Pod for this initial browser action: many reservations may be abandoned
before Keyring participates.

The later Keyring submission changes the latency trade-off. Once Keyring has
submitted the proposed administrator DID and both sides can display the
comparison value, the claim is active and the user normally spends several
seconds verifying the code. A future prewarm path may use that interval to
schedule the selected VTA Pod and attach its PVC. It must not import the
administrator DID, add the hosting control server, start the VTA application,
or otherwise mutate the prepared VTA before matching-code confirmation. A
signed claim plus that explicit confirmation remains the authorization boundary
for every claimant-controlled write. The Admin station and public flow follow
the same rule.

Do not use `exec` into a running VTA, mount the PVC into concurrent writer Pods,
or pre-import a Farm-controlled placeholder administrator.

### 6.7 Confirmation-latency prewarm proposal

This is a proposed follow-up to fast activation, not part of the currently
implemented path. Removing the provision Job leaves one unavoidable Deployment
Pod startup. A warm-image measurement on the target development cluster still
spent about ten seconds attaching its Longhorn `ReadWriteOnce` PVC. Polling and
database work add time around that operation. Therefore a design that begins
Pod scheduling only after confirmation cannot reliably meet a sub-ten-second
confirmation-to-release target.

One instrumented public claim took 30.773 seconds from clicking **Codes match**
until the third browser step appeared. The mutually exclusive intervals were:

| Interval | Duration | Work observed |
| --- | ---: | --- |
| Click to durable `confirmed_at` | 3.568 s | Trust Task validation and confirmation transaction |
| Confirmation to provision runner start | 3.494 s | Confirmation completion, worker dispatch, and runner handoff |
| Runner start to fast-path entry | 4.961 s | Session reads and updates, DID-hosting ACL, pool detection, and confirmed-claim load |
| Fast-path entry to Deployment patch | 0.346 s | Patch the activation template and set `replicas=1` |
| Deployment patch to observed init start | 12.341 s | Pod scheduling, approximately 10 s of Longhorn volume attachment, kubelet container start, and readiness polling |
| Init start to init completion | 1.001 s | Administrator activation; the one-second observer interval bounds the recorded value |
| Init completion to Deployment ready | 1.001 s | Application start, health readiness, and observer interval |
| Deployment ready to release log | 3.285 s | Mark the session running, release the connection, and create the wallet login binding |
| Release log to third browser step | 0.776 s | Claim polling response and browser render |

The database recorded 25.153 seconds from `confirmed_at` to `released_at`; that
is a different measurement boundary from the browser's 30.773 seconds. Local
database forwarding inflated individual SQL calls in this sample, so the table
is diagnostic evidence rather than a production latency baseline. The volume
attachment and Kubernetes intervals remain relevant to the prewarm decision.

Prewarm only the VTA with an active Keyring claim; do not keep the whole pool
running:

```text
Get my VTA
  -> reserve one pool member; keep Deployment at replicas=0
Keyring submits its DID and the comparison value is displayed
  -> start the selected Deployment Pod
  -> attach the existing PVC and wait at a non-mutating activation gate
Codes match
  -> durably record confirmed_at and admin_did
  -> release the gate in the same Pod
  -> import the administrator DID and add the hosting control server
  -> start the VTA application and wait for readiness
  -> mark running and release the claim
```

The confirmation signal must not recreate the Pod, because doing so would lose
the scheduling and volume-attachment benefit. The preferred shape is a bounded
activation-gate init process in the final Deployment Pod. It authenticates as
the Pod's service account to an internal Farm endpoint and watches or polls for
durable authorization derived from `mobile_connections.confirmed_at`. It must
not trust a browser-provided flag. After authorization, the same process loads
the confirmed public identifiers, performs the existing receipt-protected
activation, and exits so the application container can start.

The internal gate protocol requires a focused implementation spike before this
proposal is accepted. It must provide service-account authentication, bounded
waiting, cancellation, and a way to distinguish a stale claim from the active
claim without putting the browser capability, comparison value, or private
material in Kubernetes objects or logs. A Kubernetes object update that takes
longer to project into the Pod is not suitable for the latency target.

No new pool-level feature flag is required. Prewarm applies to Admin and public
claims under the same conditions:

- the session is a compatible pooled `vta_only` session with a parked
  Deployment;
- Keyring has submitted a DID and the claim is in the comparison stage;
- the claim is unexpired and is the only active claim for the session;
- cluster admission still has room for the Pod;
- the Pod uses the final VTA image, PVC, service account, and resource requests.

The claim and setup-session rows remain the source of truth; a new persistent
pool state is not required. The activation identity binds the waiting Pod to
the active connection. If confirmation arrives before prewarm is ready, the
same Pod continues through the gate as soon as it starts, so the current fast
activation behavior remains the slow-path outcome without creating a Job or a
second PVC writer.

An open or claimed request that expires, is cancelled, or is superseded must
cancel the gate, scale the Deployment back to zero, and restore its neutral
parked template. Because the gate has not mutated the VTA, that session may
return to inventory after the ordinary claim-expiry checks. Confirmation wins
the race through the existing database lock: after `confirmed_at` commits, the
session is consumed and must never be reclaimed even if activation fails.

Prewarm consumes a Pod slot, an attached volume, and the gate container's
resource requests while the comparison screen is open. Capacity accounting
already reserves the eventual VTA resources, but rollout must also enforce a
bounded number of concurrent prewarm Pods so Pod-count and storage-attachment
limits cannot be exhausted by abandoned claims.

Instrument `claim_did_submitted`, `prewarm_pod_scheduled`,
`prewarm_volume_attached`, `prewarm_gate_ready`, `claim_confirmed`,
`activation_init_completed`, and `claim_released`. Report prewarm hit rate,
expired-prewarm rate, confirmation-to-release p50/p95, and the percentage of
confirmations that arrive before the gate is ready. The acceptance target is
p95 confirmation-to-release below ten seconds for prewarm hits with a warm
image cache. Compare at least 30 Admin and public activations using the same
start and end events.

### 6.8 Compatibility and rollout

Apply the fast path whenever all of these are true:

- the session belongs to `pool_sessions`, regardless of whether the claim came
  from the Admin station or public user flow;
- its mode is `vta_only`;
- it was prepared with a parked Deployment compatible with activation init;
- the confirmed connection operation is initial VTA provisioning;
- the image has passed the activation idempotency compatibility check.

Ordinary non-pool `vta_only`, `full_stack`, additional-admin ACL grants, and
maintenance operations keep their existing offline Job flows. This keeps the
first implementation scoped to the pool without changing full-stack
sequencing.

Fast activation is the only activation path for a newly confirmed pooled VTA;
there is no per-pool or environment switch. Recovery continues the legacy Job
path only when that Job already exists, because crossing from an in-progress
Job to Deployment activation could create two PVC writers. Do not otherwise
fall back after fast activation has begun. A failed activation requires
reconciliation or an operator retry of the same path.

### 6.9 Observability

Emit structured, identifier-safe stage timings for `claim_confirmed`,
`hosting_acl_ready`, `deployment_activation_patched`,
`activation_init_started`, `activation_init_completed`, `deployment_ready`, and
`claim_released`.

Include the entry-point kind (`admin_station` or `public_signup`), session ID,
activation identity, image digest when available, node, outcome, and duration.
Do not include the claim handle, public browser token, comparison value, or
administrator DID. Compare latency and failure rates by entry point, while
keeping the worker and success criteria identical.

### 6.10 Implementation and acceptance

1. Instrument the current Job-based path and record both entry-point baselines.
2. Verify or add idempotent VTA CLI behavior for administrator import and
   control-server registration; add crash-window tests.
3. Add a Kubernetes helper that atomically patches the parked Deployment with
   the activation init container, annotations, and `replicas=1`.
4. Add activation observation that distinguishes init failure from application
   readiness failure and returns bounded diagnostic logs.
5. Route every confirmed pool session through the pooled activation worker;
   retain the existing advisory lock, hosting ACL step, status transitions, and
   release behavior.
6. Add recovery and concurrency tests for both Admin and public confirmation.
7. Run canary pools and compare p50/p95 with the baseline before broad rollout.

Acceptance requires that successful Admin and public pooled claims create no
provision Job; one Deployment Pod activates and serves the VTA; repeated worker
kicks, API or Pod restarts, and duplicate confirmations do not repeat completed
mutations or release twice; no concurrent writer touches the PVC; and a failed
confirmed activation never returns to available inventory.

## 7. Cleanup and Retention

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

Selected cleanup can remove every member without deleting the batch. Because
the image lives on `pool_batches`, **Add inventory** continues to work when the
member list and live total are both empty.

Claimed sessions are not affected by unused-inventory cleanup. They remain
ordinary running Farm sessions managed by an administrator until a separate
Portal enrollment design is implemented.

## 8. Failure and Retry Rules

- Batch creation is transactional for database rows. Either all requested users,
  sessions and memberships are admitted, or none are.
- External preparation is asynchronous and idempotent per setup session.
- A failure before confirmation can be retried or explicitly cleaned.
- A failure after confirmation can be retried, but never returned to inventory.
- Two station requests cannot allocate the same session: allocation serializes
  pool mutations and locks the chosen setup session.
- Deployment creation at zero replicas is idempotent. Claim activation scales
  that existing Deployment instead of creating a second one.

## 9. Capacity Admission

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

## 10. Migration Safety

Migration 0043 contains the two pool tables and whole-batch cleanup marker
described above. The earlier ownership design and its `users.account_kind` and
`setup_sessions.portal_user_id` columns were rolled back and are not part of
this schema.
