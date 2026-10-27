# Public VTA Signup Flow Design

- Status: Proposed
- Scope: `vtafarm`, `vtafarm-api`, and the existing pre-provisioned VTA pool
- Related designs: `docs/pooled-vta-claim-design.md` and
  `docs/vta-claim-implementation-design.md`
- Reference experience:
  `https://simplified-vta-farm.ic3.workers.dev/get-started/`

## 1. Summary

The public signup page should not generate a permanent page or assign a VTA
when somebody merely visits the Farm. It should create a short-lived claim only
after the visitor selects **Get my VTA**.

That action atomically reserves one waiting VTA from the explicitly enabled
public-signup inventory and returns its existing VTI claim trigger link. The browser keeps an
opaque access token in `sessionStorage`; the token is not placed in the URL.
The same `/get-started` route therefore looks personal to that browser tab
without creating an indexable, shareable, user-specific page.

The reservation has the existing five-minute QR lifetime:

- if nobody scans it, it lapses and the VTA can be offered again;
- if a different phone scans it first, the browser shows that phone's claim
  check and the intended visitor can reject it;
- if the visitor scans and confirms the matching claim check, that VTA is
  permanently consumed from the pool and is never offered again;
- closing the page does not strand inventory: an unconfirmed claim becomes
  eligible again after its existing open or confirmation deadline.

The existing pool allocator, `mobile_connections` state machine, signed Trust
Tasks, comparison code, provisioning worker, and Keyring redeem flow remain the
source of truth. The public flow adds a safe browser capability and public UI;
it does not create a second claim protocol.

## 2. User Journey

The home page keeps **Sign in** as a secondary action and makes **Get your VTA**
the primary onboarding action. It opens `/get-started`, which presents the same
six-stage journey as the reference page:

1. Install Keyring from TestFlight or Google Play, or continue if it is already
   installed.
2. Select **Get my VTA**. The Farm reserves one waiting pooled VTA and displays
   its claim QR.
3. On a desktop, scan the QR with Keyring. On a phone, select **Open in
   Keyring**, using the existing `keyring://` conversion of the API-provided VTI
   trigger link.
4. Compare the six-character value in Keyring with the value on the Farm page,
   then select **Codes match**. The page follows provisioning until the claim is
   released to Keyring.
5. Scan the FPDev VTC's public DID and submit the join request in Keyring.
6. Open the configured Discord invitation, say hello, and request access while
   the VTC request is pending.

The page must work in both layouts:

- **Two-device:** a laptop displays the QR and confirmation state while a phone
  runs Keyring.
- **One-device:** the phone displays an **Open in Keyring** button. Returning to
  the browser resumes the same reservation and its live state.

The VTC currently publishes a QR containing the bare community DID. Until
Keyring defines a same-device deep link for a bare DID, the mobile fallback is
**Copy community DID** plus **Open the VTC page**. Do not invent a URI scheme for
this step. The VTA claim already has a supported `keyring://` deep link.

## 3. Product Decisions

### 3.1 Allocate only on explicit intent

`GET /` and `GET /get-started` never reserve inventory. Allocation happens only
on the visitor's explicit button press. This prevents crawlers, link previews,
page refreshes, and casual visitors from exhausting the pool.

Disable the button while the request is in flight. If an active reservation is
present in `sessionStorage`, resume it instead of creating another one.

### 3.2 Publish inventory deliberately

Not every admin pool batch should be public. Add nullable
`pool_batches.purpose` with `admin_station` and `public_signup` values, plus
nullable `pool_batches.public_signup_enabled_at`. The batch creation form asks
for the purpose when the administrator enters the requested count, VTA image,
and label. The default is `admin_station` so a batch cannot become public by
accident.

Creating a `public_signup` batch does not expose it immediately. After enough
members reach `pool_waiting`, an administrator explicitly enables it. More than
one public-signup batch may be enabled. This makes replenishment additive: an
administrator can create another 20 VTAs while the original batch of 60 is
still serving visitors, then enable the new batch without changing frontend
configuration or interrupting allocation.

Disabling a batch stops new reservations but does not invalidate claims already
issued from it. Those claims can finish or lapse normally. Existing pool cleanup
rules continue to prevent removal of active or claimed sessions.

