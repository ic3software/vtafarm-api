# Custom Stack Connection — Design

Lets a **`vta_only`** session connect to a `full_stack` other than the platform
stack — provided that stack is **one this farm provisioned**.

Two halves, useless apart:

- **Share** — a `full_stack` owner mints a share code and hands it to somebody
  out of band — §4.
- **Connect** — the `vta_only` create form grows a **Customize** path that takes
  that one code instead of silently using the platform stack — §5.

> **Status: backend complete (phases 0–4); frontend outstanding.** §12 tracks it.
> §9 is the section to read first — it is where this design can go wrong.

Companion: [`vtafarm/docs/custom-stack-connection-frontend.md`](../../vtafarm/docs/custom-stack-connection-frontend.md).

Prerequisite reading: [`vta-setup-design.md`](vta-setup-design.md) §"DID hosting
credentials".

---

## 1. Scope

**Stacks outside this farm are not supported.** Not deferred behind a flag, not
half-built — the API has no code path for them and the code format does not
pretend otherwise. §11.1 records what it would take.

| In scope | Out of scope |
| --- | --- |
| `vta_only` → any `full_stack` **row in this farm's database** | any daemon this farm did not provision — §11.1 |
| A share code — one value, nothing alongside it — minted and rotated by the stack's owner | a directory of stacks accepting connections |
| Warning a provider what they are about to break, and telling the consumer afterwards | revoking a single connection, or blocking a delete — §7.4 |
| — | migrating a live `vta_only` between stacks — §11.3 |
| `vta_only` on the managed zone | `vta_only` on a custom domain (still excluded, `custom-domain-design.md` §18) |
| `full_stack` as provider | `vta_only` as provider — it deploys neither mediator nor daemon |

---

## 2. What a `vta_only` is wired to, and who may touch it

Three values, three columns that already exist (migration 000023). **The schema
does not need to change to hold a non-platform target** — which is the whole
reason this feature is small.

| Value | Column | Where it lands |
| --- | --- | --- |
| Mediator DID | `mediator_did` | `[messaging] did` in the VTA's `config.toml` (`templates.go`) |
| DID resolution URL | `did_hosting_server_url` | `vta_did_url = <server>/<name>-vta` → `[vta_did] url` |
| DID control URL | `did_hosting_control_url` | every `didhosting.Factory.For(...)` call |

`did_hosting_control_url` is the one that carries authority: it is not data we
render into a file, it is a URL **this server makes authenticated requests to**,
signing them with vtafarm-api's own private key.

| Call | When | ACL role on the target |
| --- | --- | --- |
| `New()` → `GET /api/server-info` | first use of a URL | none |
| `RegisterDid(path, didLog)` | `runSetup`, after `vta setup` | **admin** |
| `CreateAcl(vtaDid, "service", …)` | `runProvision` | **admin** |
| `ServerDid()` → `did-mgmt servers add --id control` | `runProvision` | none (cached) |
| `DeleteDid` / `DeleteAcl` | teardown | **admin** |

So connecting to a stack means **vtafarm-api must hold an admin ACL entry on
that stack's DID-hosting daemon**.

### 2.1 Why that is already true — and why it is exactly the scope boundary

`step_dids_grant_farm` (`orchestrator_fullstack.go:299`) runs
`did-hosting-daemon add-acl --did <DID_HOSTING_DID> --role admin --label vtafarm`
as an offline Job on the dids PVC, for **every** `full_stack` session — not just
the platform stack. `vta-setup-design.md` justifies it as "the farm operates
these deployments and manages the `did.jsonl` documents they serve".

That was written about operating customers' stacks. It also, unplanned, makes
every customer `full_stack` a **valid target at zero cost**: one keypair,
vtafarm-api's own, authenticates against all of them.

The set of stacks we hold admin on is exactly the set of `full_stack` rows in
our database. §1's boundary is not a policy choice layered on top of the
design — it *is* the design. A stack we did not provision would 401 on the
first upload, and §9.1 explains why that failure is currently silent.

---

## 3. The one rule that makes this safe

> **The code is evidence, not configuration.**

The three values a consumer session is built from are read from the **provider's
row in our own database**. The pasted code does two things and no more: it names
which row, and it proves the sharer authorised it. What the recipient sees about
that stack comes from the server (§5.2), never from what they pasted.

Nothing the user types ever becomes a URL this server connects to.

Everything downstream follows from that sentence:

- **No SSRF.** `did_hosting_control_url` still only ever comes out of our own
  DB, exactly as today. A pasted `http://169.254.169.254/` reaches no socket; it
  fails a string comparison.
- **No credential relay.** The `aud` of every id_token we sign still comes from
  a daemon we provisioned. The self-asserted-audience hole (§9.4) stays closed
  by construction rather than by a check we have to remember.
