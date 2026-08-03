# Platform Stack Admin Grant — Design

Lets an admin add a second (third, …) **super admin** to the platform stack's
VTA from the admin panel, with exactly the authority the stack's first admin
DID got at provisioning time.

**Self-service is the point.** A co-admin pastes the `did:key` their own
`pnm setup` minted and the grant happens; nobody has to collect DIDs by hand and
run `pnm acl create` for each one.

**Adding only.** Removing an admin is `pnm acl delete <did>` against the running
VTA — no downtime, and nothing here has to answer "which entry is whose", which
§7.2 shows this side cannot answer well.

Today that authority is minted once, in the middle of the pipeline, and never
again: `fsStepImportAdminDid` runs `vta import-did --role admin` against the
VTA's fjall store while the VTA is still down, and after `deploy_vta` there is
no route to a second one. The only way to add a co-admin is for the existing
admin to run `pnm acl create` from their own workstation — which works, but is
invisible to the farm and unavailable to anyone who does not already hold the
credential.

> **§7 is the section to read first** — it is where this design can go wrong.

Prerequisite reading: [`full-stack-setup-design.md`](full-stack-setup-design.md)
§"Phase 2", and `CLAUDE.md` §"The platform stack".

---

## 1. Scope

| In scope | Out of scope |
| --- | --- |
| The **platform stack** only — one session, owned by the `platform` system account | every other `full_stack`; every `vta_only` (§10.1) |
| Granting `role=admin` with **no contexts** — unrestricted, identical to `pnm-bootstrap` | context-scoped grants, `initiator`/`application`/`reader`, expiry (§10.2) |
| Adding an admin | **removing** one — that is `pnm acl delete` against the live VTA (§10.4) |
| A record of **what was added from here** | any copy of the VTA's own admin list (§7.1) |
| Accepting ~60–120s of VTA downtime per operation (§3) | zero-downtime grants (§10.3 records what that costs) |

---

## 2. What "the same permission as the first admin_did" means

Precisely: `Role::Admin` with an **empty** context list.

`fsStepImportAdminDid` (`internal/setup/orchestrator_fullstack.go:1071`) passes
no `--context`:

```go
cmd := fmt.Sprintf("vta import-did --role admin --label pnm-bootstrap --did %s", shellQuote(adminDid))
```

`vta import-did` writes that empty list through unchanged
(`vta-service/src/import_did.rs`, `.with_contexts(args.context.clone())`) and
prints `Contexts: unrestricted`. Server-side that is the definition of a super
admin (`vti-common/src/auth/extractor.rs:258`):

```rust
pub fn is_super_admin(&self) -> bool {
    self.role == Role::Admin && self.act_scope().is_unrestricted()
}
```

So this feature emits **the identical command** with a different `--did` and
`--label`. There is no new authority model to design, and nothing to keep in
sync with the pipeline: if the pipeline's grant ever changes shape, one grep for
`import-did` finds both call sites.

Note the contrast with `fsStepVtcAclGrant`, which is deliberately *narrow* —
`--context <vtc> --admin-expires 1h`. The codebase already distinguishes a
short-lived machine identity from a stack owner. This feature mints the latter,
on purpose, because co-management is the stated goal.

---

## 3. Transport: offline, through the PVC

The VTA's ACL lives in a fjall store on a `ReadWriteOnce` PVC
(`internal/k8s/component_jobs.go:64`), and the running VTA holds a store-level
lock on it. A Job cannot write that store while the Deployment is up — it fails
with `store error: FjallError: Locked`, and on a different node it cannot even
mount the volume.

So every operation is: **scale to 0 → wait for the pod to actually terminate →
Job → scale back to 1**.

This is not a new pattern. `reissueDidsEnroll`
(`internal/handler/setup_fullstack.go:526`) does exactly this against the dids
daemon, including the `defer` that brings the Deployment back up whatever
happens. That handler is the template; this one differs only in which Deployment
it stops and which binary the Job runs.

**Blast radius of the window.** The mediator and the dids daemon are separate
Deployments and stay up, so no `vta_only` session loses its mediator or its DID
host. What is down is the platform stack's own VTA, and the VTC that
authenticates against it. For an operation performed a handful of times in the
life of an environment, that is the right trade — §10.3 records what buying it
back would cost.

---

## 4. Data model

Migration `000027_vta_admin_grants`.

```sql
CREATE TABLE vta_admin_grants (
    id          BIGSERIAL PRIMARY KEY,
    session_id  BIGINT NOT NULL REFERENCES setup_sessions(id) ON DELETE CASCADE,
    did         TEXT   NOT NULL,
    label       TEXT   NOT NULL DEFAULT '',
    status      TEXT   NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','granted','failed')),
    error_msg   TEXT   NOT NULL DEFAULT '',
    requested_by BIGINT NULL REFERENCES admins(id) ON DELETE SET NULL,
    granted_at  TIMESTAMPTZ NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One live grant per DID per session. Partial, so a failed attempt stays as
-- history and the same DID can be retried without deleting the first record.
CREATE UNIQUE INDEX vta_admin_grants_live_unique
    ON vta_admin_grants (session_id, did)
    WHERE status IN ('pending','granted');
```