### 3.3 Reuse the existing allocator

Add a public allocator entry point that selects across enabled
`public_signup` batches and then calls the same locked allocation path as the
admin claim station. It must retain the existing database serialization and
row-lock behavior. Concurrent admin and public requests can therefore never
receive the same VTA. Prefer the oldest enabled batch and lowest waiting session
ID so older inventory drains first; correctness must not depend on that order.

For two simultaneous visitors, allocation behaves as follows:

1. A and B send reservation requests with different browser capability tokens.
2. A acquires the pool allocation lock, selects waiting session 101, creates a
   `mobile_connections` row with claim handle A, and commits.
3. B then acquires the lock. Session 101 now has a live reservation, so B
   selects session 102 and receives claim handle B.
4. If session 101 was the last available member, B receives
   `409 public_pool_empty` instead of session 101.

The QR payload is built from its connection's random 128-bit claim handle, so
A and B receive different QR values as well as different pooled VTAs. A lapsed
VTA may later be reserved again, but the replacement gets a new claim handle;
the expired QR never becomes valid again.

Selection may stay deterministic inside the enabled batch; the security
property is atomic exclusivity, not randomness. The public response must not
expose the batch ID, session ID, user ID, inventory count, internal VTA name, or
candidate DID.

### 3.4 Treat the browser token as a scoped capability

The public browser has no Farm account, so it needs a capability limited to one
claim. The capability also makes reservation creation idempotent:

1. Before the POST, generate 32 random bytes with Web Crypto and keep the
   base64url token in the page's `sessionStorage`.
2. Send it as `X-VTA-Claim-Token` when creating the reservation.
3. Store only its SHA-256 digest on the associated `mobile_connections` row as
   `public_access_digest`, protected by a unique index.
4. In the same allocation transaction, return the existing connection when the
   digest already exists; otherwise reserve one waiting VTA and attach the
   digest.
5. Keep `{requestId, accessToken}` in `sessionStorage` and send the same header
   for status, confirm, and reject calls.
6. Compare digests in constant time and reject tokens belonging to any other
   claim.

This prevents a double-click, frontend retry, or lost HTTP response from
reserving two VTAs for the same browser operation. Two independent visitors
generate different tokens and therefore receive independent reservations. The
API validates that the decoded token is exactly 32 bytes; clients without Web
Crypto cannot start public signup.

The token never appears in a path, query string, trigger link, analytics event,
or application log. All public claim responses use `Cache-Control: no-store`;
the page uses `Referrer-Policy: no-referrer`. The token authorizes only read,
confirm, and reject for its one reservation. It cannot list inventory, allocate
a second VTA, or call any user/admin API.

The existing admin routes remain admin-authenticated and do not use this token.

### 3.5 Confirmation is the permanent handover boundary

The visitor replaces the event-station administrator only for the comparison
step. The Farm still computes the check from the DID in Keyring's signed claim;
Keyring independently computes its copy. Selecting **Codes match** performs the
existing compare-and-set transition:

```text
open -> claimed -> confirmed -> provisioning -> released
           |
           +-> rejected

open/claimed -> lapsed
```

Only `confirmed` and later states consume the VTA permanently. An expired,
rejected, or never-scanned reservation leaves `setup_sessions.admin_did` empty
and allows a future reservation for the same pooled VTA. A provisioning failure
after confirmation remains claimed and requires operator retry; it must never
return to public inventory.

### 3.6 Measure inventory, not people

The Farm can accurately report how many pooled VTAs have been handed out, but
it cannot claim that this is the number of unique humans. Public signup has no
Farm account, and one person could claim more than one VTA. Use **VTAs handed
out** rather than **users** in the Admin UI.

Show an aggregate public-signup inventory card and the same breakdown for each
batch. The categories must be mutually exclusive:

| Counter | Meaning |
| --- | --- |
| Requested | Original requested batch size; informational after cleanup |
| Preparing | Pre-provisioning has not reached `pool_waiting` |
| Available | Ready, unconfirmed, and not covered by a live reservation |
| Reserved | In an unexpired `open` or `claimed` browser/Keyring window |
| Handed out | Confirmation happened; includes activating, running, and post-confirmation failures |
| Failed unclaimed | Preparation failed before any confirmation |
| Deleting | Explicit cleanup owns the session |