- **No stale-value class of bug.** A rebuilt stack is a *different row* with a
  different share code, so an old code fails to resolve instead of provisioning
  against a daemon that no longer exists.

A design that instead took the URLs from the sender would need every mitigation
in §9.4 to be correct forever. This one needs none of them — and since a code
names no host at all, there is nothing a caller could type that becomes a
socket.

---

## 4. Share: the share code

### 4.1 The share code is the grant

One nullable column on the provider's row:

```sql
share_code TEXT NULL          -- NULL = this stack is not shared
```

| Action | Effect |
| --- | --- |
| enable sharing | mint a random code |
| rotate | mint a new one — outstanding codes stop working, live connections keep running |
| disable | `NULL` — no new connections; live ones keep running |

One column rather than a `shared_use_enabled` boolean *plus* a code, because the
two would always have to agree and the code alone already answers both
questions. It is a capability, not a password: stored in plaintext, displayed to
its owner, never hashed — there is nothing to protect it *from* that rotation
does not handle better.

**Rotation is the reason this is not just a public toggle.** A bare
"anyone may connect" flag would let anybody enumerate stack names and attach; a
code means the owner chose each recipient, and can un-choose all of them in one
click without touching anyone already connected.

### 4.1.1 Format

16 characters of Crockford base32, grouped in fours —
`K7M2-9XQP-4B8W-3NRT`. 75 bits of entropy plus a **check symbol** as the last
character (Crockford's own scheme).

**Always alphanumeric.** Crockford's check alphabet extends the 32 data symbols
with `*~$=U` for remainders 32–36, so 5/37 of draws would end in punctuation —
`FDGE-K0G4-AWNF-CQS~`. Valid, and nothing downstream breaks, but a code that
cannot be read down a phone or typed on an arbitrary keyboard has given up the
one property this format was chosen for. `NewShareCode` rerolls until the check
symbol lands inside the data alphabet: 37/32 ≈ 1.16 attempts on average, 0.2
bits off the 75.

`ValidateShareCode` is deliberately **not** narrowed to match. The reroll
governs what we mint; codes handed out before it existed are live credentials
in the database, and rejecting their check symbol would lock out their holders
for a cosmetic reason.

Crockford rather than raw base32 because §2.2 offers this code to be read aloud,
and a format meant for oral transmission without a checksum is half-designed.
Three properties, all of which the paste side depends on:

- **Normalise before comparing.** Strip dashes and whitespace, uppercase, and
  fold the ambiguous glyphs — `I`, `L` → `1`, `O` → `0`. `k7m2-9xqp…`,
  `K7M2 9XQP…` and `K7MZ…`-with-a-typed-`l` all reach the same comparison.
- **The check symbol catches a typo locally**, before any request. That makes
  "you mistyped this" a different, instant answer from "this code is wrong"
  (§5.2) — different problems needing different actions from the user.
- **Compare in constant time** after normalising. It is a credential.

The check symbol is not security; an attacker computes it as easily as we do.
It exists so that the overwhelmingly common failure — a hand-copied character —
is diagnosed as itself.

The **platform stack has no code and needs none.** It is reached by the default
path, which sends no code at all. That also means nobody can accidentally
paste their way onto it through the Customize form — the two paths stay
distinct.

### 4.2 The code is the whole handover

Nothing travels with it. Not a JSON document, not the stack's name, not the
mediator DID or the DID-hosting URL.

The first cut passed a bundle carrying all of those. Removing it cost nothing,
because none of it was doing work:

| Field | Why it went |
| --- | --- |
| `stack` | the code is globally unique (`setup_sessions_share_code_unique`), so it identifies its stack alone |
| `mediator_did`, `did_hosting_server_url`, `did_hosting_did` | only ever compared, never used — §3 already says the row is authoritative |
| `farm` | a code from another deployment simply does not resolve |
| `v` / `kind` | the check character (§4.1.1) already rejects anything that is not a code |

And it removed a hazard rather than only weight. With a bundle, a client *can*
parse it and render "connecting to **alice**, mediator `did:webvh:…`" the moment
it is pasted — every value is right there — which would show a confident tick
for a bundle whose code is garbage. §5.2 existed to make that not happen. With a
bare code there is nothing to render from except the server's answer, so
presenting the sender's claims as facts is **structurally impossible** rather
than merely discouraged.

The one real loss: a code pasted at the wrong farm gets the generic
`invalid_bundle` instead of "this is for a different VTA Farm". Rare, and the
generic answer is true.

### 4.3 Where it is offered

| Surface | Route | Who |
| --- | --- | --- |
| Own `full_stack` detail | `GET /api/v1/setup/:id` → `connection` object | the owner |
| Admin session detail | `GET /api/v1/admin/setup-sessions/:id` | admins, for support |

Populated only once `status = 'running'`, `mediator_did <> ''` and
`did_hosting_did <> ''` — the same readiness rule `resolveSharedInfra` already
applies to the platform stack. Absent before that, so the UI cannot offer a
code that would fail on arrival.

---

## 5. Connect: the Customize path

`POST /api/v1/setup` grows one optional field:

```jsonc
{
  "mode": "vta_only",
  "vta_name": "myvta",
  "vta_image": "ghcr.io/…",
  "share_code": "K7M2-9XQP-4B8W-3NRT"                // optional
}
```

Absent → today's behaviour exactly. Present → the provider is the named stack
instead of the platform one.

The field is an **object, not a string to parse**. The frontend owns "the user
pasted something"; the backend owns "is this stack usable". That split is what
lets the confirmation card (§5.2) exist at all.

### 5.1 `resolveProvider` — one function, two entry points

Rather than bolting a second path beside `resolveSharedInfra`, both collapse
into one lookup that differs only in which row it finds:

```go
// ref == nil  → the platform stack (today's default, semantics unchanged)
// ref != nil  → the stack the code opens
func (h *SetupHandler) resolveProvider(ref *stackRef) (v sharedInfra, provider *model.SetupSession, reason, detail string)
```

This *reduces* the number of code paths that can wire a session to a mediator,
rather than adding one. The platform branch keeps its exact current reasons
(`platform_stack_missing` / `platform_stack_not_ready` /
`shared_infra_unconfigured`).

**Shipped in phase 2**, as `resolveProvider()` — the ref parameter arrives with
the code branch. The refactor split the judgement out of the I/O:
`providerInfra(*SetupSession)` decides whether a candidate row is usable and is
pure, so it can be tested directly and so a code-named provider is held to
*the same* readiness bar rather than a second copy of it that drifts.

Two pre-existing bugs surfaced while doing it, both fixed there:

- **The fail-open on a DB error reached `POST /setup`.** `resolveSharedInfra`
  returned `ready=true` with a zero `sharedInfra` when the lookup itself failed,
  on the stated reasoning that "create re-reads the row anyway". It did not
  re-read: it used those empty values, so a transient database error could
  create a session with no mediator DID and a `vta_did_url` of `/<name>-vta`.
  The lookup failure is now its own reason, `provider_lookup_failed`, and the
  two callers part company on it — `GET /setup/availability` still fails open,
  because a blip must not blank the create screen, while `POST /setup` refuses,
  because there the choice is between waiting and provisioning a dead agent.
- **The `ServerURL == ""` guard was unreachable.** It tested the output of
  `DidsURL()`, which always prefixes `https://` and so is never empty; a
  provider row with no dids hostname produced `https://.`, passed the check, and
  got snapshotted onto the session permanently. It now tests the two components
  the hostname is built from.

The code branch, in two tiers. **Everything before the code is verified must
answer identically**, or the endpoint becomes a directory of which stacks exist
and which are shared — precisely what §11.2 says there will not be.

**Tier 1 — is this code usable at all.** One reason for every outcome:

| Check | |
| --- | --- |
| `kind` / `v` recognised, fields present, `code` well-formed (§4.1.1) | 400 `bad_bundle` |
| `farm` matches `CLUSTER_DOMAIN` | 422 `wrong_farm` |
| `SELECT … WHERE vta_name = ? AND mode = 'full_stack'` **and** `share_code` non-NULL **and** constant-time equal | **403 `invalid_bundle`** |

No such stack, a stack that never shared, a stack that turned sharing off, a
rotated code, and a hand-mangled code all produce the same 403. They are the
same fact from the holder's point of view — *this code does not currently open
anything* — and the only honest next step for all five is the same one: ask the
owner for a current code.

`bad_bundle` and `wrong_farm` stay distinct because neither requires knowing
anything about our data to determine.

**Tier 2 — the caller holds a valid code**, so specificity costs nothing:

| Check | Failure |
| --- | --- |
| `status = 'running'`, `mediator_did <> ''`, `did_hosting_did <> ''` | 409 `stack_not_running` |
| connection count below the cap (§6.3) | 409 `stack_at_connection_limit` |

Then, and only then, `sharedInfra` is built **from the provider row** —
`provider.MediatorDid`, `provider.DidsURL()`, `provider.DidsURL()` — and the
existing create path continues untouched.

No check reaches the network. Reachability is proven a moment later by
`Factory.For()` fetching `/api/server-info` from a host we provisioned.

### 5.2 The code has to be checkable before the form is filled in

`POST /api/v1/setup/connection/validate` — user auth, rate-limited via the
existing `middleware.RateLimit`, runs §5.1 and creates nothing.

| Outcome | Body |
| --- | --- |
| passes | `{"stack": "alice", "mediator_did": …, "did_hosting_server_url": …, "connections_used": 2, "connections_max": 10}` |
| fails | the same `reason` + `detail` `POST /setup` would have returned |

This exists because of a flaw in the obvious design, and it is worth naming.
A code carries no information — the stack's name, its mediator and its DID host
are facts this server holds and the sender never transmits — so a client has
nothing to render from except this response. That is the property worth keeping:
with a JSON bundle, a client *could* build the card from the paste and show a
confident tick for a code that is pure garbage, discovered only after naming the
agent, picking an image and pressing Create. There is now nothing to build it
from but the truth.

The validate route makes the confirmation card render **values this server read
from its own database**, which is the only version of that card worth showing.
The check that matters — §5.1 tier 1 — happens at paste time, where a wrong code
costs one field to fix rather than a whole form.

Three notes:

- **It is an oracle, and that is fine.** 75 bits of entropy behind an
  authenticated, rate-limited route is not brute-forceable, and tier 1's single
  reason means the oracle answers exactly one question: *does the code I was
  given work.* That is the question it exists to answer.
- **It is not authoritative.** `POST /setup` re-runs §5.1 in full. A stack can
  stop running, rotate its code or fill up between the two calls, so validate is
  a courtesy and create is the gate. Never skip the create-time check because
  validate passed.
- **This reverses an earlier decision.** A "test this code" *button* was
  rejected as a worse copy of create. That reasoning was wrong once the
  confirmation card was in the design: the card is not a test the user opts into,
  it is a claim the UI makes unprompted, and it must not be a claim the server
  never checked.

### 5.3 One code, one stack

Taking the mediator from stack A and the DID host from stack B is refused
implicitly: the code opens one stack and every value comes from that row.
Splitting them is technically possible over public HTTPS, but the dids daemon
bakes `[identity] mediator_did` into its own recipe at provisioning time, so the
pair is meaningful. Keeping it atomic also keeps §7's dependency tracking to a
single nullable column instead of a join table.

---

## 6. Schema

```sql
-- migrations/000025_stack_connection.up.sql

-- Provider side. NULL means "not shared" — one column rather than a boolean
-- plus a code, because the two would always have to agree and the code alone
-- answers both questions (design §4.1).
ALTER TABLE setup_sessions ADD COLUMN share_code TEXT NULL;

-- Consumer side. Neither column is needed to RUN the session — the three
-- snapshotted values in mediator_did / did_hosting_*_url already do that.
-- They exist for the three things a snapshot cannot answer: finding dependents
-- cheaply (design §7), naming the provider in the UI, and letting support
-- answer "why is this agent dead" without correlating URLs by eye.
ALTER TABLE setup_sessions
    ADD COLUMN connection_source TEXT NOT NULL DEFAULT 'platform'
        CHECK (connection_source IN ('platform', 'in_farm')),
    ADD COLUMN provider_session_id BIGINT NULL
        REFERENCES setup_sessions(id) ON DELETE SET NULL;

CREATE INDEX setup_sessions_provider_idx
    ON setup_sessions (provider_session_id)
    WHERE provider_session_id IS NOT NULL;
```

**Shipped in phase 1**, plus one backfill the table above does not show. `
did_hosting_did` has been a `full_stack` output column (the daemon's own DID)
and is `''` on every `vta_only` row. Its meaning widens here to "the DID of the
daemon at `did_hosting_control_url`", true for both modes, which is what
`Factory.For`'s audience check (§9.4) compares against. Existing `vta_only` rows
are joined to the daemon they actually point at — matching on
`did_hosting_server_url` rather than assuming the platform stack — so a row
whose daemon no longer has a row keeps `''` and therefore keeps "no expectation
on record" rather than being handed a DID that was never its daemon's.

The down migration does not reverse that backfill: it restores the schema, not a
snapshot of the data, and the value is correct independently of this feature.

### 6.1 `ON DELETE SET NULL` is the whole orphan mechanism

Not a fallback — **the** mechanism. §7 blocks nothing and writes nothing at
delete time; when a provider row goes, Postgres nulls every dependent's
`provider_session_id` in the same transaction, and

```sql
connection_source = 'in_farm' AND provider_session_id IS NULL
```

is exactly and permanently "the stack this agent connected to is gone". The UI
reads it (§8), nothing has to have been running at the moment of deletion, and
there is no reconciler to drift.

`RESTRICT` would mean §7.4's rejected design. A plain `NULL`-able column with no
FK would mean writing the orphan marker by hand in every delete path — user
delete, admin delete, cascade from a user deletion — and getting it wrong in the
one nobody tested.

### 6.2 Why the CHECK admits only two values

`'external'` is not reserved in the constraint. Adding it later is a one-line
`ALTER`, and leaving it out now means the schema states the same scope §1 does
instead of hinting at a path the code cannot take.

### 6.3 A cap on connections per stack

`MAX_STACK_CONNECTIONS`, default `10`, `0` = unlimited.

Crude, and deliberately so. It is not a capacity model — §9.2 explains why there
isn't one — it is a bound on how much of somebody else's storage and message
volume a single share code can commit before a human notices.

It carries more weight than it looks, because §7.4 leaves a provider **no way to
remove one connection**. Rotating the code stops new ones; the cap is what
limits how many arrived before they thought to. An admin can raise it globally;
a per-stack override is §11.4.

### 6.4 No new session status

An orphaned consumer stays `running`, because it is: the Deployment, Service and
Ingress are in the consumer's own namespace and nothing in a provider teardown
touches them. The pod serves; it just cannot resolve its own DID or reach a
mediator.

A `disconnected` status was considered and dropped. It would have to be written
by whichever code path deleted the provider, which is exactly the synchronous
marking §6.1 removes — and it would claim the pod had stopped, which is a
different and larger lie than a `running` badge next to an explicit "the stack
this agent connected to no longer exists" (§8).

So no change to `STATUS_META`, no change to the `SetupStatus` union, and no
`Orchestrator.Resume` query to re-check.

---

## 7. Lifecycle

Two operations, one of which needs no code at all.

### 7.1 The consumer deletes itself

Unchanged, and already correct. `teardownSession` calls `DeleteDid` /
`DeleteAcl` through `session.DidHostingControlURL` — the daemon the DID was
actually uploaded to — and treats failures as warnings. The "snapshotted, not
looked up" rule was written for platform-stack rebuilds; it is what makes a
third-party provider work without a line of new code.

One change: those warnings stop being rare. A consumer whose provider is gone
fails both calls on every delete. They belong in the delete response as a
non-fatal note, not buried in `log.Printf` — the user is entitled to know their
DID may still be published somewhere.

### 7.2 The provider deletes itself — allowed, with a warning

Unchanged behaviour: the delete goes through. Its dependents keep running in
their own namespaces, degraded — the mediator and daemon are gone, so the VTA
can no longer resolve its own DID or route a message, but nothing about the
consumer's Deployment, Service, Ingress, PVC or Vault seed is touched.

The only change is at the UI layer: because `GET /setup/:id` reports
`connections[]` (§8), the delete confirmation can name what it is about to
break. That is the whole mitigation, and it is a confirmation, not a gate.

`ON DELETE SET NULL` (§6.1) marks the orphans as a side effect of the delete
itself. No dependent is written to, no status changes, no code runs.

### 7.3 The consumer finds out on its next page load

There is no notification channel (the one under discussion for uptime monitoring
is a different thing), so the signal is a query, not an event:
`connection_source = 'in_farm' AND provider_session_id IS NULL` (§6.1). The
agent's detail page renders it as "the stack this agent connected to no longer
exists"; the user deletes the agent when they are ready.

Deleting an orphaned consumer works: `teardownSession` reaches for a daemon that
is gone, both calls fail, and both are warnings (§7.1). Everything in the
consumer's own namespace is cleaned up normally.

### 7.4 Rejected: a delete guard, and per-connection revoke

An earlier draft had `DELETE /setup/:id` answer **409** while connections
existed (the precedent being `DELETE /api/v1/domains/:id`), plus a
`DELETE /setup/:id/connections/:name` for the provider to eject one consumer.

Both are out, and they had to go together. The 409 alone is a trap: a provider
who cannot remove a connection and cannot delete their stack while one exists is
pinned forever by a single consumer they never met. Revoke existed only to
unpin them.

Dropping the pair rests on one fact: **a provider teardown destroys nothing of
the consumer's.** The pod keeps running, the seed stays in Vault, the namespace
stays. There is no data loss to prevent, so there is nothing for a hard gate to
protect — only a surprise to prevent, and a confirmation dialog does that.

What this costs, stated plainly: **a provider has no way to remove one
connection.** Their levers are rotating the code (stops new arrivals, §4.1) and
deleting the stack (stops everyone). §6.3's cap is the only thing bounding how
many can arrive in between. If an abusive-consumer case ever turns up, revoke is
additive — §7.2 blocks nothing, so adding it later breaks no behaviour anyone
depends on.

---

## 8. Surfacing it

| Where | What |
| --- | --- |
| `GET /setup/:id` (consumer, `vta_only`) | `connection_source`, and when `in_farm`: the provider's name, or an explicit "gone" when `provider_session_id IS NULL` |
| `GET /setup/:id` (provider, `full_stack`) | `share_code` (§4.3) + `connections[]` — the dependents' names and statuses |
| `GET /admin/setup-sessions` | a provider column, so support can see the topology without a query |

The provider's dependent list is not decoration — it is the entire mitigation
for §7.2. Deleting the stack is allowed and breaks every agent on that list, so
the list has to be visible from the page where Delete lives, and named in the
confirmation.

Names and statuses only. The dependents belong to other users; nothing else
about them is the provider's business.

---

## 9. Conflicts and risks

Ordered by damage. Two of the four from the pre-scoping draft are gone: SSRF and
credential relay (§9.4) collapse into "not reachable" under §3, and the
third-party-ACL blocker is now the scope boundary rather than a hazard.

### 9.1 `RegisterDid` failing is silent — a latent bug this feature makes reachable

**Severity: high. Independent of this feature; must be fixed with it.**

In `orchestrator.go`, the `RegisterDid` error path logs and carries on. The
session reaches `running` with a DID that was never published: green UI, dead
agent.

Today that needs a platform stack whose ACL entry went missing — rare, and an
operator's problem. After this feature, every ordinary user can aim a session at
a stack whose daemon might be mid-restart, and hit it.

The fix is not part of the connection flow but must ship with it: a failed DID
upload has to fail the session, or at minimum leave a visible marker on the row.
§5.1's checks reduce how often it happens; they cannot make a silent failure
acceptable.

**Shipped in phase 0.** Every failure in the upload block — no DID log parsed,
no `vta_did_url`, no client for the control URL, `RegisterDid` itself — now
calls `markFailed`. The one exception is `didHosting == nil`, which stays a
warning: that is a deployment-wide "no keypair configured" state rather than a
property of the session, `runProvision` already treats it the same way, and
failing on it would break every local environment that runs without one.

**One gap remains, and it is pre-existing.** The upload runs *after* the row is
written to `vta_setup_complete`, so a crash in between leaves a session whose
DID was never published and which nothing retries. The ordering cannot simply be
reversed: `Resume` re-runs sessions in `vta_setup_running`, and
`registerAtomic` sends `force=false` and errors on any non-2xx, so a replayed
upload against an already-published path would fail — turning a crash-recovery
into a dead session. Closing this properly means making the upload idempotent
first (the way `CreateAcl` already treats 409 as success), which is a change to
what we assume of the daemon's contract and wants its own verification against
the daemon source. Not attempted here.

### 9.2 Cross-tenant coupling is real and barely mitigated

**Severity: high. §7.4 deliberately declines to gate it.**

An agent's liveness now depends on a resource owned by someone whose incentives
are not aligned:

| Failure | Handled? |
| --- | --- |
| Provider deletes their stack | **warned, not blocked** — §7.2; consumer learns on next load, §7.3 |
| Provider's stack breaks, or they upgrade to an image that breaks the mediator | **no** — dependents degrade with no signal at all |
| Provider's PVC fills with DID logs they did not create | **partly** — §6.3 bounds the count, not the volume |
| Provider's mediator carries message volume they did not generate | **no** |
| Consumer misbehaves and the provider wants them gone | **no** — §7.4; the levers are rotate, or delete the stack |

The second row is the one to keep in mind: a *deleted* provider is the case with
a clean signal, and it is the least likely failure. A provider whose mediator is
merely broken produces a consumer that looks perfectly healthy in the portal and
silently delivers nothing. Nothing in this design detects that, and the honest
statement is that liveness monitoring of a consumer's actual messaging path does
not exist for the platform stack either.

`capacity.VtaOnly` remains correct for the consumer's *pod*, which is the only
thing landing in our cluster's model. What is unmodelled is the load on the
provider's fixed-size mediator and daemon.

This is acceptable for a feature aimed at people sharing with people they know,
which is what §1 and §4.1 constrain it to. It would not be acceptable behind a
public directory — which is why §11.2 says there will not be one.

### 9.3 A stale code

**Severity: medium. Caught twice.**

A rebuilt stack is a *different daemon* with a different `did_hosting_did` and an
empty ACL. Deleting and recreating a stack produces a new row with a new share
code, so an old code simply fails to resolve — there is no row for it to find.
The first cut also compared three display values as a second line of defence;
dropping the bundle dropped the belt and kept the braces, which is the half that
was actually load-bearing.

### 9.4 SSRF and credential relay — closed by construction

**Severity: was high. Now not reachable.**

Recorded because it is the reason §3 is written the way it is, and because the
first change that makes URLs sender-supplied reopens all of it:

1. **SSRF** — a pasted URL turning this server into a probe of link-local, cloud
   metadata or in-cluster addresses.
2. **Credential relay** — `New()` takes the `server_did` a remote host *claims*
   and uses it as the `aud` of tokens signed with the farm's admin key. A
   hostile host claiming another daemon's DID gets a token it can replay there.

Under §3 neither is reachable: we connect only to hosts we provisioned. Worth
doing anyway, as defence in depth and because it protects the platform path
too — pass the expected `did_hosting_did` into `Factory.For()` and refuse a
mismatched `/api/server-info`. Cheap, and it means §11.1 starts from a
`didhosting` that is already safe.

**Shipped in phase 0.** `Factory.For(controlURL, expectedServerDid)` refuses a
daemon whose self-reported DID is not the expected one; `""` means "no
expectation on record" and accepts anything, which is the state of every
`vta_only` row until phase 1 backfills `did_hosting_did`. The check runs on
cache hits too — otherwise one unverified call would disarm it permanently for
that URL — and a mismatch does not evict the cached client, because a mismatch
says the *caller's* expectation is wrong, not that the client is unusable.

### 9.5 Untrusted TLS

**Severity: low under §1.**

`CLAUDE.md` records the rule the hard way: components resolve each other's
`did:webvh` over HTTPS and reject an untrusted chain — a staging certificate
passes `tls_provision` and then crash-loops the mediator. Every in-farm stack is
covered by our wildcard or by cert-manager, so this is only a hazard for
§11.1. `didhosting.New()`'s real handshake is the pre-flight either way.

### 9.6 Name collisions get a worse error, not a bug

**Severity: low. The message lies; nothing breaks.**

`setup_sessions_did_path_unique` is `(did_hosting_server_url, vta_name)` — it
already scopes per daemon, so pointing at a second daemon is what it was built
for. But `setup_sessions_vta_name_unique` is **global**, so two users still
cannot both call their agent `main`, and the error says `vta_name already in use`
with no hint that the other holder is on a stack they have never heard of.

Do **not** relax the global index. The admin routes resolve a session by name
with no `user_id`, and the whole "a session is addressed by its name" design
rests on it. Fix
the message.

One collision the global index catches by accident and must keep catching:
connecting a `vta_only` named `alice` to a provider whose own session is named
`alice` would mint `alice-vta` on a daemon already serving `alice-vta`. Both
rows carry `vta_name = 'alice'`, so the global index refuses it. The provider's
`alice-mediator` / `alice-vtc` paths are unindexed but can never be produced by
a `vta_only`, which only mints `-vta`. The suffix convention is doing
load-bearing work, exactly as `vta-setup-design.md` claims.

### 9.7 The mediator accepts any VTA — resolved

**Severity: none. Settled by product decision; no work.**

The VTA's config carries only `[messaging] kind = "existing", did = <mediator>`.
The open question was whether the mediator grants mediation to any DID that asks
or keeps its own allow-list — if the latter, provisioning would have needed a
seventh step in §5.1 and a matching teardown action.

**Decision: mediation is open to every DID.** No allow-list, so a consumer's VTA
needs no admission on the provider's mediator and nothing has to be withdrawn
when it goes away.

Two consequences worth keeping visible:

- The mediator is not an access-control point for this feature. The **share
  code is the only gate** (§4.1) — once someone has connected, the mediator will
  keep serving them regardless of what the provider does with the code
  afterwards. §7.4 already says the same thing from the other direction.
- A wrong or rotated code is refused at create time and never again. There is no
  second checkpoint at runtime, which is exactly why §5.1's checks are the ones
  that have to be right.

### 9.8 Blast radius on first release

**Severity: process.**

`full_stack` is behind `users.beta_access`, so only beta users can *provide*.
Consuming has no gate. Putting Customize behind the same flag for its first
release keeps the set of people who can create a cross-tenant dependency equal
to the set who already understand the stack. One condition in `POST /setup`,
easy to remove later, awkward to add after the fact.

---

## 10. Work breakdown

### vtafarm-api

| # | Change | Files |
| --- | --- | --- |
| 1 | Migration (§6) | `migrations/000025_stack_connection.{up,down}.sql` |
| 2 | Model fields, `Connection()` builder, `IsShared()` | `internal/model/setup_session.go` |
| 3 | Share code: mint, normalise, check symbol, constant-time compare (§4.1.1) | `internal/setup/sharecode.go` (new) + test |
| 4 | `resolveProvider` replacing `resolveSharedInfra` (§5.1) | `internal/handler/setup.go` |
| 5 | `connection` request field + create wiring | `internal/handler/setup.go` |
| 6 | `PUT /setup/:id/sharing` (+ admin twin) | `internal/handler/setup.go`, `setup_admin.go` |
| 7 | `POST /setup/connection/validate` (§5.2) | `internal/handler/setup.go`, `router/router.go` |
| 8 | `connection` + `connections[]` in provider responses; `connection_source` + provider name in consumer responses (§8) | `setup_fullstack.go`, `setup.go`, `admin.go` |
| 9 | Availability split (§10.1 below) | `internal/handler/setup.go` |
| 10 | Make `RegisterDid` failure visible (§9.1) | `internal/setup/orchestrator.go` |
| 11 | Expected-audience check (§9.4, defence in depth) | `internal/didhosting/{factory,client}.go` |
| 12 | **Every new route documented** | `internal/apidocs/openapi.yaml` |
| 13 | `MAX_STACK_CONNECTIONS` | `internal/config/config.go`, `.env.example` |

Nothing in the delete path changes (§7.2), and there are two new routes (#6,
#7). §9.7 removed the mediator prerequisite entirely; §7.4 removed two endpoints,
a status, and every handler branch that would have had to write to another
user's session row.

#3 and #4 are the two that need tests rather than review: the share code's
normalisation table (§4.1.1) and §5.1 tier 1 answering identically for all five
of its inputs. Both are the kind of thing that works when written and quietly
stops working later.

### 10.1 The availability gate has to split — decided

`GET /setup/availability` reports `vta_only.available: false` with reason
`platform_stack_missing` when there is no platform stack, and `POST /setup` 503s
to match. Once Customize exists that is no longer the whole truth.

**Decision: a farm with no platform stack can still create a `vta_only`, as long
as the caller brings a code.** The platform stack is a default, not a
prerequisite for the mode. That is what forces the gate to split rather than
just relax: `platform_stack_missing` stays a true and useful statement about the
default path, and blocking the whole mode on it stops being one.

```jsonc
"vta_only": {
  "count": 3,
  "available": false,             // still means: the DEFAULT path
  "reason": "platform_stack_missing",
  "detail": "…",
  "custom_target_allowed": true   // ← new; false only when capacity is exhausted
}
```

`POST /setup` mirrors it: the `resolveSharedInfra` 503 applies only when no
`connection` was sent.

### vtafarm

See the companion frontend doc. Both repos branch as
`feat/vta-only-custom-stack` and must be reviewed together.

### Documentation

- `vta-setup-design.md` §"Open: a user-supplied DID host" — narrow to §11.1 and
  point here.
- `CLAUDE.md` — add this doc to the `docs/` table, and extend "Shared
  infrastructure comes from the platform stack", whose title stops being the
  whole truth the day this ships.

---

## 11. Open questions

### 11.1 Stacks outside this farm

Out of scope (§1). What it needs, unchanged from
`vta-setup-design.md` §"Open: a user-supplied DID host": either publish our
client DID and have the operator enroll it (no new secrets, but one identity
holds admin across every user's daemon), or mint a keypair per session and
enroll that (contained and revocable, but a private key per session, which
belongs in Vault beside the master seed).

It would also reopen every mitigation in §9.4 and make §9.5 load-bearing, and it
would have to replace §5.1's DB lookup with something else entirely — the
sender's values would become configuration again, losing §3. Treat it as a different feature
that happens to share a UI, not as a later phase of this one.

### 11.2 There will not be a stack directory

Not an open question so much as a decision worth recording. Browsing or
searching stacks that accept connections would turn §9.2's coupling from a
favour between two people into a marketplace with no quota model behind it. The
share code (§4.1) exists precisely so that connecting requires somebody to have
chosen you.

### 11.3 Migrating a live agent between stacks

Out of scope, and probably not buildable as stated: the VTA's `did:webvh`
contains its host, so moving it mints a new DID — a new identity, not a
migration. The honest answer for a user whose provider disappeared is "create a
new agent", and the UI should say that plainly rather than implying a Move
button could exist.

### 11.4 Per-stack connection limits

§6.3 is a global cap. A provider who wants to host thirty agents, or exactly
one, has no way to say so. A `max_connections` column would be trivial; whether
anyone wants it is unknown until the feature has users.

### 11.5 Removing a single connection

Rejected for now (§7.4) because there is no data loss to prevent and it existed
only to unpin a delete guard that is also gone. It becomes worth revisiting the
first time a provider actually wants a specific consumer off their stack — the
`DeleteAcl` + `DeleteDid` pair is already how teardown works, so the mechanism
exists. It is additive: nothing in §7 blocks or writes anything today, so adding
it later changes no behaviour anyone depends on.

### 11.6 Should the platform stack become an ordinary provider?

§5.1 already unifies the *lookup*. The remaining asymmetry is that the platform
stack has no share code and is reached by a nil ref. Collapsing that too —
giving it a code and making the default path just a preselected one — would
remove the last special case, at the cost of touching the one path every
existing session depends on. After this ships, not with it.

---

## 12. What has shipped

Nothing yet.

| Item | Status |
| --- | --- |
| §9.7 mediator allow-list | ✅ resolved — open to every DID, no work |
| Migration + model (§6) | ✅ phase 1 |
| Share code: mint / normalise / validate / compare (§4.1, §4.1.1) | ✅ phase 1 |
| `resolveProvider`, platform path (§5.1) | ✅ phase 2 |
| `resolveProvider`, code tiers (§5.1) | ✅ phase 4 |
| `POST /setup/connection/validate` (§5.2) | ✅ phase 4 |
| `connection` on `POST /setup` (§5) | ✅ phase 4 |
| Sharing toggle: `PUT /setup/:id/sharing` (+ admin twin) | ✅ phase 3 |
| `connection` + `connections[]` in responses (§8) | ✅ phase 3 |
| Availability split (§10.1) | ✅ phase 4 |
| `RegisterDid` failure visible (§9.1) | ✅ phase 0 |
| Expected-audience check (§9.4) | ✅ phase 0 |
| Frontend Share + Customize | ☐ |
| openapi.yaml | ✅ phases 3–4 |