Named for the session, not for the platform stack, because the mechanism is
session-generic and only the *route* is narrowed (§1). Generalising later is a
route addition, not a migration.

`ON DELETE CASCADE`: the grants describe a store that is deleted with the
session. There is nothing to orphan.

**No `role` or `contexts` column.** Every row is an unrestricted admin — that is
the feature. Adding a column that only ever holds one value invites a second
value without the authorization work §7.4 would need.

---

## 5. API surface

All admin-cookie only, all under the existing `/api/v1/admin` group.

| Method | Path | Notes |
| --- | --- | --- |
| `GET` | `/api/v1/admin/platform-stack/admins` | The grant rows — what was added from here. Never blocks, never causes downtime. |
| `POST` | `/api/v1/admin/platform-stack/admins` | `{did, label, confirm}` → grants. Synchronous; 60–120s. 409 while another grant holds the window (§7.6). |

`confirm` must equal the platform stack's label, mirroring the guard on
`DELETE /admin/setup-sessions/:id` and enforced at the API, not the UI. It is
the speed bump on an irreversible privilege grant that also takes production
down for a minute — see §7.4 for why a speed bump and not a second approver.

Validation on `did`: must start with `did:`, must be `did:key:` (the VTA's DI
proof verifier is `did:key`-only, so anything else produces an ACL entry that
can never authenticate), and must not already hold a live grant (409).

Synchronous, following `reissueDidsEnroll`. The row is written `pending`
**before** the k8s work starts, so a client that times out at an ingress proxy
has not lost the operation — `GET` still shows it, and it lands `granted` or
`failed` regardless of who is listening.

---

## 6. The operation

One helper, `runVtaAclJob(ctx, session, cmd)`.

```
0. refuse unless domain_type = platform (§1); TryLock or 409 (§7.6)
1. resolve ns, deployment name (k8s.FSVtaName), selector "app=fs-vta,session-id=<id>"
2. ScaleComponentDeployment(vta, 0)
3. defer: ScaleComponentDeployment(vta, 1) + WaitForComponentDeploymentReady
          — unconditionally, exactly as reissueDidsEnroll does
4. WaitForComponentPodsGone(selector, 2m)
5. DeleteComponentJob(jobName)         — clear a previous TTL'd run
6. CreateComponentJob(image: session.VtaImage, PVC: vta-data, cmd)
7. WaitForJob → JobLogs
8. parse, persist
```

The `cmd` does one thing — import, unless the DID is already there:

**Grant** — `vta import-did` prompts `Overwrite?` through `dialoguer` when an
entry already exists (`vta-service/src/import_did.rs`), and `interact()` on a
pod's non-TTY stdin **errors**, failing the Job. So probe first, the way
`fsStepVtcAclGrant` probes with `list-acl`:

```sh
if vta acl get <did> >/dev/null 2>&1; then
  echo "VTAFARM_ALREADY_PRESENT"
else
  vta import-did --role admin --label <label> --did <did>
fi
```

`VTAFARM_ALREADY_PRESENT` is not an error — it is the idempotent outcome, and
the row still lands `granted`.

A condition's exit status does not trigger `set -e`, so the probe stays a test
rather than a failure. `set -e` itself is now defensive rather than load-bearing
— the import is the last command, so its status is the script's — and it stays in
front of whoever appends a line next, since a trailing command would otherwise
mask a failed import and have the API report a grant that never happened.

---

## 7. Where this can go wrong

### 7.1 No copy of the ACL is kept

An earlier draft stored one — captured as a by-product of the grant Job, which
could run `vta acl list` inside a window already being spent. It is gone, and the
reason is worth recording because the idea looks free.

It was never free:

- **It could not be trusted to be current.** It only advanced when someone
  granted, so it could be weeks old, and a co-admin's rotation invalidates it
  minutes after the grant that captured it (§7.2). Every surface showing it had
  to date it and explain why it was old.
- **It carried the only unvalidated component.** Parsing `vta acl list` output
  is the one part of this feature that cannot be checked without the real
  binary, and it existed solely to feed that copy.
- **Its one logical consumer went with revoke.** The last-admin guard was the
  only thing that ever *decided* anything on it; after §10.4 it fed a display.

What replaces it is `pnm acl list` against the running VTA: live, exact, no
downtime, and already in the hands of everyone who can act on this. The API
records what it did — grants — and points at `pnm` for what is true now.