`mobile_connections.confirmed_at IS NOT NULL` is the durable boundary for
**Handed out**. Count it once per pooled session, not once per connection row.
This means a VTA stays handed out even if activation later fails and an operator
must retry it. The headline operational numbers are **Available** and **Handed
out**; the other counters explain why the sum has not yet become either one.

The public page should not expose exact stock counts. It needs only
`available`/`unavailable`; exact inventory belongs in the Admin UI. Alert
operators when aggregate available stock crosses a configured threshold, for
example 10 VTAs or 20 percent of enabled inventory. Replenishment remains an
explicit admin action rather than automatically creating billable resources.

### 3.7 Recommended admin workflow

1. Create a pool batch with label `Public signup - <date>`, purpose **Public
   signup**, and the desired capacity, for example 60.
2. The existing capacity admission rejects the request before creation if the
   cluster cannot eventually run all 60 VTAs.
3. Watch the batch counters while its members are prepared. A requested size of
   60 does not mean 60 are immediately available.
4. Enable public signup after the first intended tranche is available. The
   public allocator can use only members in **Available**.
5. Monitor **Available**, **Reserved**, and **Handed out**. When **Available**
   reaches the warning threshold, create and prepare a replenishment batch.
6. Enable the replenishment batch when ready. Both batches feed the same public
   endpoint until the older inventory drains.
7. Pause a batch to stop new allocations, or clean it only after the existing
   unused-inventory safety rules allow cleanup.

If all enabled batches have zero available members, the API returns
`public_pool_empty`. The public page shows a temporary capacity message and no
QR. The Admin UI distinguishes whether inventory is still preparing, temporarily
reserved, permanently handed out, or failed, so the operator knows whether to
wait, retry, or create more capacity.

### 3.8 Replenish an existing batch

The current implementation accepts a quantity only while creating a new batch;
it has no operation for adding members to an existing batch. Add an **Add
inventory** action to the batch detail page so an event such as Conference A can
remain one operator-facing claim station when its original inventory runs out.

```text
POST /api/v1/admin/pool-batches/:id/inventory
Idempotency-Key: <new key for this addition>

{"count":10}
```

The operation reuses the batch's VTA image and infrastructure settings, checks
current cluster capacity, and atomically creates the additional users, setup
sessions, pool memberships, and resource rows. Existing claimed members do not
block replenishment. Cleanup in progress does block it. A successful addition
increments `pool_batches.requested_count`, starts the ordinary preparation
workers, and leaves the existing claim-station URL unchanged.

Record every addition in a small `pool_batch_additions` table containing batch,
administrator, count, idempotency key, request digest, and timestamp. This keeps
an audit trail and makes retries safe even though `requested_count` becomes a
cumulative target instead of only the original creation request. Enforce the
same VTA image for additions; create a separate batch when inventory must use a
different image version.

For example, Conference A can move from 10 total / 10 handed out to 20 total /
10 preparing / 10 handed out. As the new members reach `pool_waiting`, the same
Conference A claim station starts allocating them. For public signup, either
this action or an additional enabled public-signup batch can replenish the
shared inventory; the latter keeps rollout batches independently observable.

## 4. Browser State Model

| Page state | User experience | Next action |
| --- | --- | --- |
| `intro` | Keyring download links and an explanation that a VTA is waiting | Get my VTA |
| `reserving` | Single progress indicator; controls disabled | Wait |
| `open` | QR, **Open in Keyring**, and server-based expiry countdown | Scan/open Keyring |
| `claimed` | Six-character comparison value | Codes match / Not mine |
| `confirmed` or `provisioning` | Provisioning progress; no second allocation | Wait |
| `released` | VTA is linked; reveal the FPDev community step | Continue to FPDev VTC |
| `rejected` or `lapsed` | Explain that no VTA was handed over | Generate a new QR |
| `empty` | No public VTA is available right now | Try again later / Discord |
| `failed` | No success claim; preserve a support reference, not internals | Retry if safe / Discord |

