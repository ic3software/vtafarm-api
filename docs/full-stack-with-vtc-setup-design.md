# Full Stack + VTC Setup — Design

Design for a fourth setup mode, **`full_stack_with_vtc`**: everything
`full_stack` already provisions (VTA + DIDComm Mediator + WebVH DID Hosting
daemon — see [`full-stack-setup-design.md`](full-stack-setup-design.md), the
base this doc only describes the *delta* on top of), plus a fourth
component, a **Verifiable Trust Community (VTC)** service (`vtc-service`),
automatically continuing in the same pipeline once the first three are up.

```text
https://fpp-xxxx.{domain}        ← VTA REST + DIDComm
https://mediator-xxxx.{domain}   ← DIDComm Mediator
https://dids-xxxx.{domain}       ← WebVH DID Hosting daemon
https://vtc-xxxx.{domain}        ← VTC REST + admin SPA + public website  (new)
```

> **Status: design only.** No code in this repo implements
> `full_stack_with_vtc` yet. §4c names one small **upstream** addition needed
> in `vtc-service` before this can be built; §17 records what was verified
> against the actual `vtc-service` / `vta-service` / `did-hosting-daemon`
> sources.

**There is no standalone `vtc_only` mode.** An earlier draft of this design
explored deploying a bare VTC pointed at an arbitrary/external VTA. It's
gone: a VTC that isn't wired to a mediator and a DID-hosting daemon isn't a
scenario this farm needs to support, and reusing the *existing session's own*
mediator/dids (rather than requiring the user to have external ones) is both
simpler to provision and the only combination that actually makes sense
operationally. Read [§7](#7-why-this-is-simpler-than-it-sounds) before
assuming this needs the same ACL-handshake ceremony a bare "attach VTC to
someone else's VTA" design would need — it doesn't.

---

## 1. Why VTC needs a mediator and DID-hosting

A VTC is a community layer on top of a VTA — it never mints keys itself, it
receives a sealed key bundle from the VTA at setup
(`docs/03-vtc/getting-started.md`, `vtc-service/README.md` in the
`verifiable-trust-infrastructure` repo). Deployed against a mediator-less,
DID-hosting-less VTA it would technically start, but:

- **No mediator → no DIDComm.** The VTC's join-request/credential-issuance
  surfaces are REST-first, but a VTC with no `[messaging]` configured can't
  participate in any DIDComm-based flows (join requests submitted over
  DIDComm, cross-community recognition, etc.) — it just logs a one-line
  "messaging not configured" warning at startup and stays REST-only.
- **No DID-hosting integration → the VTC has to self-host its own
  `did:webvh` log**, duplicating hosting capability the stack already has
  running two components away.

`full_stack` already stands up exactly the mediator + dids daemon a VTC
needs. Wiring the VTC to *those same instances* — rather than requiring the
user to separately operate (or vtafarm-api to separately provision) a second
mediator and a second dids daemon just for the VTC — is the only sensible
shape for this mode.

## 2. Component topology

```text
namespace: vtafarm-user-{userID}
┌──────────────────────────────────────────────────────────────────────────────────────┐
│   fs-{sid}-vta   fs-{sid}-mediator   fs-{sid}-dids   fs-{sid}-vtc     (new)           │
│   :8100          :7037               :8534           :8200                            │
│      │                │                  │               │                            │
│      └──── vta_did ───┘                  │               │                            │
│      └──── did-mgmt servers add ──────────┴───────────────┘   (§4a — VTC's did:webvh   │
│                                                                  is hosted by dids)     │
│      └──── [messaging].mediator_did ───────────────────────────► (VTC routes DIDComm    │
│                                                                  through the mediator)  │
└──────────────────────────────────────────────────────────────────────────────────────┘
```

`fs-{sid}-vtc` follows the exact naming convention the other three
components already use (`internal/k8s/fullstack_names.go` — `FSVtaName`,
`FSMediatorName`, `FSDidsName`); add `FSVtcName(sessionID uint) string {
return fmt.Sprintf("fs-%d-vtc", sessionID) }` alongside them. One more PVC +
Deployment + Service + Ingress, built entirely on the generic
`ComponentJobSpec`/`ComponentDeploymentSpec` helpers `full_stack` already
uses — no new K8s primitives needed, same as the other three.

## 3. URLs & DNS

Extend `FullStackHosts` (`internal/setup/subdomain.go`) to derive a fourth
host sharing the same random ID:

```go
func FullStackWithVtcHosts(env string) (vtaSub, mediatorSub, didsSub, vtcSub string)
```

Four Cloudflare A records, all created up front (same reasoning as
`full_stack`'s three — the rendered recipes embed the final HTTPS URLs):

```text
A   fpp-xxxx.{domain}        →  {CLUSTER_INGRESS_IP}
A   mediator-xxxx.{domain}   →  {CLUSTER_INGRESS_IP}
A   dids-xxxx.{domain}       →  {CLUSTER_INGRESS_IP}
A   vtc-xxxx.{domain}        →  {CLUSTER_INGRESS_IP}     (new)
```

## 4. The three mechanisms this leans on

### 4a. Registering the session's own dids daemon with the VTA — already-implemented, reused as-is

`vtc-service`'s non-interactive setup TOML can route the VTC's `did:webvh`
through a **registered** hosting server (`[webvh] server_id = "..."`)
instead of self-hosting at `base_url`
(`vtc-setup.example.toml`). Making that server the session's own
`fs-{sid}-dids` daemon requires the VTA to know that server exists — and
**`vtafarm-api` already does exactly this today**, for `vta_only`'s optional
external "DID Hosting Control" server:

```go
// internal/k8s/setup_jobs.go — CreateProvisionJob, already implemented
cmd := fmt.Sprintf("vta import-did --did %s --role admin", adminDid)
if controlDid != "" {
    cmd += fmt.Sprintf(" && vta did-mgmt servers add --id control --did %s --label 'DID Hosting Control Plane'", controlDid)
}
```

`full_stack_with_vtc` reuses the identical command shape — `vta did-mgmt
servers add --id <id> --did <did> --label <label>` — pointed at the
session's *own* dids daemon instead of an external shared one:

```sh
vta did-mgmt servers add --id dids --did {{ .DIDHostingDid }} --label "full_stack_with_vtc dids daemon"
```

`{{ .DIDHostingDid }}` is `full_stack`'s own `3d` output (already parsed by
`step_dids_p2` via `ParseServerDid` — see
[`full-stack-setup-design.md` §8](full-stack-setup-design.md#8-output-parsing-regex)),
already sitting on `SetupSession.DIDHostingDid`. No `--url` flag — same as
the existing `controlDid` usage, the VTA resolves the server's live
endpoints from its DID document, consistent with how every other
DID-to-endpoint resolution in this system works.

> **Placement constraint (verified in `vta-service` source):** `servers add`
> **resolves the server DID at registration time** —
> `operations/did_webvh/servers.rs::add_webvh_server` calls
> `validate_server_did`, which does a live `did_resolver.resolve(server_did)`
> and requires the resolved document to advertise a supported hosting
> service (`DIDCommMessaging` / `WebVHHosting`). The dids daemon serves its
> own `did.jsonl`, so its DID (`3d`) only resolves **once the daemon is
> running**. This step must therefore run **after `deploy_dids`** — and,
> being an offline fjall-store write, also **before `deploy_vta`**. Both
> constraints are satisfied by placing it in the post-gate finish phase
> (§6). Re-runs return `Conflict: webvh server already exists` — resume
> logic treats that as success.

This piece needs **zero upstream changes** — it's a new Job in
vtafarm-api's own orchestrator, built entirely from a command shape that
already ships and runs today.

### 4b. Authorizing the VTA on the dids daemon — one new offline Job

Hosted-mode publication is a **live, authenticated** call: at
`step_vtc_setup` time the running VTA authenticates to the dids daemon *as
its own `vta_did`* (challenge-response; `operations/did_webvh/mod.rs::
authenticated_server_transport` → `load_vta_webvh_signing_identity`) and
pushes the VTC's `did.jsonl` (`transport.publish_did`). The daemon's ACL is
**deny-by-default** — `did-hosting-common/src/server/acl.rs::check_acl`
returns `Forbidden: DID not in ACL` for any DID without an entry — and
`full_stack`'s daemon setup writes exactly **one** ACL entry (the recipe
admin `3b`, `"Setup recipe admin"`). The VTA's DID has none, so the publish
would 403.

This mirrors what `vta_only` already does for its external shared host —
`orchestrator.go` calls `didHosting.CreateAcl(session.VtaDid, "service", …)`
before registering the server ("The ACL must exist before the VTA pod
starts pushing DID updates"). `full_stack` itself never needed such a
grant: nothing in its pipeline authenticates to its daemon — the DID logs
reach it via offline `load-did` Jobs, and runtime resolution (`GET
…/did.jsonl`) is public by design. This mode is the first to make a
*runtime authenticated* call against the in-session daemon, which is why
the grant appears only now. For the in-session daemon the grant is done
**offline** instead: the daemon binary ships an `add-acl` subcommand
(`did-hosting-daemon add-acl --did <did> --role <admin|owner|service>
[--label …]`) that writes the fjall ACL keyspace directly — the same
stopped-daemon CLI family as `load-did` and `invite`, so it slots in right
next to them, **before `deploy_dids`**:

```sh
did-hosting-daemon add-acl --did {{ .VtaDid }} --role service --label "session VTA"
```

Role `service` matches the `vta_only` precedent exactly. The VTA DID (`1a`)
is known from `step_vta_setup`, well before this step runs. New step:
`step_dids_grant_vta` (§6/§8).

### 4c. The VTC's own ephemeral setup-key handshake — still needs one upstream addition

Provisioning a VTC authenticates to the VTA with an ephemeral `did:key`
(`vta_sdk::provision_client::EphemeralSetupKey`) that must already hold an
admin ACL entry at the VTA *before* `vtc setup --from <toml>` runs — see
`vtc-service`'s module docs (`vtc-service/src/setup/from_toml.rs`) and
`docs/03-vtc/getting-started.md` §"Non-interactive setup" (both in the
`verifiable-trust-infrastructure` repo). The interactive wizard generates
this key inline and pauses for the operator to grant it; a K8s Job can't
pause for a TTY prompt.

`vtc-service`'s CLI (`vtc-service/src/main.rs`) has no command that just
generates + persists the key and exits — only the interactive wizard calls
`EphemeralSetupKey::generate()` + `persist_to()` outside of tests, and
`vtc setup --from` requires the key to **already** exist at
`setup_key_file`.

**Proposed addition** (mirrors the existing shape of
`vta bootstrap provision-request` — local-only, file-based, non-interactive):

```text
vtc setup generate-key --out <path>
```

Generates a fresh key, persists it (the same `EphemeralSetupKey::persist_to`
format `vtc setup --from` already loads via `setup_key_file`), prints:

```text
setup_key_did=did:key:z6Mk...
```

This is the **one** piece of this whole design that requires a change
outside vtafarm-api. (`vtc create-did-key` is *not* a substitute — it writes
an ACL entry into the VTC's own store and prints a credential; it does not
persist the `setup_key_file` JSON. Fallback if the upstream change stalls:
the persisted format is a trivial 4-field JSON —
`{version: 1, did, private_key_multibase, note}` — so vtafarm-api *could*
mint the Ed25519 `did:key` in Go and write the file into the VTC PVC
itself, at the cost of duplicating the did:key/multibase encoding logic.)

## 5. Why this needs no ACL-grant gate and no service downtime

Every new step in §4 runs as an **offline Job against a store whose daemon
isn't running at that moment** — the same trick `full_stack` already relies
on everywhere (`step_mediator_reprov`, `step_dids_load_did`,
`step_import_admin_did`). Because `full_stack_with_vtc` **always builds the
VTA fresh in the same run** (it never attaches to an already-running
session, unlike an earlier "attach VTC to an existing farm session" idea
this design explicitly dropped — see the note at the top):

- No `awaiting_*_grant` pause — nothing needs a human in the loop, since
  vtafarm-api both generates the ephemeral key *and* grants its ACL itself.
- No scaling any Deployment to 0 first — each offline step lands in a
  window where its target store has never been claimed yet.

There are **two such offline windows**, and the new steps split across
them:

1. **The dids-store window** — after the dids setup Jobs, before
   `deploy_dids`. `step_dids_grant_vta` (§4b) joins `step_dids_invite` /
   `step_dids_load_did` here.
2. **The VTA-store window** — after the `awaiting_admin_did` gate, before
   `deploy_vta` (the same window `step_import_admin_did` already uses).
   `step_vtc_register_dids` (§4a), `step_vtc_setup_key`, and
   `step_vtc_acl_grant` (§4c) go **here, not earlier**. The hard reason is
   `step_vtc_register_dids`: `servers add` resolves the daemon's DID live
   (§4a), so it must run after `deploy_dids` — which is only true
   post-gate. The other two just travel with it: all three VTC pre-steps
   stay one contiguous block right before the components that consume
   them, which as a side effect keeps the ephemeral key's
   `--admin-expires 1h` grant-to-use window at minutes no matter how long
   the gate takes — the TTL never needs thinking about.

The **only** step that needs live components is the final
`vtc setup --from` call itself (§6, `step_vtc_setup`) — a genuine network
round-trip against the VTA (plus mediator resolution via `[messaging]` and
the daemon-hosted publish via `[webvh]`), so it runs *after* `deploy_vta`,
when all three earlier components are already up.

## 6. State machine

Steps inherited unchanged from `full_stack` are marked *(unchanged)*; see
[`full-stack-setup-design.md` §5](full-stack-setup-design.md#5-state-machine)
for their full description. New steps are unmarked.

```text
pending
  → dns_provision           create 4 Cloudflare A-records (was 3)
  → env_provision           EnsureUserEnvironment + EnsureUserAccess — policy
                            widened to the vtc/* prefix too (§10)          (unchanged, widened)
  → k8s_provision           PVCs/Services/Ingresses for vta/mediator/dids/vtc (was 3)
  → step_vta_setup          → 1a VTA DID, 1b Mediator DID                  (unchanged)
  → step_mediator_p1                                                       (unchanged)
  → step_mediator_reprov    → 2a digest, 2b admin DID                      (unchanged)
  → step_mediator_p2        → 2c admin priv key                           (unchanged)
  → step_dids_p1                                                           (unchanged)
  → step_dids_provision     → 3a digest                                   (unchanged)
  → step_dids_p2            → 3b admin DID, 3c admin priv key, 3d daemon DID (unchanged)
  → step_dids_invite        → 3e dids admin-enroll URL                     (unchanged)
  → step_dids_load_did      loads VTA + mediator DID logs (not VTC's — it   (unchanged)
                            doesn't have one yet; see §5)
  → step_dids_grant_vta     `did-hosting-daemon add-acl --did {{1a}} --role service` (§4b)
  → deploy_dids                                                            (unchanged)
  → deploy_mediator                                                        (unchanged)
  → awaiting_admin_did      ⏸ gate: PNM admin DID (skip if supplied upfront) (unchanged)
  → step_import_admin_did                                                  (unchanged)
  → step_vtc_register_dids  `vta did-mgmt servers add --id dids --did {{3d}}` (§4a —
                            daemon now live + resolvable; VTA store still unclaimed)
  → step_vtc_setup_key      `vtc setup generate-key` → setup_key_did        (§4c)
  → step_vtc_acl_grant      `vta contexts create --id {{vtc_name}} --name "VTC"
                             --admin-did {{setup_key_did}} --admin-expires 1h` (§4c)
  → deploy_vta                                                             (unchanged)
  → step_vtc_setup          LIVE `vtc setup --from vtc-setup.toml` (VTA +
                            mediator + dids all reachable now) → vtc_did,
                            admin_did, install_url, claim_code
  → deploy_vtc              Deployment fs-{sid}-vtc (start it)
  → running                 return 4 URLs + everything full_stack returns +
                            vtc_did + install_url + claim_code (once)
        ↓ (any step)
     failed
```

Every `step_*` above follows the same `WaitForJob` + `JobLogs` + parse cycle
as the rest of `full_stack` (`orchestrator_fullstack.go`'s existing
pattern) — `runFullStackWithVtc` calls the *same* `o.fsStepVtaSetup`,
`o.fsStepMediatorP1`, …, `o.fsStepDidsP2`, `o.fsStepDidsInvite`,
`o.fsStepDidsLoadDid`, `o.fsDeployDids`, `o.fsDeployMediator` methods
`full_stack` already has, adding one step (`step_dids_grant_vta`) to the
pre-gate dids window and four (`step_vtc_register_dids` /
`step_vtc_setup_key` / `step_vtc_acl_grant` before `deploy_vta`, then
`step_vtc_setup` + `deploy_vtc` after it) to the post-gate finish phase.
No existing `full_stack` method changes. Extend the `Resume` queries with
the new pre-gate status (`step_dids_grant_vta`) and post-gate statuses
(`step_vtc_register_dids` … `deploy_vtc`) — re-running a phase from its top
re-creates Jobs idempotently (AlreadyExists ignored, `WaitForJob`
re-attaches by name); the two grant commands additionally need their
Conflict cases tolerated (§8).

## 7. Why this is simpler than it sounds

Compare to the abandoned "attach a VTC to an arbitrary/already-running VTA"
shape: that design needed a manual `awaiting_grant` gate (or, for a
farm-hosted target, a scale-to-0/Job/scale-back-to-1 dance to work around
the target VTA's fjall lock — the same constraint
`full_stack`'s own `ReissueDidsEnroll` already works around for the dids
daemon). `full_stack_with_vtc` needs **neither**, because it never attaches
to something already running — every dependency the VTC needs (a VTA, a
context with its ACL granted, a registered dids server the VTA is
authorized to publish to, a reachable mediator) is minted in the *same*
pipeline run, in the right order, before the VTC ever asks for any of it.
The only genuinely-live, network-based step in the whole mode is
`step_vtc_setup` itself, and by construction everything it depends on is
already up by the time it runs.

## 8. Per-step Jobs (new steps only)

All Jobs use `internal/k8s/component_jobs.go`'s `ComponentJobSpec` /
`CreateComponentJob` — no new K8s plumbing, same generic helper
`full_stack` already uses for its other components.

### `step_dids_grant_vta` — DIDS Job (workingDir `/work/dids`, before `deploy_dids`)

```go
k8s.ComponentJobSpec{
    Name:           FSJobDidsGrantVta(s.ID),
    Image:          s.DidsImage,
    Command:        []string{"sh", "-c", fmt.Sprintf(
        "did-hosting-daemon add-acl --did %s --role service --label %s",
        shellQuote(s.VtaDid), shellQuote("session VTA"),
    )},
    WorkingDir:     "/work/dids",
    ServiceAccount: k8s.PodOperatorServiceAccount,
    PVCMounts:      []k8s.PVCMount{{Name: "dids-data", ClaimName: k8s.FSDidsName(s.ID), MountPath: "/work/dids"}},
    Env:            fsNoColorEnv(),
}
```

Authorizes the session VTA's DID (`1a`) to publish DID logs to the daemon
(§4b). Offline store write — must run before `deploy_dids`, same
constraint (and same placement window) as `step_dids_invite` /
`step_dids_load_did`. Nothing to parse. On re-run the daemon CLI errors
`ACL entry already exists` — resume treats that as success (check the
error text, or `list-acl` first).

### `step_vtc_register_dids` — VTA Job (workingDir `/work/vta`, post-gate, before `deploy_vta`)

```go
k8s.ComponentJobSpec{
    Name:           FSJobVtcRegisterDids(s.ID),
    Image:          s.VtaImage,
    Command:        []string{"sh", "-c", fmt.Sprintf(
        "vta did-mgmt servers add --id dids --did %s --label %s",
        shellQuote(s.DIDHostingDid), shellQuote("full_stack_with_vtc dids daemon"),
    )},
    WorkingDir:     "/work/vta",
    ServiceAccount: k8s.VtaServiceAccount,
    PVCMounts:      []k8s.PVCMount{{Name: "vta-data", ClaimName: k8s.FSVtaName(s.ID), MountPath: "/work/vta"}},
    Env:            fsNoColorEnv(),
}
```

Runs in the post-gate finish phase: after `deploy_dids` (the daemon must be
live — `servers add` resolves `3d` and checks for a `WebVHHosting`-family
service in its DID document, §4a) and before `deploy_vta` (offline
fjall-store write). Nothing to parse — success is exit code 0; on re-run,
`Conflict: webvh server already exists` counts as success.

### `step_vtc_setup_key` — VTC Job (workingDir `/work/vtc`)

```go
k8s.ComponentJobSpec{
    Name:           FSJobVtcSetupKey(s.ID),
    Image:          s.VtcImage,
    Command:        []string{"sh", "-c", "vtc setup generate-key --out /work/vtc/setup-key.json"},
    WorkingDir:     "/work/vtc",
    ServiceAccount: k8s.PodOperatorServiceAccount, // no Vault access needed for this step
    PVCMounts:      []k8s.PVCMount{{Name: "vtc-data", ClaimName: k8s.FSVtcName(s.ID), MountPath: "/work/vtc"}},
    Env:            fsNoColorEnv(),
}
```

Parse `setup_key_did=(did:\S+)` from stdout; persist to
`SetupSession.VtcSetupKeyDid` (mostly for debuggability/audit — nothing
downstream reads it back from the DB, `step_vtc_acl_grant` gets it straight
from this Job's own logs in the same orchestrator run).

### `step_vtc_acl_grant` — VTA Job (workingDir `/work/vta`, post-gate, before `deploy_vta`)

```go
k8s.ComponentJobSpec{
    Name:           FSJobVtcAclGrant(s.ID),
    Image:          s.VtaImage,
    Command:        []string{"sh", "-c", fmt.Sprintf(
        `vta contexts create --id %s --name "VTC" --admin-did %s --admin-expires 1h`,
        shellQuote(s.VtcName), shellQuote(setupKeyDid),
    )},
    WorkingDir:     "/work/vta",
    ServiceAccount: k8s.VtaServiceAccount,
    PVCMounts:      []k8s.PVCMount{{Name: "vta-data", ClaimName: k8s.FSVtaName(s.ID), MountPath: "/work/vta"}},
    Env:            fsNoColorEnv(),
}
```

`s.VtcName` (new request field, default `"personal-vtc"`, same convention as
`vta_name`) doubles as the VTA context id the VTC's community lives under —
using it rather than a fixed `"default"` avoids collisions if the VTA ever
ends up hosting more than one context. Nothing to parse.

`vta contexts create` is offline (fjall) with exactly these flags —
`--id`, `--name`, `--admin-did` (atomically writes an admin ACL entry
scoped to the new context), `--admin-expires N[s|m|h|d|w]` (swept by the
running VTA's ACL sweeper). Because this runs post-gate, minutes before
`step_vtc_setup` consumes it, `1h` is comfortable (§5). **Resume:** on
re-run it errors `Conflict: context already exists`; since resume re-mints
a *fresh* setup key first, the fallback grant for an existing context is
`vta import-did --did <setup_key_did> --role admin --context {{vtc_name}}
--label vtc-setup` (offline, upserts, context-scoped — the same command
family the PNM import already uses; no expiry flag, acceptable for the
retry path).

### `step_vtc_setup` — VTC Job (workingDir `/work/vtc`)

```go
k8s.ComponentJobSpec{
    Name:           FSJobVtcSetup(s.ID),
    Image:          s.VtcImage,
    Command:        []string{"sh", "-c", "vtc setup --from /config/vtc-setup.toml"},
    WorkingDir:     "/work/vtc",
    ServiceAccount: k8s.VtaServiceAccount, // needs Vault (kubernetes auth) — §9
    PVCMounts:      []k8s.PVCMount{{Name: "vtc-data", ClaimName: k8s.FSVtcName(s.ID), MountPath: "/work/vtc"}},
    ConfigMapName:  FSJobVtcSetup(s.ID),
    ConfigMapKey:   "vtc-setup.toml",
    ConfigMapData:  toml, // §9
    Env:            fsNoColorEnv(),
}
```

`setup-key.json` (written by `step_vtc_setup_key`, on the same PVC, already
ACL-granted by `step_vtc_acl_grant`) is loaded via the TOML's
`setup_key_file = "setup-key.json"` (relative path, resolved by
`workingDir`). Parse the terse completion block
(`vtc-service/src/setup/from_toml.rs::print_setup_summary_terse`):

```text
VTC setup complete.
vtc_did=did:webvh:...
admin_did=did:key:...
config_path=config.toml
data_dir=./vtc-data
install_url=https://vtc-xxxx.example.com/admin/install?token=...
claim_code=ABCD-1234
```

One parser, `ParseVtcSetupOutput(logs string) (VtcSetupOutcome, error)`,
extracts all five `key=value` lines in one pass (`(?m)^vtc_did=(.+)$` etc.)
— unlike `full_stack`'s other parsed values, these aren't embedded in prose,
so there's nothing per-field to disambiguate. `admin_did` here is the VTC's
own pre-claim install admin, **not** the PNM `admin_did` column
`full_stack` already uses for `step_import_admin_did` — give it its own
column (§11), don't overload the existing one.

> **`install_url` expires fast.** The setup-minted install token uses the
> VTC's default TTL — `INSTALL_TOKEN_DEFAULT_TTL_SECS` = **15 minutes**
> (`vtc-service/src/install/token.rs`). Users polling `GET /setup/:id`
> after a long-running pipeline will usually find it already dead, which is
> why the reissue endpoint (§13) is a **required** part of this mode, not a
> nice-to-have — the frontend should offer "mint a fresh install link" as
> the normal path whenever the token's mint time is stale.

### `deploy_vtc`

```go
k8s.ComponentDeploymentSpec{
    Name:           k8s.FSVtcName(s.ID),
    Image:          s.VtcImage,
    Command:        nil, // image entrypoint
    WorkingDir:     "/work/vtc",
    ServiceAccount: k8s.VtaServiceAccount, // reads its Vault key bundle at every boot
    PVCMounts:      []k8s.PVCMount{{Name: "vtc-data", ClaimName: k8s.FSVtcName(s.ID), MountPath: "/work/vtc"}},
    Port:           8200,
    Labels:         fsLabels("vtc", s.ID),
}
```

Service + Ingress for `fs-{sid}-vtc` are created up front in `k8s_provision`
(§2), same pattern as the other three components.

## 9. TOML template — `vtc-setup.toml`

Renders the schema `vtc-service/src/setup/from_toml.rs::VtcWizardInputs`
deserializes. **Note the `[secrets]` shape differs from the VTA's own setup
TOML**: `vta setup --from` uses a tagged `backend = "vault"` field
(`docs/02-vta/non-interactive-setup.md`), but `vtc setup --from`'s
`[secrets]` is the VTC's own **implicit-selection** `SecretsConfig`
(`vtc-service/src/config.rs` — it deliberately mirrors
`vti_secrets::SecretsConfig`'s field names, and is the same type the
runtime `config.toml` uses) — setting `vault_addr` activates the Vault
backend, there is no `backend =` key. Every `vault_*` field below is
verified against that struct (defaults: `vault_kv_mount = "secret"`,
`vault_secret_key = "seed"`, `vault_auth_method = "kubernetes"`). Both
`VtcWizardInputs` and `SecretsConfig` are `#[serde(deny_unknown_fields)]`,
so a stray `backend = "vault"` copied from the VTA template would fail fast
as a parse error rather than silently misconfiguring anything.

```toml
config_path    = "config.toml"
base_url       = "{{ .VtcPublicURL }}"        # https://vtc-xxxx.{domain}
vta_did        = "{{ .VtaDid }}"              # 1a
context        = "{{ .VtcName }}"
setup_key_file = "setup-key.json"

[webvh]
server_id = "dids"                            # registered in step_vtc_register_dids (§4a/§8)

[messaging]
mediator_did = "{{ .MediatorDid }}"           # 1b — the same shared mediator
mediator_url = "{{ .MediatorURL }}"           # informational only; endpoint is resolved from the DID doc

[secrets]
vault_addr        = "{{ .Vault.Addr }}"
vault_kv_mount    = "{{ .Vault.KVMount }}"
vault_secret_path = "{{ .Vault.SecretPath }}"  # vtc/user-<id>/session-<id>/key-bundle
vault_secret_key  = "bundle"
vault_auth_method = "kubernetes"
vault_k8s_role    = "{{ .Vault.K8sRole }}"     # vta-user-<id> — reused, see §10
vault_skip_verify = {{ .Vault.SkipVerify }}
```

`[webvh].server_id = "dids"` matches the `--id dids` used in
`step_vtc_register_dids` (§4a/§8) — the VTA resolves it against its own
registry. `domain`/`path` are left unset: this dids daemon isn't
multi-tenant, and an unset `path` lets it auto-assign one, avoiding any
collision risk (§8).

## 10. Secrets handling — reuses the VTA's Vault mechanism, not the mediator's

VTC natively supports **kubernetes auth** for its Vault backend
(`vault_auth_method` defaults to `"kubernetes"` — same field names as the
VTA's, per the config's own doc comment: "Field names mirror the VTA's...
so the shared Vault builder is reused"). This is simpler than the
mediator's Vault integration, which needs **token** auth (a minted
`VAULT_TOKEN` injected via a per-session K8s Secret,
`fsMediatorVaultEnv`/`MintMediatorToken`/`FSMediatorTokenSecret`) because its
binary lacks kubernetes-auth support.

> **Image build requirement:** `vault-secrets` is **not** a default
> feature of `vtc-service` (`Cargo.toml`: `vault-secrets =
> ["vti-secrets/vault-secrets"]`, listed as non-default in
> `docs/03-vtc/feature-flags.md`). The published VTC image must be built
> with `--features vault-secrets`, exactly analogous to the mediator's
> `secrets-vault` build requirement in
> [`full-stack-setup-design.md` §9](full-stack-setup-design.md#9-secrets-handling).
> Without it, `create_secret_store` fails at setup with a
> feature-not-compiled error.

VTC needs none of the mediator's token machinery:

- **No new ServiceAccount.** Every VTC Job/Deployment that touches Vault
  runs as the same `vta` ServiceAccount the VTA already uses
  (`k8s.VtaServiceAccount`), in the same per-user namespace.
- **No new kubernetes-auth role.** The existing per-user role
  (`vault.UserName(userID)`, bound to SA `vta` + the user's namespace) is
  reused as-is — only its **policy** needs widening.
- **No new K8s Secret, no token minting/injection.** Contrast the
  mediator's `MintMediatorToken`/`CreateComponentSecret`/
  `TeardownMediatorVault` — none of that machinery is needed here.

```go
// VtcPrefix is the KV v2 path (under the mount) where a full_stack_with_vtc
// session's VTC key bundle lives. EnsureUserAccess (below) globs over
// secret/{data,metadata}/vtc/user-<id>/*, mirroring MediatorPrefix.
func VtcPrefix(userID, sessionID uint) string {
    return fmt.Sprintf("vtc/user-%d/session-%d", userID, sessionID)
}
```

Extend `EnsureUserAccess`'s policy string (`internal/vault/client.go`) with
two more lines, same capabilities as the existing VTA seed grant:

```text
path "%[1]s/data/vtc/user-%[2]d/*"     { capabilities = ["read", "create", "update", "delete"] }
path "%[1]s/metadata/vtc/user-%[2]d/*" { capabilities = ["read", "delete"] }
```

Add `DeleteVtcSecrets(ctx, userID, sessionID uint) error`, mirroring
`DeleteMediatorSecrets` (KV v2 metadata delete of the `vtc/user-*/session-*`
prefix) — called at teardown alongside `TeardownMediatorVault`, since
there's no token to revoke for the VTC.

`vault_secret_key = "bundle"` (§9) stores the VTC's serialized
`VtcKeyBundle` bytes under that field name — `vti_secrets`'s generic seed
store is byte-agnostic (the field is called "seed" by default because the
VTA uses it for a BIP-32 seed; VTC reuses the same abstraction for a
different payload shape).

## 11. Data model changes

Additive columns on `SetupSession` (new migration `000012_vtc_fields` —
next free number after `000011_add_beta_access_to_users`):

```go
const ModeFullStackWithVtc = "full_stack_with_vtc"

// vtc component — subdomain/CFRecordID follow the same pattern as
// MediatorSubdomain/CFRecordMediator and DidsSubdomain/CFRecordDids.
VtcSubdomain string  `gorm:"column:vtc_subdomain;not null;default:''"`
CFRecordVtc  *string `gorm:"column:cf_record_vtc"`

VtcName  string `gorm:"not null;default:'personal-vtc'"` // also the VTA context id (§8/§9)
VtcImage string `gorm:"not null;default:''"`              // required, like VtaImage/MediatorImage/DidsImage

// Collected outputs.
VtcSetupKeyDid string `gorm:"not null;default:''"` // ephemeral did:key from step_vtc_setup_key (§8) — audit/debug only
VtcDid         string `gorm:"not null;default:''"` // vtc_did — the VTC's own did:webvh
VtcAdminDid    string `gorm:"not null;default:''"` // admin_did from the setup summary — NOT the PNM AdminDid column (§8)
VtcInstallURL  string `gorm:"not null;default:''"` // install_url — reveal-once, like MediatorAdminKey/WebvhAdminKey
VtcClaimCode   string `gorm:"not null;default:''"` // claim_code — reveal-once, delivered over a logically separate channel

// VtcInstallUsed mirrors DidsEnrollUsed — set by the frontend once the user
// opens VtcInstallURL, so GET /setup/:id stops re-offering a dead link. The
// VTC's own install-token state machine already refuses a second claim;
// this just improves the UI.
VtcInstallUsed bool `gorm:"not null;default:false"`
```

```go
func (s *SetupSession) VtcFQDN() string { return s.VtcSubdomain + "." + s.Domain }
```

## 12. Config / env additions

| Var | Purpose |
| --- | --- |
| `GITHUB_VTC_PACKAGE_NAME` | GHCR package for `GET /setup/images?component=vtc` (default `vtc-service`, same convention as `GITHUB_MEDIATOR_PACKAGE_NAME`) |

Reuse everything else — `CLUSTER_INGRESS_IP`, `CLUSTER_DOMAIN`,
`CLOUDFLARE_*`, all `VAULT_*` (no new Vault token role needed, §10).

**No new ClusterRole permissions.** The `secrets` verb `full_stack` already
added (for the mediator's `VAULT_TOKEN` Secret) isn't needed here — `vtc`
has no equivalent. Everything else the existing rule set already grants
(namespaces, SAs, pods, pods/log, configmaps, PVCs, services, roles,
rolebindings, jobs, deployments, ingresses) covers the fourth component too.

## 13. API surface

`mode = "full_stack_with_vtc"` on `POST /api/v1/setup` selects this path.
Response shapes extend `full_stack`'s
([`full-stack-setup-design.md` §12](full-stack-setup-design.md#12-api-surface))
with a fourth URL and the VTC's collected outputs.

| Method | Path | Change from `full_stack` |
| --- | --- | --- |
| `POST` | `/setup/validate` | also assert the vtc host is creatable |
| `POST` | `/setup` | accept `mode=full_stack_with_vtc`; requires `vtc_image` (like `mediator_image`/`dids_image`); optional `vtc_name` (default `personal-vtc`); creates 4 DNS records |
| `GET` | `/setup/:id` | fourth URL (`urls.vtc`); `collected.vtc_did`; `action_required.install_url` + `action_required.claim_code` (reveal-once, alongside the existing mediator/webvh admin keys); `vtc_install_used` |
| `GET` | `/setup/:id/logs` | `?source=` gains `dids_grant_vta\|vtc_register_dids\|vtc_setup_key\|vtc_acl_grant\|vtc_setup\|vtc` |
| `POST` | `/setup/:id/vtc/install-ack` *(new)* | frontend marks `vtc_install_used = true` once the user opens `install_url` — mirrors `AckDidsEnroll` |
| `POST` | `/setup/:id/vtc/reissue-install` *(new, **required** — the setup-minted token lives only 15 min, §8)* | remints a fresh install URL **and claim code** via `vtc admin invite --did <vtc_admin_did>` — mirrors `ReissueDidsEnroll`: scale `fs-{sid}-vtc` to 0, wait pod gone, run the Job, scale back to 1 (via `defer`, so the VTC restarts even on failure). Parse both the `Install URL (one-shot):` and `Claim code …:` lines from the Job log (the CLI prints them on stderr; K8s captures both streams), update `vtc_install_url` + `vtc_claim_code`, reset `vtc_install_used` |
| `DELETE` | `/setup/:id` | tear down all 4 DNS records + all 4 components' resources |

`GET /setup/:id` (full_stack_with_vtc) response sketch — extends the
`full_stack` shape:

```jsonc
{
  "id": "ab12cd34",
  "mode": "full_stack_with_vtc",
  "status": "running",
  "urls": {
    "vta":      "https://fpp-a1b2c3d4.example.com",
    "mediator": "https://mediator-a1b2c3d4.example.com",
    "dids":     "https://dids-a1b2c3d4.example.com",
    "vtc":      "https://vtc-a1b2c3d4.example.com"
  },
  "collected": {
    "vta_did":               "did:webvh:…:dids-a1b2c3d4.example.com:vta",
    "mediator_did":          "did:webvh:…:dids-a1b2c3d4.example.com:mediator",
    "did_hosting_did":       "did:webvh:…:dids-a1b2c3d4.example.com",
    "mediator_admin_did":    "did:key:z6Mk…",
    "did_hosting_admin_did": "did:key:z6Mk…",
    "vtc_did":               "did:webvh:…:dids-a1b2c3d4.example.com:<vtc-path>"
  },
  "action_required": {
    "dids_admin_enroll_url": "https://dids-a1b2c3d4.example.com/enroll/…",
    "install_url":           "https://vtc-a1b2c3d4.example.com/admin/install?token=…",
    "claim_code":            "ABCD-1234",
    "reveal_keys_once":      true
  },
  "vtc_install_used": false
}
```

**Every new route must be added to `internal/apidocs/openapi.yaml`** with
the `User` tag (API Docs Rule, `CLAUDE.md`).

## 14. Teardown

`DELETE /setup/:id` for `full_stack_with_vtc` extends
`full_stack`'s teardown ([`full-stack-setup-design.md` §13](full-stack-setup-design.md#13-teardown))
with:

- Delete the 4th Cloudflare record (`CFRecordVtc`).
- `DeleteComponentResources(ctx, ns, k8s.FSVtcName(sid))` alongside the
  other three; `DeleteAllComponentJobs` already covers the new Job names
  once they're added to the session's job-name list.
- `vault.DeleteVtcSecrets(ctx, userID, sid)` alongside
  `TeardownMediatorVault` — no token to revoke for the VTC.

No de-registration of the `dids` server entry from the VTA's registry is
needed — the VTA itself is being deleted as part of the same teardown (its
namespace goes away with everything else), so there's nothing left to hold
a stale registration.

## 15. Manual gates that remain

Exactly one — **the WebAuthn admin-claim ceremony.** `install_url` +
`claim_code` (delivered over conceptually separate channels — the VTC
itself refuses a claim without both, per `getting-started.md` §"Step 3")
are surfaced once the session reaches `running`. It doesn't block
deployment and is single-use — and since the setup-minted token expires
after 15 minutes (§8), the reissue endpoint (§13) is the *expected* way
users actually claim, not an edge case. Same category as `full_stack`'s
existing dids admin-enrollment gate, not a new kind of manual step.

There is **no** ACL-grant gate — see §5/§7.

## 16. Implementation checklist

1. **Upstream (`verifiable-trust-infrastructure`):** add
   `vtc setup generate-key --out <path>` to `vtc-service` (§4c). Nothing
   below can be built end-to-end without this (Go-side fallback noted in
   §4c if it stalls).
2. **Image pipeline:** publish the `vtc-service` image built with
   `--features vault-secrets` (non-default — §10) under
   `GITHUB_VTC_PACKAGE_NAME`.
3. Migration `000012_vtc_fields` — additive columns (§11).
4. `model.SetupSession` — new fields, `ModeFullStackWithVtc` constant,
   `VtcFQDN()`.
5. `internal/k8s/fullstack_names.go` — add `FSVtcName`,
   `FSJobDidsGrantVta`, `FSJobVtcRegisterDids`, `FSJobVtcSetupKey`,
   `FSJobVtcAclGrant`, `FSJobVtcSetup` alongside the existing FS* helpers
   (and to `allFSJobNames` for teardown).
6. `internal/setup/subdomain.go` — `FullStackWithVtcHosts` (§3).
7. `internal/setup/templates_fullstack.go` (or a new
   `templates_fullstack_vtc.go`) — `RenderVtcSetupTOML` (§9).
8. `internal/setup/parser_fullstack.go` (or a new
   `parser_fullstack_vtc.go`) — `setup_key_did` regex, the 5-field terse
   completion-block parser (§8), and the reissue parser for `vtc admin
   invite`'s `Install URL` / `Claim code` lines (§13).
9. `internal/vault/client.go` — `VtcPrefix`, `DeleteVtcSecrets`, widen
   `EnsureUserAccess`'s policy string (§10).
10. `internal/setup/orchestrator_fullstack_vtc.go` — `runFullStackWithVtc`:
    calls `full_stack`'s existing step methods unchanged through
    `fsStepDidsLoadDid`, adds `step_dids_grant_vta` before `fsDeployDids`
    (§6/§8), then `fsDeployDids`/`fsDeployMediator` unchanged; the finish
    phase (after the `admin_did` gate) runs `step_import_admin_did` →
    `step_vtc_register_dids` → `step_vtc_setup_key` → `step_vtc_acl_grant`
    → `deploy_vta` → `step_vtc_setup` → `deploy_vtc`. Extend resume for
    the new pre-gate + post-gate statuses; tolerate the two grant
    Conflicts on re-run (§8).
11. `internal/handler/setup_fullstack_vtc.go` — branch on `mode`; 4th DNS
    record; require `vtc_image`; extend the `full_stack` response shape
    with `urls.vtc`/`collected.vtc_did`/reveal-once install fields;
    install-ack + reissue endpoints; extended teardown (§14).
12. `internal/config/config.go` + `.env.example` — `GITHUB_VTC_PACKAGE_NAME`.
13. `internal/apidocs/openapi.yaml` — document new/changed routes
    (`User` tag).

---

## 17. Verified against source

Claims in this design that were checked directly against the upstream
codebases (`verifiable-trust-infrastructure` for vtc/vta,
`webvh-build-pipeline` for the dids daemon), rather than taken from docs:

| Claim | Evidence |
| --- | --- |
| `vtc setup --from <toml>` exists and is fully non-interactive | `vtc-service/src/setup/from_toml.rs` — parses `VtcWizardInputs`, feeds the same `apply()` as the wizard; terse `key=value` summary incl. `install_url`/`claim_code` |
| No standalone setup-key generator exists (upstream ask, §4c) | `vtc-service/src/main.rs` — only `Setup{--from}`, `Status`, `CreateDidKey`, `Admin`, `Acl`; `CreateDidKey` writes the VTC store/credential, not the `setup_key_file` JSON |
| Setup-key JSON shape (Go fallback option) | `vta-sdk/src/provision_client/setup_key.rs::PersistedKey` — `{version: 1, did, private_key_multibase, note}`, 0600 |
| `[secrets]` field names + defaults + `deny_unknown_fields` | `vtc-service/src/config.rs::SecretsConfig` (mirrors `vti_secrets`; kv_mount `secret`, secret_key `seed`, auth `kubernetes`) |
| `vault-secrets` is a non-default vtc feature | `vtc-service/Cargo.toml` + `docs/03-vtc/feature-flags.md` |
| `[messaging]` fields (`mediator_did` required, `mediator_url` optional) | `vti-common/src/config.rs::MessagingConfig` |
| `[webvh].server_id` → `WEBVH_SERVER` template var | `vtc-service/src/setup/wizard.rs` (`WebvhTarget`, `build_template_vars`) |
| `vta contexts create` offline, `--admin-did`/`--admin-expires N[s\|m\|h\|d\|w]`, atomic ACL; Conflict on existing id | `vta-service/src/main.rs::ContextCommands::Create`; `operations/contexts.rs` (`Conflict: context already exists`) |
| `vta import-did --role admin --context <ctx>` as the exists-tolerant regrant | `vta-service/src/main.rs::ImportDid` (`--context Vec<String>`) |
| `servers add` resolves the DID at add time (placement constraint, §4a) | `vta-service/src/operations/did_webvh/servers.rs::add_webvh_server` → `validate_server_did` (live resolve + `WebVHHosting`/`DIDCommMessaging` service required; Conflict on duplicate id) |
| Hosted publish = live authed call as `vta_did` | `operations/did_webvh/mod.rs::authenticated_server_transport` + `transport.publish_did` in `create_did_webvh`; provision-integration selects it via `WEBVH_SERVER` (`operations/provision_integration/{mod,webvh}.rs`) |
| Daemon ACL is deny-by-default; setup seeds only the recipe admin (§4b) | `did-hosting-common/src/server/acl.rs::check_acl` (`Forbidden: DID not in ACL`); `did-hosting-daemon/src/setup_recipe.rs` (single `Role::Admin` entry) |
| Daemon offline `add-acl` CLI, roles admin/owner/service | `did-hosting-daemon/src/main.rs::Command::AddAcl` → `did-hosting-common/src/server/cli_acl.rs` |
| Hosted register resolves a domain; first boot seeds `public_url` host as default domain | `did-hosting-control/src/routes/did_manage.rs::register_did` → `resolve_request_domain`; `did-hosting-common/src/server/domain/seed.rs` (tier-2 legacy `public_url` seed, sets default) — so no explicit `domain` needed in §9's `[webvh]` |
| Install token TTL 15 min; `vtc admin invite` remints URL + claim code on a stopped daemon | `vtc-service/src/install/token.rs::INSTALL_TOKEN_DEFAULT_TTL_SECS`; `vtc-service/src/main.rs::run_invite_cli` |

One assumption **not** yet verified end-to-end: the daemon's own DID
document (created by its recipe setup) advertising a `WebVHHosting`-family
service that `validate_server_did` accepts, and the full
authenticate-and-publish round-trip against it. Both ends are this
workspace's own code and the `vta_only` external-host flow exercises the
same wire path today, but `full_stack_with_vtc`'s first integration test
should cover `step_vtc_register_dids` + `step_vtc_setup` explicitly.