The cost, stated plainly: nothing in the product shows the admins this API did
not add, which is the `pnm-bootstrap` entry from provisioning plus anything added
out of band. That is a real gap, and the answer to it is a terminal.

### 7.2 The submitted DID goes stale almost immediately

The DID a co-admin sends is the **temporary** `did:key` that `pnm setup` minted
and parked in their keyring. On their first authenticated command PNM rotates:
`POST /acl/swap` atomically moves the entry — same role, same contexts — onto a
fresh long-lived DID and deletes the temp (`vta-sdk/src/session.rs:1107`).

So minutes after a successful grant, `vta_admin_grants.did` names a DID that is
no longer in the ACL, while the co-admin holds full super admin under a DID this
farm has never seen.

This is not a bug to fix, it is the protocol working. What follows from it:

- A grant row is **a record of an event**, not a statement of current access.
  The UI must label it that way (`granted <date>`) and must not present it as
  the admin list.
- The **label is required** for exactly this reason. `POST /acl/swap` carries
  role, contexts and label onto the new entry
  (`vta-service/src/operations/acl.rs`, `with_label(old.label.clone())`), and of
  those the label is the only human-readable one. Grant without it and the ACL
  holds a did:key nobody can attribute to a person.
- Nothing on this side follows the DID to where it moved. `pnm acl list` against
  the running VTA is what answers "who can act on this now", and it is exact
  where any stored copy would be a guess.
- **This is why removal is not built here.** Deleting a granted DID after a
  rotation would remove nothing; a removal that works must target a DID from the
  the VTA's live ACL — which this side does not hold and deliberately does not
  try to.

  It is the strongest argument for keeping removal out of scope (§10.4).
  Attributing a rotated entry to a person is not something this side can do:
  `vta acl list` prints DID / role / label / contexts / created, so the only
  handle is the **label** — which the swap does preserve
  (`with_label(old.label.clone())`), and which is why the API requires one. An
  operator at a `pnm` prompt reads that label with the whole ACL in front of them
  and decides. A form cannot do better, and would take a maintenance window to do
  worse.

### 7.3 Nothing stops the last admin being removed

The VTA has no last-admin protection — only "solo admins can assign the admin
role" (`vti-common/src/acl/mod.rs:797`). Nothing refuses a delete that empties
the ACL, and nothing stops a new super admin removing the one who granted them.

Since removal is out of scope (§10.4), this is not a guard this feature can
usefully supply — the deletes happen at a `pnm` prompt, where nothing this API
does can intercept them. It is recorded because granting is what *creates* the
exposure: every grant adds someone able to do it.

Recovery, if it happens, is an offline `vta import-did` against the PVC — which
is exactly the machinery this feature already runs. Lockout is survivable here
because we hold the cluster.

### 7.4 This makes a vtafarm admin cookie equivalent to VTA super admin

Today those are separate trust domains. A vtafarm admin can provision and delete
stacks; a VTA super admin can sign, read the vault, and mint keys. After this
feature, any vtafarm admin session can grant itself the second.

That is a real escalation and it should be a deliberate decision, not a side
effect. It is accepted here because the platform stack is the farm's own stack,
run by the same operators, on a cluster where those operators already have PVC
access — the authority exists whether or not there is a button for it.

Two things make it accountable rather than silent:

- `requested_by` records which admin, and the row is permanent.
- `confirm` (§5) means it cannot be a stray click or a CSRF-shaped accident.

A second-approver flow (`pending` → approved by someone already holding a VTA
credential) was considered and deferred: it is the right shape the moment this
generalises to **customers'** stacks (§10.1), where the argument above does not
hold. The `pending` status and `requested_by` column exist so that flow is an
added transition, not a schema change.

### 7.5 A failed scale-back leaves the stack down

The `defer` runs unconditionally, but it can itself fail. `reissueDidsEnroll`
logs and moves on. That is survivable for a dids daemon; for the platform
stack's VTA it should be louder — the deferred restart failing must produce a
distinct error in the response body, not only a log line, so the operator knows
to check rather than assuming the failure was confined to the grant.

---

### 7.6 Self-service makes concurrency real

While one operator held the credential and did this by hand, two overlapping
windows could not happen. Self-service makes them ordinary — two admins click
"add" within the same minute.

That is destructive, not merely racy. Both scale the VTA to 0; both then
`DeleteComponentJob` and `CreateComponentJob` under the **same** name (there is
one per session), so each can delete the other's running Job; and the first
`defer` to fire scales the VTA back up while the other is still writing to the
fjall store it holds a lock on.

Two guards, because one does not cover it:

- `runVtaAclJob` takes a process-wide `TryLock` before anything is scaled.
  `TryLock`, not `Lock`: a caller who queued would sit through one outage and
  then start another, and a queue of these is a queue of outages. Refusing with
  409 says the true thing — nothing is broken, come back in a minute.
