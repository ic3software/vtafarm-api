# VTA Claim Trigger Link Implementation Design

- Status: Draft for implementation review
- Scope: `vtafarm-api` and the VTA Farm web client
- Related issue: [ic3software/vtafarm-api#75](https://github.com/ic3software/vtafarm-api/issues/75)
- VTI baseline: [VTI trigger links PR #58](https://github.com/trustoverip/dtgwg-vti-spec/pull/58),
  reviewed at commit `e5b76e5dced42420277a2a115fa104e68b42eb0f`
- Trust Task baseline:
  [trustoverip/dtgwg-trust-tasks-tf#753](https://github.com/trustoverip/dtgwg-trust-tasks-tf/pull/753),
  merged as `6bffafea1bf5c887aca505e4a14ae9aabe4d08f9`
- Go bindings: `trust-tasks-go v0.7.1` and
  `trust-tasks-go/proof v0.1.2`

## 1. Summary

Replace the current Farm-specific JSON QR code and unsigned mobile callback
with the VTI `vta-claim` trigger-link flow. A wallet scans or opens the link,
resolves the Farm DID, and sends a signed
[`provision/vta/claim/0.1`](https://trusttasks.org/spec/provision/vta/claim/0.1)
Trust Task. The claim atomically reserves the handle for the wallet's fresh
administrator key, but does not yet write that key into a VTA.

The Farm and wallet independently display a six-character claim check derived
from that key. The signed-in person confirms or rejects the match on the Farm
page. The wallet learns the result by polling
[`provision/vta/redeem/0.1`](https://trusttasks.org/spec/provision/vta/redeem/0.1).
Only after confirmation, writing the key into the VTA, and receiving a response
from the VTA does the Farm return the terminal hand-over containing the VTA DID.

This two-task lifecycle is a protocol invariant:

```text
claim -> trust-task-next-step -> browser confirmation -> redeem -> hand-over
```

The claim task has no `#response`. A successful claim returns a signed
`trust-task-next-step` expecting `provision/vta/redeem/0.1`. This prevents the
Farm from signing a terminal success while the person can still select
"Not mine" or allow the confirmation hold to lapse.

## 2. Goals

- Generate the VTI `vta-claim` trigger link for initial VTA setup and adding an
  administrator to a running VTA.
- Consume `provision/vta/claim/0.1` and `provision/vta/redeem/0.1` over Trust
  Task HTTPS binding 0.2.
- Verify wallet proofs and sign Farm replies with the verification
  relationships required by the task specifications.
- Require the claim check before writing the candidate administrator key.
- Make Trust Task retries idempotent across API replicas and process restarts.
- Preserve the existing VTA provisioning and ACL workers behind the new
  confirmation boundary.
- Remove the legacy JSON callback, mobile progress credential, and SIOPv2
  login paths in the same cutover.
- Keep the reservation model compatible with future pre-provisioned VTA pool
  work without implementing that pool here.

## 3. Non-goals

- Building or operating a pre-provisioned VTA pool.
- Implementing the Farm side of the VTI `sign-in` flow.
- Implementing a VTA Key Grant flow.
- Hosting `link.trustoverip.org` or defining its browser behavior.
- Supporting both the legacy JSON QR flow and the VTI flow after cutover.
- Defining a Farm-local Trust Task type, payload extension, progress token, or
  alternate response schema.

## 4. External readiness

| Dependency | Current state | Implementation consequence |
| --- | --- | --- |
| `provision/vta/claim/0.1` | Merged and published; status is draft | Implement against the published task and pin the generated bindings. Keep task-specific code behind a small adapter because a draft can still change. |
| `provision/vta/redeem/0.1` | Merged and published; status is draft | Implement the polling and terminal hand-over exactly as specified. Do not retain the old progress-token protocol. |
| Go bindings | Published in `trust-tasks-go v0.7.1`; proof support is a separate module | Pin `trust-tasks-go v0.7.1` and `trust-tasks-go/proof v0.1.2`. Do not copy generated payload types into this repository. |
| VTI PR #58 | Open and internally consistent for VTI-LNK-110 through 117, but its introduction still says the Farm returns the DID in the first response and VTI-LNK-112 does not name `TrustTaskHTTPS` | Not a coding blocker. Treat the merged task specifications as authoritative for the exchange lifecycle. Re-review PR #58 before release. |
| Development Farm DID | The development log is reported as published; its final `TrustTaskHTTPS` service and assertion key still need environment verification | Backend and fixture tests can proceed. Startup now fails closed until the published document matches all three Farm settings. |
| Production Farm DID | Not published at the planned URL | Production release blocker. |
| Keyring `vta-claim` support | External implementation dependency | API implementation can proceed. End-to-end acceptance waits for support for both claim and redeem. |
| `link.trustoverip.org` | No public DNS at the time of this review | In-app scanning can be tested. System-camera and universal-link testing wait for the host or an agreed temporary `LINK_HOST`. |

Any local clone of `dtgwg-trust-tasks-tf` must include PR #753 before it is
used as a source reference. The published Go modules and registry documents are
the pinned dependency sources for this implementation.

## 5. Protocol contract

### 5.1 Trigger link

The Farm constructs this logical shape:

```text
https://link.trustoverip.org/t#_from=<farm-did>&_id=<handle>&_exp=<unix-time>&_type=/vti/flow/vta-claim/0.1
```

- `_from` is the configured Farm DID.
- `_id` is 16 random bytes encoded as 22-character unpadded base64url.
- `_exp` is an integer UTC epoch time no more than 300 seconds after creation.
- `_type` is `/vti/flow/vta-claim/0.1`.
- The complete ASCII link must not exceed 251 bytes for QR level M.
- Field order and escaping are deterministic and covered by a test vector.
- The link confers no authority by itself. A valid signed Trust Task is required
  to claim its handle.

The API constructs the complete link. The web client renders the exact returned
string as both the QR payload and the anchor `href`; it never reassembles the
fragment.

### 5.2 Trust Task HTTPS endpoint

The Farm DID advertises a `TrustTaskHTTPS` service whose `serviceEndpoint` is a
Trust-Task base. The wallet composes the binding-owned suffix:

```text
POST <TRUST_TASK_BASE>/trust-tasks
Content-Type: application/json
Accept: application/json
```

The endpoint is public. Request authentication comes from the Trust Task's
Data Integrity proof, not from a Farm login or a bearer token.

HTTP status mapping follows HTTPS binding 0.2. The Trust Task document or error
code remains authoritative:

| Outcome | HTTP status |
| --- | --- |
| Signed next step or terminal `#response` | `200` |
| Duplicate still executing, with no retained response yet | `202`, empty body |
| Completed duplicate for which no response exists or was retained | `204`, empty body |
| `malformedRequest` | `400` |
| `idConflict` | `409` |
| Task/proof/recipient/expiry rejection | `422` |
| Duplicate-execution record unavailable or retryable dependency outage | `503` |
| Unhandled internal failure | `500` |

`202` never means that the first arrival was merely accepted for later work. A
first request is processed synchronously to a Trust Task reply. For redeem, the
Farm may hold the request for up to 25 seconds before returning `pending`.

Every reply, including errors, uses `Cache-Control: no-store`.

### 5.3 Claim request

The canonical type URI is:

```text
https://trusttasks.org/spec/provision/vta/claim/0.1
```

The payload contains only:

```json
{"handle":"<22-character handle>"}
```

Required checks, in the generic consumer pipeline and then the task handler:

1. The envelope and payload validate against their published schemas.
2. `recipient` is exactly `FARM_DID`.
3. `issuer` is a fresh Ed25519 `did:key`.
4. The proof uses `eddsa-jcs-2022` with `proofPurpose: authentication` and
   verifies against `issuer`.
5. `parentThreadId` is present and equals `payload.handle`.
6. The handle exists, remains inside its claim window, and is still `open`.
7. The request document passed duplicate-execution checks before dispatch.

The issuer is the administrator key `K_a`; there is no `adminDid` field through
which the claimant can nominate another key. The operation is derived from the
locked reservation and can never be selected by the wallet.

On success the Farm atomically moves `open -> claimed`, records `K_a`, and
returns a signed `trust-task-next-step` with:

- one `expects` entry whose `typeUri` is
  `https://trusttasks.org/spec/provision/vta/redeem/0.1`;
- `continuation` = `proceed`;
- that entry's `hint.handle` = the handle;
- `inResponseTo.typeUri` = the claim Type URI and `inResponseTo.id` = the
  claim document ID.

The next step is signed by the Farm for `assertionMethod`. It must not contain
the VTA DID, claim check, owner identity, progress URL, credential, or session
token. The claim task defines no `#response`.

### 5.4 Claim check and confirmation

The claim check is the first six characters of the upper-case base32 encoding
of SHA-256 over the UTF-8 bytes of the full `K_a` DID string.

The Farm shows its computed value only in the signed-in owner view. The wallet
computes and displays its own value. Neither value is transported from one side
to the other.

The person chooses one of two owner-authenticated actions:

- **They match:** compare-and-set `claimed -> confirmed`, then schedule the
  existing VTA provisioning or ACL worker after the transaction commits.
- **Not mine:** compare-and-set `claimed -> rejected`, discard `K_a`, and leave
  the VTA in the pool or unchanged.

If the confirmation hold expires, the reservation becomes `lapsed` and `K_a`
is discarded. Nothing is written into a setup session's administrator field or
a running VTA's ACL before confirmation.

This design applies the check to both initial provisioning and adding an
administrator. The merged Trust Task requires confirmation before hand-over;
VTI-LNK-117 additionally makes it mandatory for a running VTA.

### 5.5 Redeem request and outcomes

The canonical type URI is:

```text
https://trusttasks.org/spec/provision/vta/redeem/0.1
```

The wallet sends freshly identified and freshly signed redeem documents with
the same `K_a`, Farm recipient, endpoint, and handle used for claim. The Farm
refuses a different issuer with `provision/vta:notClaimant`.

`redeem` does not change reservation state. It reports one of these outcomes:

| Reservation state | Reply |
| --- | --- |
| `claimed` | After holding for up to 25 seconds, `provision/vta/redeem:pending` with `details.state=claimed` and `confirmBy`. |
| `confirmed` | `pending` with `details.state=confirmed`. |
| `provisioning` | `pending` with `details.state=provisioning`. |
| `rejected` | Final `provision/vta/redeem:rejected`; wallet discards `K_a`. |
| `lapsed` | `provision/vta:handleExpired`; wallet discards `K_a`. |
| `released` | Terminal `provision/vta/redeem/0.1#response`. |

Only one redeem poll may remain open for a reservation. Additional concurrent
polls receive retryable `unavailable` with `retryAfter`.

The terminal response is available only after `K_a` is in the VTA and the VTA
answers. Its payload is generated from server-side state:

| Field | Required | Meaning |
| --- | --- | --- |
| `handle` | Yes | The original reservation handle. |
| `vtaDid` | Yes | The VTA now administered by `K_a`. |
| `operation` | Yes | `provisionVta` or `grantAcl`. |
| `mediatorDid` | No | Mediator DID when available. |
| `didLogUrl` | No | VTA DID log URL when available. |

The Farm signs the hand-over for `assertionMethod` and keeps returning the same
semantic hand-over to the same key for at least as long as it keeps the
reservation record. No response contains a bearer credential, progress URL,
person identity, or Farm web session.

### 5.6 Proofs and DID roles

- Wallet claim and redeem documents use `K_a`, `eddsa-jcs-2022`, and
  `proofPurpose: authentication`.
- Farm next-step, terminal response, and Trust Task error documents use the
  configured Farm signing key and `proofPurpose: assertionMethod` where the
  framework requires a proof.
- The Farm verifies proofs before returning handle-specific information.
- The wallet verifies Farm proofs against the verification method published in
  the Farm DID document, not against a key supplied in a response.
- `FARM_SIGNING_KEY` is distinct from the DID update key,
  `DID_HOSTING_PRIVATE_KEY`, and any mobile HMAC key.

Use the upstream `trust-tasks-go/proof` verifier for the supported
`eddsa-jcs-2022` `did:key` path. Keep Farm signing and DID resolution behind
small interfaces so the transport/handler code does not depend on DID-hosting
implementation details.

### 5.7 Error codes

Use the published full codes without aliases:

| Code | Condition |
| --- | --- |
| `provision/vta:keyUnsupported` | Issuer is not an Ed25519 `did:key`. |
| `provision/vta:handleNotFound` | No reservation has the handle. |
| `provision/vta:handleExpired` | Claim or confirmation window lapsed, or the reservation ended. |
| `provision/vta:notClaimant` | Redeem was not signed by the claimant key. |
| `provision/vta/claim:alreadyClaimed` | Another valid claim already locked the handle. |
| `provision/vta/redeem:pending` | Confirmation or provisioning is unfinished. |
| `provision/vta/redeem:rejected` | The person selected "Not mine". |

Handle failures expose no details. A second scanner must not learn who claimed
the handle or which VTA it represents.

## 6. Architecture

```text
signed-in browser             vtafarm-api                    wallet
       |                           |                            |
       | create reservation        |                            |
       |-------------------------->|                            |
       | trigger link + expiry     |                            |
       |<--------------------------|                            |
       |                           |<------ signed claim -------|
       |                           | verify + open -> claimed   |
       |                           |------ signed next-step ---->|
       | poll owner status         |                            |
       |-------------------------->|<------ redeem poll --------|
       | claim check               |------ pending(claimed) ---->|
       |<--------------------------|                            |
       | confirm / reject          |                            |
       |-------------------------->|                            |
       |                           | confirmed -> provisioning  |
       |                           | write K_a; wait for VTA     |
       |                           | provisioning -> released   |
       |                           |<------ redeem poll --------|
       |                           |------ signed hand-over ---->|
```

PostgreSQL is authoritative for the reservation, duplicate-execution record,
and concurrency decisions. No correctness decision depends on in-memory state,
an API replica, or an open HTTP connection.

## 7. Persistence model

### 7.1 Reservation state

The stored state follows the published family convention exactly:

```text
open -> claimed -> confirmed -> provisioning -> released
           |
           +-> rejected

open/claimed -> lapsed
```

`rejected` and `lapsed` are final. Every transition is a compare-and-set under
a row lock.

Transient worker failures do not create a protocol success or hand-over. Keep
the reservation in `confirmed` or `provisioning`, record the operational error
internally, and retry or require operator recovery. `lapsed` applies only
before confirmation. Never report `released` until the VTA answers with the
key installed.

### 7.2 `mobile_connections` evolution

Evolve the existing table rather than create a parallel claim table:

| Column | Purpose |
| --- | --- |
| `claim_handle VARCHAR(22) UNIQUE` | Trigger-link `_id`, generated with `crypto/rand`; required for every new VTI reservation and nullable only for retained legacy history. |
| `status` | The reservation state above. |
| `admin_did` | Candidate `K_a`; populated only after a valid claim. |
| `claimant_digest` | SHA-256 digest of `K_a`; authenticates redeem after a rejected or lapsed claim without retaining the discarded key. |
| `operation` | Server-derived `provision_vta` or `grant_acl`; never supplied by the wallet. |
| `expires_at` | End of the initial claim window (`_exp`). |
| `confirm_by` | End of the post-claim confirmation hold. |
| `claimed_at` | Time the valid claim locked the handle. |
| `confirmed_at` | Time the owner confirmed the check. |
| `released_at` | Time the VTA answered with `K_a` installed. |
| `redeem_poll_id`, `redeem_poll_until` | Cross-replica lease that permits only one held redeem poll for the reservation. |
| `provision_error` | Internal diagnostics only; never returned in Trust Task replies. |

Use the existing session and VTA columns to derive operation and terminal
hand-over fields. The implementation migration may rename or replace the
legacy `accepted_at`, `provisioned_at`, and `connected_at` fields, but runtime
code must expose only the new lifecycle after cutover.

The cutover does not translate live legacy callback attempts into Trust Task
claims. Before deployment, drain or cancel unfinished legacy rows. Completed
historical rows may be mapped to `released` or retained as history outside the
active partial indexes.

Required constraints and indexes:

- unique `claim_handle`;
- one active reservation per setup session and operation;
- active indexes include `open`, `claimed`, `confirmed`, and `provisioning`;
- `admin_did` is empty in `open`, `rejected`, and `lapsed` after cleanup;
- `confirm_by` is present in `claimed`;
- `released_at` is present only in `released`.

### 7.3 Duplicate-execution records

Add one shared table for the Trust Task consumer pipeline:

| Column | Purpose |
| --- | --- |
| `document_id` primary key | Trust Task document `id`. |
| `document_digest` | SHA-256 of the canonical complete document, proof included. |
| `state` | `executing` or `completed`. |
| `response_bytes` | Exact bytes of the signed next step, response, or error when retained. |
| `retain_until` | End of the document acceptance window. |
| `lease_expires_at` | Recovery deadline for an abandoned executor. |
| timestamps | Operations and cleanup visibility. |

Store reply bytes rather than `JSONB`, so a bit-for-bit retry can receive the
same signed bytes. The HTTPS status is deterministically recovered from the
retained document (`trust-task-error` code mapping versus a successful
document), so it is not stored separately. All API replicas consult the same
table.

Rules:

1. Perform schema, audience, time, and proof checks before burning a document
   ID.
2. Atomically create the duplicate record immediately before dispatch.
3. The same ID and same digest returns the retained result when completed.
4. The same ID and digest receives `202` while a live execution lease exists.
5. The same ID with another digest receives `idConflict` and HTTP `409`.
6. An expired execution lease may be reclaimed transactionally.
7. If the record cannot be consulted, fail closed with retryable `unavailable`.
8. Retain the record for at least as long as the Farm would execute that
   document. For claim, this is at least the handle window; retain terminal
   hand-over results with the reservation record.

The claim transition and retained next-step bytes commit atomically. A crash
must not consume the one-time handle without preserving the reply that retries
need.

## 8. Transactions and concurrency

### 8.1 Claim transaction

Lock the reservation by `claim_handle` and atomically:

1. confirm `status=open` and `now < expires_at`;
2. derive the operation from the locked setup session;
3. set `admin_did=K_a`, `status=claimed`, `claimed_at`, and `confirm_by`;
4. store the signed next step in the duplicate-execution record.

Only one claim wins. Later claims receive `alreadyClaimed`, including when they
carry another valid document ID.

### 8.2 Confirm and reject transactions

Confirmation locks the reservation and session, compare-and-sets
`claimed -> confirmed`, and commits before starting a worker. After commit:

- `provision_vta` copies `K_a` into the setup session through the existing
  provisioning boundary and starts/resumes provisioning;
- `grant_acl` calls the serialized ACL path for the running VTA.

The worker moves `confirmed -> provisioning` and finally
`provisioning -> released` only after the VTA answers.

Rejection compare-and-sets `claimed -> rejected` and clears `admin_did`. An
expiry worker similarly moves eligible `open` or `claimed` rows to `lapsed`.
Confirmation, rejection, and expiry races have exactly one winner.

Manual administrator submission conflicts while the reservation is `claimed`,
`confirmed`, or `provisioning`. Rejecting or lapsing the claim releases that
conflict when no VTA mutation occurred.

### 8.3 Redeem polling

Redeem first authenticates and authorizes `K_a`, then reads the reservation.
Use database notification or bounded polling to wait for a state change, but
never rely on a process-local channel for correctness. Cap the HTTP hold at 25
seconds and permit one open poll per reservation.

A client disconnect does not cancel or change the Trust Task. A wallet retries
with a new redeem document; it bit-for-bit retries only when delivery of the
same document is uncertain.

## 9. HTTP API changes

All added, changed, and removed routes must be reflected in
`internal/apidocs/openapi.yaml`.

### 9.1 Public Trust Task route

```text
POST /trust-task/trust-tasks
```

`TRUST_TASK_BASE` is the public base advertised by the DID, for example
`https://vtafarm-api.firstperson.dev/trust-task`. The binding appends
`/trust-tasks`.

The handler:

- enforces a bounded JSON body and rejects trailing data;
- dispatches only canonical Type URIs;
- runs the framework consumer pipeline and duplicate-execution checks;
- verifies the proof before task-specific lookup;
- produces signed Trust Task documents and binding-compliant HTTP statuses;
- applies public rate limits and `Cache-Control: no-store`.

### 9.2 Owner reservation routes

Keep the existing authenticated create/current/refresh/cancel surface, but
replace callback data with trigger-link data:

- create/current returns `trigger_link`, `expires_at`, display status, and safe
  operation context;
- owner current returns `claim_check` and the candidate identifier only while
  awaiting confirmation;
- refresh replaces only an `open`, expired, rejected, or lapsed reservation;
- cancellation may close an `open` reservation but cannot undo a confirmed
  administrator mutation.

Add owner actions:

```text
POST /api/v1/setup/:id/mobile-connections/:request_id/confirm
POST /api/v1/setup/:id/mobile-connections/:request_id/reject
```

Both use the existing signed-in account ownership check. The add-administrator
case is available only on the page of the account that already controls the
running VTA.

### 9.3 Removed public mobile routes

Remove at cutover:

```text
POST /api/v1/mobile-connections/callback/:token
GET  /api/v1/mobile-connections/:request_id
POST /api/v1/mobile-connections/:request_id/complete
```

Claim replaces the unsigned callback. Redeem replaces the HMAC progress poll.
The terminal hand-over replaces the separate mobile completion credential and
report. There is no compatibility bridge.

## 10. Backend package boundaries

Add `internal/trusttask` with focused responsibilities:

- `endpoint`: HTTPS binding parsing, headers, status mapping, and body limits;
- `consumer`: common envelope validation and duplicate-execution pipeline;
- `dispatcher`: canonical Type URI routing;
- `proof`: wallet proof verification and Farm reply signing interfaces;
- `claim`: claim validation, reservation compare-and-set, and next-step;
- `redeem`: claimant authorization, bounded wait, and outcome mapping;
- `errors`: typed framework, family, and task errors independent of Gin.

`internal/connection` continues to own reservation persistence and downstream
work. Split the current `AcceptMobile` behavior into explicit operations:

- claim a verified candidate without changing a VTA;
- confirm or reject the candidate;
- begin provisioning after confirmation;
- release only after the VTA answers.

Transport and proof packages do not call Kubernetes or manipulate setup
sessions directly. Existing orchestrators are invoked only after a committed
confirmation transition.

Use generated payload models from:

```text
github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/provision/vta/claim/v0_1
github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/provision/vta/redeem/v0_1
```

Do not fork the upstream JSON schemas into this repository.

## 11. Web client changes

- Render only the backend-provided `trigger_link`.
- Wrap the QR in an anchor whose `href` is byte-for-byte the same string.
- Use byte mode, error correction level M, at least four CSS pixels per module,
  a quiet zone, and dark modules on a light background in both themes.
- Do not place a logo over the code.
- On expiry, cover the code and offer an explicit one-click replacement; do
  not silently generate another handle.
- When claimed, hide the QR and display the six-character check with
  **They match** and **Not mine** actions.
- Explain that the user compares the value with the one independently shown by
  the wallet.
- Continue owner-side polling through confirmation and provisioning. The
  browser never sees redeem documents or wallet credentials.
- Add `Referrer-Policy: no-referrer`, `Cache-Control: no-store`, and no
  third-party scripts on pages that show a trigger link or claim check.
- Until the VTI link host is live, say "open Keyring and scan" rather than
  promising that the system camera will open the wallet.

The full trigger link and handle must not enter analytics, error reporting,
client logs, referrers, or server access logs.

## 12. SIOPv2 and legacy mobile removal

Remove in the same feature release:

- SIOP routes, handlers, configuration, and frontend login/identity UI;
- passkey deletion restrictions that exist only for linked SIOP identities;
- legacy mobile callback, progress, and completion handlers;
- callback/progress HMAC generation and `MOBILE_CONNECTION_SIGNING_KEY` once
  no remaining code uses it;
- legacy QR documentation and tests.

Move generic `did:webvh` resolution helpers out of SIOP packages before
deleting those packages because Trust Task recipient and Farm proof validation
still need DID support.

Migration 42 drops `user_siop_identities`, `admin_siop_identities`, and
`siop_challenges` after their runtime readers and writers have been removed.
Its down migration recreates the empty legacy schema but cannot restore rows
that were deleted when the tables were dropped.

## 13. Configuration and DID publication

Add or rename:

| Variable | Secret | Purpose |
| --- | --- | --- |
| `FARM_DID` | No | `_from`, request recipient, and Farm reply issuer. Replaces `SIOP_RP_DID`. |
| `FARM_SIGNING_KEY` | Yes | Ed25519 key used for Farm Trust Task proofs. |
| `TRUST_TASK_BASE` | No | Public base advertised by `TrustTaskHTTPS`. |
| `LINK_HOST` | No | Trigger-link origin; defaults to `https://link.trustoverip.org`. |

The private key belongs in the existing external `vtafarm-api-secrets`
Kubernetes Secret. It must never appear in Helm values, committed environment
files, logs, examples, or documentation.

The key is a base64-encoded 32-byte Ed25519 seed. Its verification method ID is
derived deterministically as `<FARM_DID>#<public-multikey>`; the same method
must appear under the Farm DID document's `assertionMethod` relationship.

Each environment's Farm DID document must contain:

1. a `TrustTaskHTTPS` service whose HTTPS endpoint equals `TRUST_TASK_BASE`;
2. a verification method matching `FARM_SIGNING_KEY` under
   `assertionMethod`;
3. the relationships needed for any other Farm protocol, without reusing the
   DID update key as the application signing key.

Startup validation must prove that `FARM_DID`, `TRUST_TASK_BASE`, and the
derived public key agree with the published document. If they do not, the
Trust Task route must remain unavailable; it must not run partially configured.

Reserve the `vtafarm` name prefix so a user-controlled VTA path cannot resemble
the Farm identity on the confirmation screen.

## 14. Security and privacy invariants

- A trigger-link handle authorizes nothing without a valid signed claim.
- A valid claim reserves a candidate key but does not authorize a VTA write.
- Browser confirmation and possession of `K_a` are both required for hand-over.
- Only the authenticated owner page receives the Farm-computed claim check.
- No Trust Task reply carries Farm-account, browser-session, or person data.
- No response names the VTA as the claimant's before `released`.
- Operation and VTA selection always come from locked server state.
- Generic errors do not reveal whether another person claimed a handle.
- Duplicate execution is shared across replicas and fails closed when its
  record is unavailable.
- Claim, redeem, confirmation, and refresh routes are rate-limited separately.
- Logs use request/document IDs where necessary, never trigger links, handles,
  proofs, administrator keys, or signed document bodies.

## 15. Verification plan

### 15.1 Unit tests

- deterministic trigger-link vector, ASCII encoding, expiry, randomness, and
  251-byte limit;
- claim-check vectors from full `did:key` strings;
- generated claim and redeem payload validation;
- valid and invalid `eddsa-jcs-2022` proofs and proof-purpose checks;
- Farm signatures resolved through the configured DID `assertionMethod`;
- wrong recipient, unsupported issuer, parent-thread/handle mismatch, expired
  handle, malformed envelope, and unknown task;
- exact next-step fields and absence of VTA/person/check data;
- redeem pending states, `confirmBy`, rejection, expiry, wrong claimant, and
  terminal response fields;
- HTTPS binding status and `Cache-Control` mapping.

### 15.2 Persistence and concurrency tests

- only one claim moves a handle from `open` to `claimed`;
- no administrator mutation occurs before confirmation;
- confirm/reject/expiry races have exactly one winner;
- manual administrator entry conflicts with an active claim;
- workers start only after the confirmation transaction commits;
- `released` is impossible until the key is installed and the VTA answers;
- only one redeem poll is held per reservation;
- identical document retry returns byte-identical retained reply;
- reused document ID with different bytes returns `idConflict`;
- duplicate-record outage fails closed;
- abandoned execution lease recovery does not repeat a consequential effect;
- restart and cross-replica retries preserve results.

### 15.3 Handler and web tests

- request body limit, trailing JSON rejection, content type, and rate limit;
- owner authorization for current, confirm, reject, refresh, and cancel;
- owner API exposes the claim check only in the claimed state;
- public APIs never expose the check or Farm session data;
- QR payload and anchor `href` are byte-identical;
- open, claimed, rejected, lapsed, provisioning, released, and recovery UI;
- legacy callback/progress/completion and SIOP routes are absent;
- passkey login remains functional;
- OpenAPI matches every added, changed, and removed route.

### 15.4 End-to-end acceptance

Against the published development Farm DID and a Keyring build supporting both
tasks, run:

1. first-administrator provisioning;
2. adding an administrator to a running VTA;
3. "Not mine" rejection;
4. unconfirmed hold expiry;
5. a second scanner racing the intended claimant;
6. lost claim reply and bit-for-bit retry;
7. repeated redeem polls across an API restart.

Verify that no hand-over exists before confirmation, a rejected/lapsed key is
never installed, the released hand-over verifies against the Farm DID, and no
link, handle, proof, or claim check appears in logs.

## 16. Decisions and remaining release gates

### 16.1 Resolved implementation decisions

1. The task namespace is `provision/vta/*`, not `vta/*`.
2. Claim returns a signed next step and never a terminal `#response`.
3. Redeem is the only source of the terminal hand-over.
4. Claim confirmation is required before both initial provisioning and
   `grantAcl`.
5. The wallet uses its administrator key for polling; there is no bearer
   progress credential.
6. There is no compatibility bridge for the legacy JSON QR flow.
7. The implementation consumes the published Go bindings rather than a local
   copy of the schemas.

### 16.2 Release gates, not coding blockers

1. Update the development and production Farm DID documents with
   `TrustTaskHTTPS` and the dedicated assertion key.
2. Reconcile VTI PR #58's "first response" sentence and name the
   `TrustTaskHTTPS` service in or around VTI-LNK-112.
3. Re-review the published draft task versions immediately before release.
4. Confirm the Keyring release that implements claim, claim-check display, and
   redeem.
5. Provide a working link host for system-camera flows, or explicitly launch
   with in-app scanning only.
6. Drain or cancel legacy mobile attempts before the no-bridge deployment.
7. Update the `vtafarm-k8s` Stack 05 secret schema to supply
   `FARM_SIGNING_KEY` instead of the removed mobile callback HMAC key.

## 17. Implementation sequence

1. Pin the Trust Task and proof Go modules; add schema/model adapter tests.
2. Add reservation-state and duplicate-execution migrations and repository
   tests.
3. Add Farm DID/config validation and proof verification/signing.
4. Add the generic Trust Task HTTPS endpoint and consumer pipeline.
5. Implement claim and signed next-step handling.
6. Add owner claim-check status, confirm, reject, and expiry behavior.
7. Connect confirmed reservations to existing provisioning and ACL workers.
8. Implement redeem long-poll, pending/rejected errors, and terminal hand-over.
9. Replace JSON QR output and update the web claim-check flow.
10. Remove legacy mobile and SIOPv2 runtime surfaces; update OpenAPI and docs.
11. Publish the development DID changes and run end-to-end acceptance.
12. Complete the remaining release gates before production enablement.

Implementation may begin against the pinned merged task specifications. A
production release must satisfy every gate in section 16.2.