Poll only while the state is non-terminal. Schedule the next poll after the
previous request completes so slow responses cannot accumulate. Calculate
countdowns using `server_time`, as the admin claim station already does.

If `sessionStorage` is missing, the page cannot recover the browser capability.
It should return to `intro`; the old unconfirmed reservation will lapse. Do not
put the token in a shareable recovery URL.

## 5. Public API Contract

All routes are unauthenticated in the account sense, HTTPS-only, rate-limited,
and covered by `NoStore` and the configured frontend CORS origin.

### `POST /api/v1/public/pool-claims`

Requires a 32-byte `X-VTA-Claim-Token`. It reserves one VTA from the enabled
public-signup batches, or returns the existing reservation for a retry using
the same token. A new reservation returns `201 Created`; an idempotent replay
returns `200 OK`:

```json
{
  "server_time": "2026-10-26T18:30:00Z",
  "connection": {
    "request_id": "<uuid>",
    "status": "open",
    "expires_at": "2026-10-26T18:35:00Z",
    "trigger_link": "https://link.trustoverip.org/t#..."
  }
}
```

Expected failures:

- `409 public_pool_empty` when enabled batches have no waiting inventory;
- `503 public_signup_disabled` when no public-signup batch is enabled or VTA Claim is not
  operational;
- `429 rate_limited` when the client exceeds the public reservation limit.

### Claim-specific routes

```text
GET  /api/v1/public/pool-claims/:request_id
POST /api/v1/public/pool-claims/:request_id/confirm
POST /api/v1/public/pool-claims/:request_id/reject
```

All three require the same `X-VTA-Claim-Token`. They return a deliberately reduced
connection view:

- `open`: status, expiry, and trigger link;
- `claimed`: status, `confirm_by`, and claim check;
- `confirmed`/`provisioning`: status and safe progress/error text;
- terminal: status and the VTA DID only when useful after `released`.

Use `404` for an unknown request and for a request/token mismatch so the API
does not become a claim-ID oracle. Confirmation and rejection retain the
existing idempotency and compare-and-set rules.

### Admin publication routes

```text
POST   /api/v1/admin/pool-batches/:id/public-signup
DELETE /api/v1/admin/pool-batches/:id/public-signup
```

The Admin Pool UI shows which batch is public, its waiting count, and an
explicit enable/disable control. Only batches whose purpose is `public_signup`
may be enabled through these routes. Enabling another batch adds its available
members to the same logical public inventory; it does not replace or disable an
existing batch.

## 6. Frontend Structure

Add a public `/get-started` route and keep the claim state in a dedicated hook
or controller rather than in the marketing home component. Reuse:

- `KeyringConnectionPanel` for QR, countdown, comparison, and confirmation;
- `keyringSameDeviceLink()` for the VTA's same-device action;
- the existing Keyring App Store/TestFlight links and assets;
- the pool station's non-overlapping polling and server-clock offset logic.

Do not reuse the admin `PoolClaim` response directly because it exposes fields
that the public contract intentionally omits.

Add public runtime configuration to `/config.js` and the Helm chart:

```text
PUBLIC_SIGNUP_VTC_URL
PUBLIC_SIGNUP_VTC_DID
PUBLIC_SIGNUP_DISCORD_URL
```

These values are public content, not secrets. Render the configured VTC DID as
the QR payload in the Farm client so step 5 can stay on the same onboarding
page. Also link to the VTC site as the authoritative fallback. Never ship a
placeholder Discord invitation such as `discord.gg/TODO`.

## 7. Abuse, Privacy, and Failure Handling

A fully anonymous public giveaway cannot prove that one human takes only one
VTA. The initial release should limit resource starvation without pretending to
solve identity:

- reserve only on an explicit POST, never on page load;
- retain the five-minute open and confirmation windows;
- apply a strict reservation limit at the shared edge (for example Cloudflare)
  and a second per-IP application limit;
- limit status polling separately so normal onboarding does not consume the
  reservation quota;
- return only coarse public errors and no inventory details;
- alert on sustained `public_pool_empty`, reservation rate-limit hits, high
  lapse ratios, confirmation failures, and provisioning failures;
- add a challenge such as Turnstile only if observed abuse justifies its
  accessibility and privacy cost.

