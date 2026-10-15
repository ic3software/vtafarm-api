# Pooled VTA: Event Pre-provisioning and Claim Design

- Status: Draft for implementation. Event flow, retention, and permanent per-slot infrastructure ownership are confirmed; the Portal binding wire contract remains proposed below.
- Date: 2026-10-14.
- Goal: Prepare VTAs before an event so attendees can claim them through Keyring with minimal waiting and no mandatory registration.
- Reference: [Pooled VTA Claim Flow](https://claude.ai/artifact/5JGCqdwwWjT5Pc11ugp3rV), reviewed against this repository. The page references `~/Projects/one-step-vta.md`; that file was unavailable at the stated local path and has not been reviewed.
- Existing contracts: [Mobile Integration](mobile-connection-app-api.md) and [VTA Claim Design](vta-claim-implementation-design.md). New endpoints, tables, and policies below are proposals, not existing API behavior.

## 1. Confirmed Product Requirements

The prototype parks each `vta_only` slot at `vta_setup_complete`: its seed, PVC, configuration, DNS, and published DID are prepared, but no VTA Deployment exists. The attendee confirms the claim before the Farm imports the administrator DID and starts the VTA.

The product owner confirmed this parked model. There is no idle VTA Deployment and therefore no per-slot running VTA CPU/memory request. PVCs and other persistent resources remain; build Jobs and shared infrastructure still consume compute. After confirmation, DID-host ACL setup, the import Job, Deployment startup, and readiness checks still take time.

The primary event flow is a **staff-operated station**: an admin displays a claim QR on a computer, the attendee scans it with Keyring, and the admin compares the attendee's check with the station's check before confirming. A fresh QR is used for the next attendee.

The intended operating rule is one VTA per attendee, supervised by staff. A phone or wallet is not a unique-person credential: Keyring can create fresh keys, so this is not a cryptographic one-person limit.

After the event, admins can batch-clean unused inventory. Successfully claimed VTAs remain running and are excluded from that cleanup, even if their holders never enroll in the Portal. Holders can later prove ownership and attach their VTA to a Farm Portal account. Until then, Farm admins manage the deployment.

Each pre-built VTA gets its own internal resource owner immediately. Create its namespace, PVC, and Vault secrets under that owner from the first build; keep these locations unchanged through claim and later Portal attachment. Do not stage attendee resources under the platform user, migrate secrets at claim time, or turn the resource owner into a login account. The platform's mediator and DID-hosting services remain shared.

## 2. Proposed Initial Scope

- Support `vta_only` using a permanent Farm-managed domain and the existing platform stack's mediator and DID host. Give each slot its own owner, namespace, and seed.
- Let admins pre-build and inspect the pool, configure an event allocation and quota, open a staffed station, and disable further distribution.
- At the station, the admin selects Next attendee to reserve one slot. No email, Farm account, or passkey is required on the attendee's phone.
- Keyring claims the displayed QR. The station hides the QR after the first successful claim and displays the comparison check; the admin confirms or rejects it.
- Reuse existing claim/redeem Trust Tasks unchanged. Later Portal ownership verification is a separate, explicitly scoped ceremony requiring Keyring integration.
- Initially allocate ready slots only and show retryable unavailability when empty. A self-service event-link page and FIFO waiting queue are deferred; neither is necessary for the staffed-station flow.
- Retain handed-over VTAs after the event and provide both new-account enrollment and existing-account linking for Portal management.

Full-stack pools, custom domains, post-claim DID-hostname changes, and unbounded automatic expansion are outside the proposed initial scope. This document does not implement the feature.

## 3. Existing Implementation and Gaps

| Source | Reusable behavior | Required pool work |
| --- | --- | --- |
| [`internal/setup/orchestrator.go`](../internal/setup/orchestrator.go) | VTA-only setup, DID publication, administrator import, Deployment readiness | Explicit pool readiness, build recovery, bounded batch concurrency |
| [`internal/connection/store.go`](../internal/connection/store.go) | Five-minute QR and confirmation windows, claim checks, session locks, claim/confirm/reject transitions | Atomic coordination with event quotas, slot ownership, and reclamation |
| [`internal/handler/mobile_connection.go`](../internal/handler/mobile_connection.go) | Trigger links and browser state views | Existing authorization requires the signed-in owner's `user_id`; station operations require admin authority instead |
| [`internal/handler/trust_task.go`](../internal/handler/trust_task.go), [`internal/trusttask`](../internal/trusttask) | Wallet proof validation, signed replies, replay handling, redeem long polling | Pool transaction integration, failure handling, and handover protection |
| [`internal/setup/initial_provision.go`](../internal/setup/initial_provision.go) | Persistent provisioning state, cross-replica advisory locks, cancellation during deletion | Recover confirmed claims before provisioning is queued; persist pool build/reaper work independently of process memory |
| [`internal/handler/capacity_service.go`](../internal/handler/capacity_service.go) | Resource estimates | Account for parked stock, concurrent activation, and retained VTAs; estimates do not reserve capacity atomically |
| [Provisioning Load Test](vta-provisioning-load-test.md) | Batch and cleanup patterns | Load tests share a system owner and use an administrator key that cannot be handed over; their sessions are not attendee inventory |
| [`internal/model/user.go`](../internal/model/user.go), [`internal/handler/user.go`](../internal/handler/user.go) | Auto-increment user IDs and an admin `system` flag | Add explicit account classification; current system detection only recognizes platform/load-test names; separate human counts from resource-owner counts |
| [`internal/handler/setup.go`](../internal/handler/setup.go), [`migrations/000003_create_setup_sessions.up.sql`](../migrations/000003_create_setup_sessions.up.sql) | Session teardown and user FK | External teardown is partly best-effort; deleting a user cascades session rows without cleaning external resources; owner removal must be guarded and last |
| [`internal/handler/signup.go`](../internal/handler/signup.go), [`internal/handler/passkey.go`](../internal/handler/passkey.go) | Passkey login/enrollment and user accounts | Email is unverified and signup resumes accounts without passkeys; pooled ownership must never be exposed through that email-only resume path |

Two implementation gaps directly affect the prototype:

1. **`vta_setup_complete` does not prove claim readiness.** The orchestrator writes this status before publishing the DID. Its comments identify a crash window that can leave the DID unpublished. Set `pool_slots.ready` only after publication and DID-resolution checks succeed, with recoverable publication or quarantine after interruption. On a publication conflict, verify the existing document and ownership; neither overwrite blindly nor treat every conflict as success.
2. **The current claim flow has no first-DIDComm-connection evidence.** An older migration contains `connected_at`, but the current `MobileConnection` model and claim/redeem flow do not maintain it. `released` means provisioning completed, not that the wallet received the response or connected over DIDComm. The prototype's cleanup rule cannot simply query `connected_at`.

The prototype also includes `didLogUrl` in its success response. Current `redeemOutcome` returns the handle, VTA DID, operation, and optional mediator DID. Follow the generated protocol schema; do not add response fields without confirming protocol and Keyring support.

## 4. QR Codes, Station Flow, and Competing Scans

```text
Admin pre-builds -> slot ready -> admin opens event station
  -> Next attendee -> reserve one slot -> display a fresh trigger QR
  -> attendee's Keyring claims -> admin compares and confirms
  -> administrator import and VTA startup -> redeem succeeds -> Keyring uses VTA
  -> admin selects Next attendee -> different slot and fresh QR
```

### What the Prototype Says, and What Changes

| QR or link | Reuse | Meaning |
| --- | --- | --- |
| Prototype event link | Shared across attendees | Opens a Farm browser page, which creates a separate reservation and trigger link for each attendee |
| VTA trigger QR | One reservation and one winning claimant | Contains a fresh `_id` handle and expiry; this is what Keyring opens |
| Proposed station page | Same computer/page across attendees | Admin-controlled UI; each Next attendee action produces a new reservation and trigger QR |

The prototype does not give everyone the same reusable VTA-claim QR. The requested station replaces its attendee-browser confirmation with staff confirmation while retaining the per-reservation trigger link. A static event-link poster is not the station's claim QR, and the existing VTA-claim flow must never be reused to add successive attendees to an already-running VTA through `grant_acl`.

Two phones can scan a visible QR, but the atomic claim transition admits only the first valid claimant. The other receives `alreadyClaimed`. The first scan has only locked an empty slot: it has not installed a key or received the VTA. Staff compare the check on the intended attendee's phone with the station. On mismatch or an unexpected claimant, staff reject and issue a new handle. Never confirm because someone merely scanned successfully.

Once confirmed, that slot is permanently removed from allocation. A later attendee gets another slot and handle. Reject/expiry before confirmation may safely reuse the untouched slot, but never the old handle. QR photographs can cause a temporary reservation race, not a second successful handover; the comparison and staff confirmation are essential.

### Station Authorization and Continuity

All station mutations require the existing admin login, event/station scope, Origin validation, and CSRF protection. The event ID, reservation ID, QR handle, wallet proof, and attendee email cannot authorize station confirmation. The attendee-facing display contains only the QR, expiry, progress, and comparison code; it never exposes admin credentials or creates a Portal login on the shared computer.

Persist the station's current reservation and display generation. Refreshing resumes it; concurrent Next attendee clicks are idempotent. Confirm/reject requests name the reservation and expected generation, preventing a stale tab from confirming the next attendee. Allow at most one unconfirmed reservation per station. After confirmation commits, staff can move to the next attendee while the previous activation continues; previous work remains visible in event history and Keyring continues redeem polling. Do not rotate a claimed QR automatically during comparison.

Multiple stations allocate from the same pool transactionally and never share one active handle. An expired admin session requires staff reauthentication; it does not transfer confirmation authority to the attendee or discard a confirmed activation. Leaving the station must lock its controls.

Keep trigger links, cookies, and claim payloads out of access logs, panic dumps, analytics, error tracking, and Referer headers. Set `Cache-Control: no-store` and `Referrer-Policy: no-referrer`, omit third-party tracking, and check CDN/Ingress and outer API loggers. Store new browser onboarding secrets as hashes; existing claim-handle storage remains governed by the current contract and restricted server access.

Staff enforce the expected one-person-one-VTA practice. No device fingerprinting or phone identifier is proposed. A stricter guarantee would require individual admission tickets or another agreed eligibility mechanism.

## 5. Data Model and State

These are logical entities. Implementation should use additive migrations and existing relationships without reserving migration numbers in this draft.

| Entity | Proposed fields and invariants |
| --- | --- |
| Pool configuration | Target ready count, total inventory/retention ceilings, build and activation concurrency, pinned image, provider snapshot, pause flag |
| `users.account_kind` | Proposed non-null classification: `person`, `platform`, `load_test`, or `pool_owner`; internal owners cannot authenticate or acquire personal credentials |
| `pool_build_batches` | Requested count, creating admin, optional event allocation, frozen profile, request digest/idempotency key, admitted capacity, progress counts; each accepted member has a stable ordinal |
| `pool_slots` | Unique live `setup_session_id` and `resource_owner_user_id`, unique `(batch_id, member_ordinal)`, optional allocated event, state, generation, readiness/handover timestamps, failure reason, worker lease and cleanup progress |
| `pool_events` | ID, label, maximum uses, held/committed counters, expiry/disabled timestamps, creating admin; public event tokens are unnecessary for staffed distribution |
| `pool_stations` | Event, creating/operator admin, current reservation, display generation, enabled state; at most one unconfirmed reservation |
| `pool_reservations` | ID, event/slot/station/mobile-connection IDs, state, deadlines, confirming admin, quota-settlement marker, idempotency-key digest |
| `setup_sessions.portal_user_id` | Nullable FK to the real Portal user; immutable infrastructure ownership remains in `user_id` |
| Pool ownership entitlement | Immutable session/reservation/VTA DID/claimant DID relationship, handover timestamp, revoked/linked state; survives short-lived QR/replay cleanup |
| Portal binding attempts | Entitlement, target account or provisional enrollment ID, browser-secret hash, nonce, purpose, expiry, verified/consumed state |
| Audit records | Actor, event, slot/reservation/session IDs, timestamps, transitions; no bearer credentials or private keys |
| Cleanup manifest/tombstone | Immutable original owner/session IDs and exact external resource identities, per-step results, deletion actor/reason, retry state; survives removal of live rows without cascading away |

### Infrastructure Owner Versus Portal Owner

Create one internal non-login `users` row per slot, explicitly classified as a pool resource owner, with a unique generated ID, no email, and no passkey. Use its numeric ID in `setup_sessions.user_id`; retain it for that deployment's lifetime. Namespace, ServiceAccount, Vault policy, and seed paths continue to use this value. Never use the admin's ID, one shared platform user, the wallet DID string, or a guessed email as this owner.

Store the attendee's real account in a separate nullable `setup_sessions.portal_user_id`. This permits an existing account to manage its newly claimed VTA without moving resources or changing namespace identity. For example:

```text
Pre-build:        session.user_id = 4102; portal_user_id = NULL
Claim + running: session.user_id = 4102; portal_user_id = NULL
Portal binding:  session.user_id = 4102; portal_user_id = 87
```

Before binding, only Farm admins can manage this session through Portal APIs; the attendee already controls the VTA through Keyring. After binding, user 87 can manage it, while Farm admins retain their existing operator access. Admin UI should distinguish infrastructure owner, claimant DID, and Portal account. Portal enrollment is optional for keeping the VTA.

This separation requires an authorization migration, not just a new column. Backfill `portal_user_id = user_id` for existing ordinary user sessions, leave platform/load-test/pool system sessions unbound, and set both IDs for newly created ordinary sessions. User-facing authorization must use `portal_user_id` exclusively after cutover; no fallback for NULL pooled owners. Update list/detail/delete, configuration/export/resources/logs, administrator grants, mobile connections, upgrade creation/status/cancellation, and user quota/profile queries. Keep namespace selection, Vault access, last-session cleanup, and resource-owner deletion keyed by the original `session.UserID`. Audit direct `user_id` queries as well as shared helpers.

Pool resource owners are never login principals and must be excluded from signup, passkey enrollment, and account-recovery issuance. Portal-account deletion cannot cascade-delete or silently return a deployment to the pool. Retain bound deployments for explicit operator disposition. Initial binding is unique and atomic; already-bound sessions cannot be reassigned by repeating a wallet proof.

Attaching an already-provisioned VTA must not be blocked by the normal two-VTA creation limit: binding allocates no new infrastructure. Count it toward the account's subsequent creation limit without granting `fullstack_access`; event quota continues to govern event issuance.

### Permanent Resource Placement

The following example uses resource owner `4102`, setup session `730`, and eventual Portal user `87`. The IDs are illustrative, not values to reserve or configure.

| Resource or identity | Pre-build placement | After claim and Portal binding |
| --- | --- | --- |
| Infrastructure owner | `users.id = 4102`, `account_kind = pool_owner` | Unchanged; still a non-login owner |
| Session ownership | `user_id = 4102`, `portal_user_id = NULL` | `user_id = 4102`, `portal_user_id = 87` |
| Kubernetes namespace | `<namespace-prefix>-4102`, normally `vtafarm-user-4102` | Unchanged |
| Workload identity | ServiceAccount `vta` in that namespace | Same namespace-scoped identity |
| Persistent data | PVC `vta-data-730` in that namespace | Same PVC and data |
| Vault policy/auth role | `vta-user-4102`, bound to that namespace and ServiceAccount | Unchanged |
| Vault KV prefix | `<kv-mount>/vta/user-4102/session-730/` | Unchanged; never moved under `user-87` |
| Master seed | `master-seed` beneath that prefix | Same seed; no regeneration or copying during claim |
| VTA hostname and DID | Created once from the session name/provider snapshot | Unchanged |

Vault paths above describe logical KV locations, not raw KV v2 HTTP API routes. Reuse `UserNamespace`, `VtaPVCName`, `UserName`, `VtaPrefix`, and `SeedPath`; do not introduce a second naming algorithm for pooled sessions. A dedicated owner means the existing per-user Vault policy cannot overlap another attendee's or the platform user's prefixes. Reusing the shared mediator/DID host does not grant access to their Vault keys. Namespace separation does not, by itself, assert network isolation or a separate Vault server per attendee.

Build sequence after database admission:

1. Load the committed slot, owner, session, and frozen provider/profile. Record expected external resource identities in the cleanup manifest before issuing side effects.
2. Ensure the owner's namespace, ServiceAccounts, and RBAC. On an existing resource, verify ownership before reuse; `AlreadyExists` alone is not ownership evidence. Record Kubernetes UIDs where available.
3. Ensure the owner's Vault policy and Kubernetes-auth role. Render setup configuration with that exact owner's seed path and role.
4. Provision session DNS and the PVC/configuration/setup Job in that namespace, then publish and verify the DID through the snapshotted shared provider. Never fall back to the platform namespace or seed path on failure.
5. Mark the slot ready only after all readiness checks complete and setup work no longer consumes running compute. Keep the PVC and required configuration; no VTA Deployment runs while parked.
6. On staff confirmation, run administrator import and start the Deployment against the same PVC and owner. Portal binding later updates management authorization only.

Persist the cluster/environment identity, namespace name/UID, Vault KV/auth mounts and logical prefix/role, DNS record IDs, and DID-host provider identifiers needed for recovery. These are protected operational metadata, not user-visible secrets. A later configuration change must not redirect cleanup to a different cluster, mount, or namespace prefix; fail closed if the original target cannot be resolved. Do not store seed values or access credentials in the manifest.

### User ID Allocation and a 50-Slot Batch

`users.id` is currently a PostgreSQL `BIGSERIAL` primary key. Personal accounts, the platform account, and pool owners use the same database-generated sequence. Creating an event alone allocates no owners; accepting its explicit 50-slot build batch creates 50 real owner rows, not an application-side reservation of an ID range.

For illustration, if the last allocated value is 20, no other inserts occur, and no values are skipped, the 50 owners receive 21 through 70; a later ordinary signup receives 71. Twenty existing users does not necessarily mean the sequence is at 20. With concurrent signup or another batch, IDs may interleave; transaction rollback or conflicts can leave gaps. Never promise consecutive IDs or derive batch membership from an ID interval. PostgreSQL allocates sequence values atomically across concurrent callers and does not generally reclaim aborted values. See [PostgreSQL sequence semantics](https://www.postgresql.org/docs/18/functions-sequence.html).

Implementation rules:

- Omit `id` on inserts and use the generated value returned by the database/ORM. Do not calculate `MAX(id) + 1`, manually insert a reserved range, reset the sequence, decrement it after cleanup, or use sequence values as an attendee count.
- Keep the numeric ID immutable. Deleted owner IDs are never intentionally reused for new inventory. An actual sequence misconfiguration after an import/restore requires a separate maintenance procedure, not a fallback in signup or pool creation.
- Generate `users.unique_id` independently within its existing 12-character column limit, enforce its unique index, and retry collisions. Account kind is stored explicitly, not encoded in a long prefix or inferred from an ID range.
- Persist exact owner/session/slot associations. Use a batch member ordinal for progress such as `17 of 50`; gaps in primary keys must not affect ordering, cleanup selection, or metrics.
- Allocate IDs only as part of committed ownership records before creating external resources. A rolled-back insert may consume a number, but it must not leave a namespace or Vault path based on an uncommitted owner.

### Atomic Admission, Build Retries, and Account Classification

For a bounded 50-slot request, resolve an existing idempotency outcome before attempting new admission. For a new request, validate the profile/event and reserve capacity under a shared database lock within the admission transaction. Persist the idempotency record, batch, 50 owner rows, 50 initial setup-session rows, 50 queued slots, and durable build-work records together. Commit before responding `202` and before workers make external calls. If existing setup creation requires DNS before its row insert, split database admission from external provisioning for this path; do not run the public signup or ordinary create handler 50 times. Existing startup recovery must not bypass the pool scheduler or activate queued/parked sessions; integrate both paths under the same work ownership and deletion fencing.

The `202` guarantee is **50 durable members admitted**, not 50 ready VTAs. Cap the request count so admission stays bounded; larger inventories use multiple explicit batches. A database failure commits no partial batch, although unused sequence values may remain. The same actor/idempotency key and payload return the same batch; a changed payload conflicts. An HTTP timeout after commit must not create another 50 owners on retry.

Workers update each existing member independently. A temporary build failure retries the same owner/session and inspects existing resources before continuing; it does not create another user. If the original identity cannot be safely recovered, quarantine and explicitly retire it before creating a separately tracked replacement with new owner/session/name/seed. Report failed and retired members separately from ready inventory. Periodically reconcile queued, failed, and retiring records; an empty namespace or old timestamp alone is not permission to delete an owner.

Add `account_kind` using an additive, reviewed backfill: identify the platform owner and legacy load-test owners from existing provenance, classify existing ordinary accounts as `person`, and create future pooled owners as `pool_owner`. Do not classify by `email IS NULL`, because legitimate personal accounts can have no email. Preserve the existing admin `system` response as a derived compatibility field and add kind/filter support without silently changing the list's default semantics. Show human-account and internal-owner totals separately; derive personal quotas from Portal ownership, not total `users` rows.

Enforce non-login status server-side across login/session issuance, passkey registration, invitation/account adoption, profile changes, recovery-link creation/consumption, and administrative personal-account controls. A pool owner always has no email, no personal passkeys, and no fullstack-access grant. Use database checks where possible and service-level checks for cross-table invariants. Do not upgrade it into a person at claim or Portal enrollment. The real Portal account gets a separate row or is an existing authenticated person account.

Each pooled owner has exactly one lifetime slot association and at most one live setup session. Enforce unique live owner/slot and owner/session associations with constraints and a transactional cross-table guard; prevent ordinary creation paths from attaching another session or custom domain to that owner. Archive original numeric IDs in tombstones; live owner/session FKs may be cleared only in final retirement. Use restrictive FKs/guards so deleting an owner cannot cascade away a live slot or cleanup manifest. Check all references, not only a zero session count.

### Stable DNS Naming (Proposed Default)

Use the existing `[dev-]vta-<vta_name>.<managed-domain>` shape with a generated lowercase random name, for example `vta-k7m2x9p4q8.<managed-domain>`. Reserve the session name transactionally with collision retries and retain retired names in a registry checked by every name-allocation path, including ordinary setup. Keep the existing `<vta_name>-vta` DID path suffix. Do not include an email, wallet DID, numeric owner ID, event label, or temporary `pool` marker. A future display name may change without renaming DNS, the session route identifier, or the DID. The exact generated-name format remains an implementation default, not an already-shipped behavior.

### State Responsibilities

```text
slot: queued -> building -> ready -> reserved -> claimed -> activating -> handed_over

reserved/claimed -> ready
  only after rejection, cancellation, or expiry, before confirmation/key import

uncertain state or possible partial key import -> quarantined -> retiring -> retired
```

Preserve existing `mobile_connections` states: `open -> claimed -> confirmed -> provisioning -> released`, plus rejected/lapsed and existing terminal states. `handed_over` is a pool allocation state, not a new Trust Task state. `released` means available for handover, never available for reuse in the pool.

- The five-minute QR clock starts when a ready slot is assigned and a handle is created. Claim starts a separate five-minute confirmation window, using existing constants and database time.
- Before confirmation, reject/expiry/cancel atomically terminates the reservation and settles quota. Return the slot only after verifying that no administrator key was installed. Every subsequent reservation gets a new handle; old handles never revive.
- Confirm must serialize with reject, expiry, and cleanup. Once confirmation commits, the slot cannot be reassigned, even before the import Job starts.
- QR and comparison deadlines no longer cancel confirmed work. An HTTP timeout is not provisioning failure and must not instruct the wallet to delete its key.
- Retry with the same reservation, slot generation, and claimant. Inspect Job/ACL/Deployment state after interruption or partial success. Quarantine or rebuild a slot if key-import state is uncertain; do not return it directly to ready.

### Quota and Idempotency

Proposed accounting: `held + committed <= max_uses`. Allocation increments held; confirmation transfers held to committed. An unconfirmed reject/expiry refunds once. Compensating a confirmed failure requires evidence that it cannot leave a usable or still-starting VTA, plus an auditable settlement operation.

Allocate the slot, create the reservation/mobile connection, reserve event quota, and save the idempotency outcome in one database transaction. Define one lock order compatible with the existing session-first claim/confirm operations; adding event locks in inconsistent order can deadlock. Use unique constraints to prevent multiple active reservations for a slot. Commit before waking workers.

Retries for the same station generation return the existing reservation. Scope `Idempotency-Key` to actor, event, station generation, and operation; return 409 when the same key carries a different payload. Lost responses must be recoverable. Repeated confirmation of the same already-confirmed claim returns current progress without restarting the Job or incorrectly reporting expiration.

## 6. Claiming Portal Management After the Event

### Recommended Product Flow

Use **wallet ownership verification followed by account enrollment**, not email-only ownership lookup and not a mandatory new general-purpose wallet-login system.

For a new Portal user:

1. On their own phone, open Farm Portal from the claimed VTA in Keyring, carrying only its public VTA DID as a hint. On a computer, select Connect my claimed VTA and enter the VTA DID shown in Keyring. The DID is not a credential. The Farm creates a short-lived onboarding attempt bound to that browser, the selected VTA, and a provisional new-account ID. It displays a separate Portal-binding QR or app link, clearly labeled as Portal access, not another VTA claim.
2. Keyring selects the previously claimed VTA, verifies the Farm request, displays the operation and browser comparison check, and asks the holder to approve. It signs the fresh ownership challenge with the retained claimant key. The Farm matches that proof against the successful event entitlement.
3. Only that browser receives a limited onboarding session for the verified VTA. It can collect an email and register a passkey, but cannot list other users' resources, obtain a general user JWT, or modify the VTA yet.
4. The attendee enters an unused email and creates a passkey. Atomically save the new account's email/passkey, set `portal_user_id`, mark the entitlement linked, and consume the onboarding attempt. Only then issue the ordinary Farm user session and show the VTA in My VTAs.
5. Later login uses the passkey. Email may be an account hint/profile label, but typing it alone cannot authenticate or find claimable VTAs.

For an existing Portal user, first authenticate with their passkey, then select Connect my claimed VTA and approve the wallet challenge. Bind the challenge to that exact account and show its identifying label in Keyring. Require a fresh passkey assertion for the binding, not merely a cookie issued by the existing public signup flow. After approval and browser confirmation, atomically attach the VTA to the authenticated account without changing the infrastructure owner.

If the attendee never visits the Portal, keep `portal_user_id = NULL`. Their VTA keeps running, Keyring keeps working, and Farm admins retain management access. Portal enrollment is neither another allocation nor a condition for retention.

### What the Proof Establishes

At successful event handover, persist an entitlement containing the session ID, VTA DID, original claim reservation, claimant Ed25519 `did:key`, and confirming admin. Commit the entitlement, pool handover timestamp, and mobile release transition consistently in one transaction after readiness succeeds. A recovery worker must reconcile this even if both browsers disconnect; do not depend on a later status GET. Private keys remain in Keyring. This record is longer-lived than QR expiry and replay caches.

The Portal challenge proves control of the private key recorded for that entitlement. Knowing a DID string, VTA DID, email, six-character check, or old trigger handle is insufficient. A saved claim/redeem signature cannot be replayed as a Portal-binding approval: purpose, audience, challenge, and browser binding are different.

Only a completed handover entitlement may bind. Pending/failed/rejected claims cannot. Binding is single-assignment: the same account's retry returns its existing result; a different target receives a conflict. A later transfer or recovery is a separate audited action, not another claim.

### Key Rotation and Long-Term Authority

Do not assume the initial administrator DID remains the runtime administrator forever. Existing Farm code documents PNM rotation on first connection, and client onboarding supports replacing the ephemeral ACL identity. A cached ACL snapshot is not current ownership evidence, and refreshing that snapshot currently stops the VTA.

For the initial Portal-binding design, require Keyring to retain the event claimant private key **as an unbound Farm ownership credential** until Portal attachment succeeds, even if it rotates its VTA runtime ACL key. This extends the existing mobile guide's key-retention requirement with an explicit purpose. Keyring must store the Farm DID, VTA DID, and original claimant relationship durably and sign only a Farm-verified, user-approved binding challenge. Verify this behavior in the actual mobile Keyring; browser-extension/PNM behavior is not evidence of mobile support.

The Farm entitlement deliberately represents the right to attach the original event allocation, not current runtime ACL membership. Granting another VTA administrator does not transfer Farm hosting ownership. A runtime key swap does not silently revoke or transfer this entitlement. Provide an admin revocation/ownership-recovery procedure for a lost or compromised claim credential; key loss is never repaired by accepting an email assertion.

Once Portal binding completes, that key cannot bind a second account, sign into the existing account, replace passkeys, or recover it. The Portal's passkey/recovery policy becomes authoritative for hosting management. Do not delete a key from Keyring merely because Portal binding succeeded if the key is still used for VTA administration or redeem recovery.

If the actual Keyring discards the original key during onboarding, automated Portal binding is blocked until it either retains an appropriate Farm binding credential or implements a verified successor-key registration. A successor registration must authenticate the old entitlement key and prove possession of the new key, bind the Farm/VTA/purpose, and revoke the old binding credential atomically. A wallet-supplied new DID or an arbitrary current ACL administrator is not a substitute. This is a Keyring integration requirement, not a claim that it is already implemented.

### Binding Ceremony Contract

Keep `provision/vta/claim/0.1` and `provision/vta/redeem/0.1` unchanged. Define the Portal-binding ceremony separately before implementation. Its proposed semantic contract is:

- Server-generated random nonce with at least 128 bits of entropy, five-minute expiry, single-use consumption, and server-side state.
- Explicit version, purpose (`enroll_new_portal_owner` or `bind_existing_portal_owner`), Farm DID/audience, exact Portal origin, attempt ID, browser-binding ID, VTA DID, claimant DID, target account/provisional ID, and deadline. Existing-account display identity is frozen for the attempt; changing the target requires a new challenge.
- A Farm-signed request verified by Keyring, and an Ed25519 proof over the complete canonical challenge under the recorded claimant key. Freeze the canonicalization, proof fields, and closed schema in the implementation contract; reuse existing cryptographic helpers only where their validation semantics match.
- Explicit wallet approval and a browser comparison check. The signing UI states that this grants Farm hosting management, including configuration/deletion rights, and does not grant staff access to the attendee's wallet.
- A random HttpOnly browser capability, hashed at rest and independent of the wallet-visible QR. Only its originating browser can poll private results and finish enrollment/linking. A wallet response never receives or forwards a Portal JWT to the station or another browser.
- A separate short-lived request-read handle in the QR/app link lets Keyring fetch the signed challenge without the browser cookie. It exposes only the request needed for informed approval, cannot finish enrollment, and is also redacted from logs. Polling/completion still require the private browser capability.
- Verification and final linking each recheck entitlement state, expiry, target account, and deletion/revocation. Linking and challenge consumption share one transaction. Replayed proofs, changed targets, and stale attempts cannot transfer ownership.

This is a proposed Farm HTTPS ceremony, not a registered Trust Task or an assertion that Keyring already recognizes a new trigger-link type. Its app-link dispatch and signed schema are implementation dependencies. Do not put a Portal-binding request into a VTA-claim QR and expect existing handlers to interpret it.

The existing [auth/oob/identify specification](https://github.com/trustoverip/dtgwg-trust-tasks-tf/blob/main/specs/auth/oob/identify/0.1/spec.md) verifies an already recognized member and participates in a larger approval flow; it is not, by itself, proof that the member owns this Farm allocation. Therefore generic VTI sign-in is not assumed to solve initial Portal attachment. It may be integrated later once its subject is securely mapped to Farm accounts and entitlements.

### Email, Passkey, and Interrupted Enrollment

The current Farm sends no verification email. Email is a self-declared label; passkeys authenticate accounts. Preserve that meaning in this feature. Email OTP/magic-link login would be a separate authentication and delivery-system change, not implied by this design.

There is a concrete integration hazard: `SignupHandler.resumeOrReject` issues a full user session for an email whose account has no passkeys. Never write a pooled owner's email or attach their VTA to such an account before credential setup. Otherwise another person could enter the same email and resume an account that already controls the VTA.

Keep new onboarding accounts isolated with no public email mapping or ordinary login session until successful WebAuthn completion. Use a purpose-limited registration context rather than the generic signup-issued JWT. Commit email uniqueness, passkey persistence, account activation, ownership binding, and attempt consumption together. If registration or the final transaction fails, leave the VTA unbound and recoverable through a fresh wallet challenge; it must not become reclaimable event inventory.

If the requested email already exists, require authentication to that existing account and a fresh challenge targeting it, or choose another unused email. Never merge based on the email. An existing email-only account that cannot provide a passkey requires the ordinary recovery/onboarding resolution without attaching the VTA; do not trust its old signup cookies. Concurrent enrollment for the same email must have one database winner and no partial binding. Expired pending accounts can be cleaned independently of VTA resources.

## 7. Proposed API

The primary frontend is an admin event/station page. REST paths below are under `/api/v1`. The existing wallet endpoint remains `POST /trust-task/trust-tasks`. Public event-link/self-service allocation routes are deferred.

| Method/path under `/api/v1` | Authority | Behavior |
| --- | --- | --- |
| `GET /admin/pool` | Admin | Inventory, active work, quarantine, retained count, budget, dependency state |
| `POST /admin/pool/slots` | Admin | `{count,event_id?}` with the configured pool profile and required idempotency key; 202 only after all requested owner/session/slot rows and work records commit; batch ID, admitted count, and status URL |
| `GET /admin/pool/batches/:id` | Admin | Requested/admitted/queued/building/ready/failed/retired counts and paginated member details; exact member identities, not an owner-ID range |
| `GET /admin/pool/slots` | Admin | Paginated inventory with state/batch/event filters; no bearer credentials |
| `POST /admin/pool/events` | Admin | `{label,max_uses,expires_at}`; 201; create a bounded distribution event |
| `GET /admin/pool/events` | Admin | Paginated events, quota, expiry, and disabled state |
| `PUT /admin/pool/events/:id/status` | Admin | `{enabled:false}` stops new reservations; does not delete resources or cancel confirmed delivery |
| `POST /admin/pool/events/:id/stations` | Admin | Create an event station; return station ID and page URL |
| `GET /admin/pool/stations/:id` | Admin with station scope | Current reservation/generation, QR while open, comparison check after claim, and progress |
| `POST /admin/pool/stations/:id/reservations` | Admin with station scope and CSRF protection | Next attendee; expected generation and idempotency key; 201 new or 200 resumed reservation |
| `POST /admin/pool/reservations/:id/confirm` | Admin with station scope and CSRF protection | Confirm the frozen reservation/generation; 202 activation; do not accept a replacement admin DID |
| `POST /admin/pool/reservations/:id/reject` | Admin with station scope and CSRF protection | 200 rejection before confirmation; next issuance gets a fresh handle |
| `DELETE /admin/pool/reservations/:id` | Admin with station scope and CSRF protection | 204 cancellation before claim; 409 for later states |
| `GET /admin/pool/events/:id/cleanup-preview` | Admin | Eligible/protected resources and a short-lived versioned plan ID |
| `POST /admin/pool/events/:id/cleanup` | Admin | Submit plan ID; recheck eligibility; 409 for stale plans, otherwise 202 with cleanup-job URL |
| `GET /admin/pool/cleanup-jobs/:id` | Admin | Progress, item failures, and retry status |
| `GET /admin/users` (extension) | Admin | Preserve the existing list contract; expose account kind and optional kind filtering, with internal owners marked `system: true` |
| `POST /portal-bindings` | Own browser capability/origin; recent passkey proof for an existing target account | Create a purpose-bound ownership challenge; no general user login |
| `GET /portal-bindings/:id` | Originating browser capability | Challenge, verified state, expiry, and allowed next step; no public ownership lookup |
| `GET /portal-binding-requests/:handle` | Short-lived request-read handle | Return the Farm-signed request for Keyring approval; no browser session or completion authority |
| `POST /portal-bindings/:id/proof` | Fresh claimant signature | Verify the separate ownership ceremony; never return a browser login token to the wallet |
| `POST /portal-bindings/:id/passkeys/begin` | Verified limited onboarding context | Start new-account WebAuthn registration |
| `POST /portal-bindings/:id/complete` | Same browser; valid WebAuthn finish or fresh existing-account authentication | Atomically consume proof, finalize account if new, and attach the VTA; only then issue the browser session |

GET must not provision resources or consume quota. Proposed HTTP errors are 400 for invalid fields, 401 for missing authentication, 403 for insufficient authority or CSRF/origin violations, 404 for unknown or non-owned resources, 409 for stale station generations, `event_exhausted`, `already_bound`, or target/email conflicts, 410 for expired/disabled events or expired attempts, 429 for rate limiting, and 503 for `pool_empty`/`at_capacity`/`dns_budget_exhausted`/`shared_infra_unavailable`. State GET may return 200 with temporary unavailability; allocation POST returns 503. Failed ownership proofs must not enumerate which email owns a VTA.

Browser REST errors follow the repository's `error`/`reason` convention:

```json
{"error":"No VTA is ready yet.","reason":"pool_empty","retry_after_seconds":10}
```

Include `Retry-After` for 429 and retryable 503 responses. Station views may include `reservation_id`, `generation`, `status`, `server_time`, `expires_at`, `confirm_by`, and `next_poll_after_seconds`. Only authorized station admins receive its trigger link for display. Use RFC 3339 timestamps in REST; preserve Unix seconds for VTI `_exp` and the existing Trust Task `confirmBy` field.

Trust Tasks retain their signed response/error schemas, including HTTP 422 for redeem pending; do not apply browser REST errors to that endpoint. Each new redeem poll uses a new document ID, while retransmitting the same document uses replay handling. The original claimant must be able to redeem again after a lost success response; define a recovery-retention window before pruning handover/replay records.

When implementing routes, update `internal/apidocs/openapi.yaml` and the mobile integration guide together. Do not publish these proposed endpoints in OpenAPI before implementation. Add required idempotency/CSRF headers to the existing CORS allowlist and restrict origins to the deployed frontend.

## 8. Workers, Capacity, and Event Traffic

- Persist build, activation, and cleanup work. Use cross-replica locks/leases and generation checks so a stale worker cannot modify a reassigned slot. External operations need stable resource identities and inspectable retry behavior.
- Pool readiness requires completed PVC/configuration work, verified DID publication/resolution, healthy shared dependencies, and no attendee administrator key. Recheck readiness freshness before claim.
- Retain the original provider URL/DID snapshot. Rebuilding the platform stack must not silently rewrite existing identities. Stop allocation when shared hosting or mediation is unavailable and report the reason.
- Configure target ready inventory, build/activation concurrency, storage/DNS budgets, and a total retained/running-VTA ceiling separately. Kept VTAs remain part of the capacity budget; refill cannot exceed the ceiling.
- Parked PVC count is not simultaneous activation capacity. Reserve enough runtime headroom for admitted claims. Existing cached, fail-open estimates are insufficient for atomic batch admission.
- Treat the prototype's concurrency of 8–10 as a candidate, not a measured safe value. Measure build/activation P50/P95, failure rate, and shared-service load. Prioritize attendee activation over background refill.
- Shared event Wi-Fi concentrates traffic behind one IP. Test existing Trust Task per-IP limits and long-poll HTTP/database connection use. Combine event, browser, handle, and global limits; neither IP-only nor freely generated DID-only limits are sufficient.
- Keep pool and DID-host domains stable after the event. For a different zone/subdomain, verify DNS authority and TLS coverage first: a single-level wildcard does not cover arbitrary deeper subdomains. Avoid per-slot certificate issuance.

If FIFO is included initially, create expiring waiting tickets that atomically hold quota, cap queue depth, and define event fairness/cancellation. Order tickets in the database. Assign a slot and start the QR timer only when it is ready; a failed build must not strand a ticket. Expire abandoned tickets and refund once. ETA is an estimate and may be unknown. Do not admit unlimited waiting work without activation capacity.

## 9. Retention and Cleanup

The confirmed requirement is to batch-clean incomplete event claims and leave successfully claimed VTAs alone. A successful claim here means staff confirmation followed by completed provisioning (`released`/pool `handed_over`); the earlier wallet `claimed` state alone is incomplete. Portal account binding and first DIDComm connection are not cleanup prerequisites.

| Event resource state | Batch cleanup decision |
| --- | --- |
| Event-allocated, unused queued/ready/building slot | Eligible after distribution closes; cancel builds and verify shutdown before removal |
| Open or wallet-claimed but unconfirmed | Terminate reservation atomically, invalidate handle, settle quota, then clean the untouched slot |
| Confirmed or activating | Protect automatically; operator review/retry because a key may already be installed |
| Handed over, with or without Portal account | Always excluded from event cleanup |
| Previously handed over but now stopped/unhealthy | Still excluded; current runtime status is not ownership state |
| Failed/uncertain activation | Quarantine; explicit separate disposition before destruction, never direct reuse |

Record durable `handed_over_at`/entitlement state, not just `status == running`. A claimed VTA may later crash, stop, or upgrade and must not suddenly look like unused inventory. Similarly, `portal_user_id IS NULL` does not mean unclaimed. Do not use the old `connected_at IS NULL` condition.

Disable new reservations before generating an event cleanup preview. Protect confirmed, handed-over, and uncertain slots, and exclude inventory reassigned to another event. Give batches/slots explicit event allocation when preparing event stock; otherwise unused slots that never had a reservation have no event association to clean by. Shared unallocated stock requires a separate pool-inventory cleanup selection. Re-lock and recheck each item during execution; a preview is not a permanent authorization snapshot. Never delete namespaces solely because they once belonged to an event.

Unconfirmed, untouched reservations may return to shared inventory. Shrinking that inventory is a separate cleanup policy. A failed slot that may contain an attendee key must be explicitly disposed of and rebuilt, never reissued with a new handle. Track DNS, DID/ACL, Kubernetes, Vault, and owner cleanup independently. Existing teardown includes best-effort steps, so deleting a session row does not prove full cleanup. Retain tombstones, external resource identifiers, and retryable cleanup work until removal is verified.

Refill runs only while enabled and within budget; ending an event should not automatically restore its peak inventory target. Closing event distribution, reclaiming unused inventory, and deleting a handed-over VTA are distinct operations in both the UI and API.

### Ordered Retirement and Resource-Owner Removal

The internal owner exists for the VTA's lifetime, not the attendee's login lifetime. Inactivity, a stopped Deployment, missing email, or an unbound Portal account are not orphan evidence. A handed-over VTA requires a separately authorized explicit deletion by its linked Portal user or a Farm admin. Deleting that VTA does not delete the person's account or other VTAs. Deleting a person's Portal account must not delete pooled infrastructure: use a restrictive `portal_user_id` FK until an explicit disposition is completed, and retain linked/revoked entitlement history so clearing a link cannot reopen initial binding.

Implement retirement as durable work with these checkpoints:

1. **Authorize and fence.** Re-lock the slot/session and recheck the cleanup plan, event membership, generation, and handover status. Mark `retiring`, block allocation/binding/activation/upgrades, and invalidate outstanding unconfirmed reservations. Serialize with build/provision workers; wait for active operations to stop. A lease timeout alone does not prove an external Job stopped. Preserve a cleanup manifest before deleting anything.
2. **Stop writers.** Cancel queued work and remove or stop all setup/import/maintenance Jobs and Deployments for this slot. Verify Pods have terminated and cannot recreate data. Record failure and retry if shutdown is uncertain; never clear the owner while stale work can still write.
3. **Remove external identities and routing.** Delete only this session's DNS records, shared DID-host document, and VTA ACL entry through the recorded provider. Do not delete the platform mediator/DID host, platform account, or shared credentials. Reconcile ambiguous provider responses rather than treating an HTTP timeout as success.
4. **Remove stored data and access.** Delete the session's Kubernetes resources/PVC and exact Vault secret subtree, including stored KV versions/metadata according to the deletion policy. Remove its exclusive Vault auth role/policy and verify the absence of effective workload access, including any issued token permissions. Do not equate role deletion alone with invalidating issued tokens. Check the storage reclaim policy: a deleted PVC may leave a retained backing volume or backup requiring explicit disposition. Never claim all data is erased while retained copies remain.
5. **Remove the namespace.** Require the expected namespace identity/labels, no unexpected workloads or other owners, and no remaining dependencies. Await actual namespace deletion; an accepted deletion request or `Terminating` status is not completion. Do not force-remove finalizers automatically. Quarantine mismatches for operator review.
6. **Finalize database retirement.** Only after all required external checkpoints are verified, mark the tombstone complete, invalidate binding entitlements/attempts, detach completed live FKs, and delete the setup-session and `pool_owner` rows in a transaction. Recheck owner kind, its exclusive slot association, absence of credentials/domains/other live references, and completion under lock. Preserve audit and original resource identifiers without deletion cascades. Keep the batch/event record and retired name reservation; do not reset any sequence.

The current `setup_sessions.user_id ON DELETE CASCADE` is not a resource cleanup mechanism. Introduce a restrictive guard for pooled owners (for example, a live slot-to-owner `ON DELETE RESTRICT` FK plus guarded finalization), and route all pool deletions through this worker, including existing admin/user session-delete endpoints. Existing teardown must return inspectable step results rather than deleting the session after best-effort calls. A failure leaves the owner/session manifest discoverable as `cleanup_failed` progress on a retiring slot, with retry/backoff and operator diagnostics. Do not delete the original metadata just to make the dashboard appear empty.

Recovery must handle the crash between an external success and saving its checkpoint: re-inspect the exact resource, treat verified absence as success, and retry remaining steps. Not-found is success only for a known target on a reachable, correct provider; authorization errors or missing provider configuration are not not-found. New creation and deletion share owner/slot locks so a zero-reference check cannot race with a new attachment. Owner rows with zero sessions but unfinished manifests remain protected; legacy orphan candidates require an explicit reviewed cleanup plan.

For the 50-slot example, if 20 are handed over and 30 remain untouched, an event cleanup can retire those 30 and their internal owners. The 20 retained owners remain even if none of their holders enrolls in the Portal. IDs for the deleted 30 stay unused; future signups and new batches use newly generated IDs. Pausing a retained VTA preserves its owner/PVC/secrets; deletion is a different operation.

## 10. Implementation Sequence and Acceptance

1. Validate the Keyring retention/signing integration for later ownership proof; agree its concrete wire contract before implementing Portal binding.
2. Add account-kind classification, permanent owner/slot constraints, batch admission/idempotency, cleanup manifests, and pool/event/station/reservation models. Build directly in each committed owner's namespace/Vault path; address readiness/publication recovery and provide inventory views.
3. Implement station authorization and atomic slot/quota/replay transitions, provisioning recovery, quarantine, and per-attendee QR rotation. Preserve signed-in owner claim flows.
4. Migrate Portal-versus-infrastructure ownership authorization, then implement limited new-account onboarding and existing-account linking with passkey and wallet proof.
5. Add cleanup previews and checkpointed retirement with owner-last deletion, capacity ceilings, refill, and account-kind-aware operational visibility. Update OpenAPI/mobile documentation and rehearse station issuance, later Portal attachment, and cleanup in an isolated environment. Portal attachment is required for feature completion, even if station work is delivered first.

Required acceptance cases:

- A 50-slot admission creates exactly 50 distinct internal owners, sessions, and queued members before 202. Concurrent signup/batches remain unique even when IDs interleave. Rollbacks and deleted owners leave harmless gaps; no code derives ownership from a numeric interval.
- Retrying an accepted batch after a lost response returns the same members. A failed admission creates no committed partial batch or external resources; build retries reuse committed owner/session IDs without inserting new users.
- The namespace name/UID, PVC, ServiceAccount scope, Vault role/prefix, hostname, and VTA DID remain identical across ready, confirmed, running, and Portal-bound states. Claim/binding performs no seed copy, namespace move, or owner conversion.
- Pool owner A's workload cannot authenticate as owner B or the platform owner and cannot read/write their Vault paths. Shared mediator/DID-host integration remains functional. A conflicting existing namespace or mismatched role binding fails closed.
- Internal owners cannot log in, enroll passkeys, obtain recovery links, acquire email/fullstack access, or receive additional sessions/domains. Legacy personal accounts without email remain personal; UI filters/counts do not report pool owners as attendees.
- Cleanup protects claimed-but-unbound and stopped retained VTAs. An event with 20 handed-over and 30 unused slots retires only the 30 selected unused owners. Person accounts and unrelated resources are untouched.
- Failure injection at every retirement checkpoint preserves a resumable manifest and owner until completion. Test already-absent resources, namespace finalizers, retained storage, unavailable providers, DB finalization failure, and a crash after an external delete but before its checkpoint.
- Direct deletion of a live pool owner is rejected; cleanup never relies on cascading session deletion. Owner finalization races with claim, Portal binding, worker restart, and attempted new attachment cannot orphan resources or erase audit history.
- Backfill preserves ordinary-session access and quotas, isolates system owners, and rejects an unsafe rollback once pooled sessions depend on separate Portal ownership. Rehearse migration and rollback constraints on an isolated database before rollout.
- Multiple replicas compete for the last slot/event use: only one succeeds. Refreshes and lost responses do not consume additional quota.
- Event/station/reservation ID or trigger QR possession cannot authorize staff confirmation. Cross-origin mutations fail; stale tabs cannot confirm a newer display generation.
- Claim/reject/expiry and confirm/reaper/cleanup races cannot double-allocate or double-refund. Confirmed slots never return to ready.
- Recovery covers crashes between transaction commit and worker startup, partial DID publication, successful Jobs before DB updates, expired worker leases, and workers restarting during deletion.
- Invalid signatures, non-claimant redeem, replay, and expired handles retain existing protocol guarantees. The claimant can recover a lost successful response.
- Two phones scanning the station QR produce one claimant. A mismatch can be rejected safely; the next attendee gets a different handle and, after a successful handover, a different slot. Next attendee never grants another administrator on the previous VTA.
- Refreshing or reauthenticating the station resumes its current reservation. Keyring restarts and missed redeem responses preserve the original claimant and eventual entitlement.
- Provider, DNS, Vault, image-pull, PVC, and readiness failures produce accurate state and bounded retries; waiting is never reported as success.
- Disabling an event preserves confirmed handovers. Cleanup protects reassigned inventory, partial deletions, handed-over slots without Portal users, temporarily unhealthy handed-over VTAs, and ongoing activation.
- Portal binding rejects known DID strings without signatures, old claim proofs, wrong Farm/origin/purpose/target, expired challenges, and simultaneous attempts to bind different accounts.
- Runtime key rotation preserves the separate unbound ownership credential; a lost/revoked credential cannot be replaced by email, a new wallet DID, or a stale ACL snapshot.
- New-account email/passkey/binding finalization is atomic. Public signup, earlier signup cookies, duplicate-email races, and abandoned passkey registration cannot gain access to a claimed VTA.
- Existing-account binding requires that account's passkey and the claimant proof. It changes `portal_user_id` only; namespace, PVC, Vault paths, and VTA identity remain stable.
- Every user read/write/upgrade/export/log route uses Portal authorization, while teardown uses infrastructure ownership. A linked account sees its existing and claimed sessions; an unrelated account and the non-login resource owner cannot access them.
- Rehearsal covers shared Wi-Fi, API restarts, and multiple replicas, measuring request-to-reservation and confirm-to-redeem time and completing actual Keyring DIDComm use.

For implementation, run `make test` (including `go vet`), test database races and migrations against isolated PostgreSQL, and format changed Go files with gofmt. Before rollout, verify the actual Farm DID, Keyring, and link host. External-readiness statements in older design documents are not current deployment evidence.

## 11. Remaining Decisions and Integration Gates

| Question | Proposed default | Implementation consequence |
| --- | --- | --- |
| Does mobile Keyring retain the original claim key and support a user-approved Portal-binding signature? | Require durable claim-key retention and an explicit binding ceremony | Must be verified with the actual mobile client; existing claim/redeem support alone is insufficient |
| Is email a profile/account hint with passkey login, or is email OTP login desired? | Keep current email-label/passkey authentication | Email OTP adds verification, delivery, recovery, and abuse handling beyond current behavior |
| First-event attendance, number of stations, acceptable activation wait, and simultaneous claims? | Pre-build enough inventory; staff retry on shortage | Determines resource ceilings, rate limits, and acceptance load |
| Domain/provider, image, resource profile, and operating ceiling? | Existing managed domain/platform stack and fixed profile | DID names are durable and kept VTAs consume ongoing resources |
| Who handles lost keys, mistaken confirmed claims, and activation failures? | Farm admin review with independent evidence and audit | Email or possession of a station QR cannot authorize takeover or reissue of a keyed VTA |

## 12. Verification Boundary

This change updates only this design document. It does not add APIs, migrations, pool resources, deployments, or version changes. The prototype, existing Farm signup/passkey and claim implementation, resource naming, user sequence/schema, teardown behavior, and related key-rotation code were inspected. Parked pre-provisioning, staffed confirmation, permanent per-slot resource ownership without migration, retention of successful claims, and optional later Portal management are confirmed requirements. Account-kind classification, batch admission, Portal authorization changes, and checkpointed cleanup are implementation requirements, not verified runtime behavior. Latency targets, actual mobile Keyring ownership-proof support, and current external-service readiness remain unverified.