- The grant route additionally refuses while a live `pending` row exists for the
  session. The lock is in-process and cannot see a second API replica; the row is
  written before any Kubernetes work and can. Bounded by `aclJobStale` (15 min,
  past the Job's own `ActiveDeadlineSeconds`) so a request that died mid-window
  cannot wedge the route permanently.

Both sit inside `runVtaAclJob` and the grant handler rather than in middleware,
so nothing can reach the window by another path.

## 8. Frontend

`src/pages/admin/PlatformStackView.tsx`, one new section below the existing
stack detail. Client methods in `src/lib/api.ts` alongside `getPlatformStack`.

- **Add admin** — the primary action, and the reason the page exists. A
  `did:key` field, a **required** label ("who is this?"), and a confirm input
  taking the stack label. The copy has to state three things plainly: the grant
  is **unrestricted super admin**, the VTA will be **down for about a minute**,
  and the DID being pasted is expected to change once its holder connects.

  Worth spelling out the flow it sits in, because a co-admin doing this for the
  first time will otherwise stop halfway: run `pnm setup --name <slug>` locally,
  paste the DID it prints here, wait for this to finish, then finish with
  `pnm setup continue <slug> --vta-did <vta_did>`.

  A 409 while another grant is running is not an error state — say "another
  admin is updating the ACL, try again in a minute" and keep the form filled in.

- **Added from here** — the `vta_admin_grants` rows, labelled as events (§7.2).
  Empty on a new stack, and it says why: the first administrator was set during
  provisioning and was never a row here.

  This is the only list on the page, so it also carries the pointer to the real
  one: `pnm acl list` for the VTA's actual administrators, `pnm acl delete <did>`
  to remove one. Both belong next to the rows they qualify, not in a footnote.

## 9. Phasing

1. ~~Migration + model + `runVtaAclJob`, with `GET`.~~ **Done.**
2. ~~`POST` (grant), with the concurrency guards of §7.6.~~ **Done.** Revoke was
   cut — see §10.4.
3. ~~Frontend section.~~ **Done** — `src/pages/admin/PlatformStackAdmins.tsx`,
   rendered from `PlatformStackView` only once the stack is `running`.

Complete. A fourth phase — capturing the ACL during `fsStepImportAdminDid` — was
planned and dropped along with the stored copy it fed; §7.1 and §10.5 record why,
because that reasoning is not recoverable from the code that is left.

`runVtaAclJob` additionally refuses any session whose `domain_type` is not
`platform`, so the scope in §1 holds even if a per-session route is wired to it
by mistake. That check is not the reason the scope is narrow — §7.4 is — and
removing it is not how the scope gets widened.

## 10. Deliberately out of scope

**10.1 Other sessions.** The mechanism is session-generic and the table is keyed
by `session_id`, but the routes are platform-stack only. Customers' stacks need
the second-approver flow of §7.4 first — the §7.4 argument for accepting the
escalation is specifically about the farm's own stack.

**10.2 Anything but unrestricted admin.** Context-scoped grants are useful and
the VTA supports them, but they need a real authorization model in the UI
(which contexts may this admin confer?) rather than a free-text field. Out of
scope until there is a second stated use case.

**10.3 Zero downtime.** Requires the farm to hold its own admin entry in the
VTA's ACL and to call `POST /acl` over REST. Technically clear — the VTA accepts
a DI-signed `auth/authenticate/0.1` Trust Task over plain REST, no DIDComm or
mediator involved (`vta-service/src/routes/auth.rs::try_authenticate_trust_task`).
The cost is a byte-exact `eddsa-jcs-2022` signer in Go (the VTI's own docs warn
that a mistake here "yields a signature that verifies nowhere"), or an image
carrying a `config-session`-built `pnm`, plus a farm-held super admin on every
stack. Revisit when this generalises past one session.

**10.4 Removing an admin.** `pnm acl delete <did>` against the running VTA does
it with no downtime and no code here. Building it into the API would mean
answering "which of these entries is the person I want to remove" — and after a
rotation the only handle is a label somebody typed (§7.2). An operator at a `pnm`
prompt has the full ACL in front of them and can decide; a form cannot. The
schema keeps no `revoked` status, so this is an addition rather than a
resurrection if it is ever wanted.

**10.5 Keeping any copy of the VTA's ACL.** Dropped along with the snapshot it
was built for — §7.1 has the reasoning. A corollary: the once-planned fourth
phase, capturing the ACL during `fsStepImportAdminDid`, is dropped too. It would
have put a parse of `vta acl list` in the provisioning critical path, where a
changed output format or a renamed flag fails the Job, fails the step and fails
the whole stack build — for a display nobody needs.