The current in-memory API rate limiter is per replica, so it is not sufficient
as the only protection for a public allocation endpoint. Edge rate limiting is
part of the release, not a later hardening task.

Never log the access token, trigger-link fragment, claimant DID, comparison
code, or full request headers. Operational logs may include the internal
request ID and terminal state.

When inventory is empty, the public copy should say: **No VTA is available
right now. Please try again later or contact us on Discord.** Do not say that
the visitor failed to create a VTA; the public flow allocates prepared inventory
and the distinction helps support diagnose an empty pool.

## 8. Implementation Plan

### Phase 1: API and persistence

1. Add `purpose` and `public_signup_enabled_at` to `pool_batches` and
   `public_access_digest` to `mobile_connections`, with the required indexes.
2. Add admin enable/disable operations and the public-safe response types.
3. Add `ReservePublic` around the existing pool lock and allocator.
4. Add public status/confirm/reject handlers with capability verification,
   `NoStore`, rate limits, and reduced error mapping.
5. Document the new endpoints in OpenAPI.

### Phase 2: Web onboarding

1. Add `/get-started`, the six-stage layout, and home-page/nav CTAs.
2. Add reservation persistence, resume, polling, expiry, replacement, and
   terminal states.
3. Reuse the Keyring QR/deep-link UI and add the FPDev VTC/Discord steps.
4. Add runtime configuration and responsive/accessibility coverage.

### Phase 3: Operations and rollout

1. Configure edge rate limits and dashboards before enabling a batch.
2. Create and enable a small canary `public_signup` batch, then complete one desktop-plus-phone and one
   phone-only claim.
3. Verify the exact production VTC DID and Discord invitation.
4. Enable the intended public batch, observe lapse/claim/failure rates, then
   increase inventory.

## 9. Acceptance Criteria

- Merely visiting or refreshing public pages does not reserve a VTA.
- Creating a batch of 60 public-signup VTAs makes at most the prepared and
  explicitly enabled members allocatable; its Admin counters reconcile every
  member into one inventory category.
- Enabling a replenishment batch adds inventory without changing frontend
  configuration or interrupting existing claims.
- Two concurrent visitors receive different claim requests and cannot read or
  act on one another's claims.
- An abandoned, rejected, or expired claim makes the unconfirmed VTA available
  again without operator work.
- A confirmed claim can be delivered only once and its VTA is never allocated
  again, including when provisioning initially fails.
- A QR scanned by the wrong phone cannot be silently confirmed: the claim check
  differs for the intended visitor, who can reject it.
- Reloading the same tab resumes the claim; a different browser without the
  capability cannot access it.
- iOS and Android can open the VTA trigger directly in Keyring; desktop can scan
  the same HTTPS payload as a QR.
- The public page completes the VTA handover before presenting VTC join and
  Discord as the next actions.
- Empty, disabled, expired, rate-limited, and provisioning-failure states have
  actionable, non-technical copy.
- The API, frontend, and pool concurrency tests pass, and the final flow is
  tested on both one-device and two-device paths.

## 10. Required Tests

Backend tests should cover concurrent public allocation, coexistence with an
admin station allocation, two distinct simultaneous tokens receiving distinct
VTAs, duplicate simultaneous POSTs with one token receiving one reservation,
public-batch enablement, token hash storage, token isolation,
unknown/mismatched token indistinguishability, every legal and illegal state
transition, expiry reuse with a new QR handle, confirmed-session exclusion,
rate limits, reduced response fields, and no-store headers.

Frontend tests should cover no allocation on render, double-click prevention,
session resume, server-relative countdowns, non-overlapping polling, deep-link
conversion, mismatch rejection, expiry replacement, stock exhaustion, and the
transition from released VTA to VTC/Discord instructions.

End-to-end tests should run at least these paths:

1. desktop QR + phone Keyring -> matching code -> released;
2. phone browser -> Open in Keyring -> return -> matching code -> released;
3. abandon before scan -> expiry -> another visitor claims the same inventory;
4. wrong phone scans -> intended visitor rejects -> replacement succeeds;
5. two simultaneous visitors -> distinct VTAs -> both release exactly once.
