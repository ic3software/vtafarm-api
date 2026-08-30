# Full-Stack VTI Setup — Design

Design for the **Full Stack** setup mode (`mode = "full_stack"`): vtafarm-api
provisions a complete, self-contained VTI stack — **VTA + DIDComm Mediator + WebVH DID
Hosting daemon + Verifiable Trust Community (VTC)** — inside one user namespace, wired
together, and returns four live URLs:

```text
https://vta-{vta_name}.{domain}        ← VTA REST + DIDComm
https://mediator-{vta_name}.{domain}   ← DIDComm Mediator
https://dids-{vta_name}.{domain}       ← WebVH DID Hosting daemon (admin panel + DID resolution)
https://vtc-{vtc_name}.{domain}        ← VTC REST + admin SPA + public website
```

**The VTC is not optional.** Every `full_stack` session provisions all four components
in one pipeline run. There is no VTC-less variant and no separate
`full_stack_with_vtc` mode — an earlier iteration of this farm shipped both
(`full_stack` = three components, `full_stack_with_vtc` = four), and that split is
**retired**: the three-component pipeline is gone, and the four-component one is what
`full_stack` now means. The only other mode is `vta_only`
([`vta-setup-design.md`](vta-setup-design.md), Mode A).

**There is likewise no standalone `vtc_only` mode.** An earlier draft explored
deploying a bare VTC pointed at an arbitrary/external VTA. It's gone: a VTC that isn't
wired to a mediator and a DID-hosting daemon isn't a scenario this farm needs to
support, and reusing the *same session's own* mediator/dids is both simpler to provision
and the only combination that makes sense operationally. See
[Appendix B](#appendix-b--vtc-design-rationale) before assuming the VTC needs the
ACL-handshake ceremony a bare "attach a VTC to someone else's VTA" design would need —
it doesn't.

This is the K8s/API automation of the manual single-host flow verified on Ubuntu
(reproduced in [Appendix A](#appendix-a--verified-ubuntu-reference-flow)). Every
`<binary> setup --from <recipe>` command in that flow becomes a one-off K8s **Job**;
every cross-component file handoff (the `~/vta`, `~/mediator`, `~/dids` home dirs on a
single host) is reproduced by mounting the relevant PVCs into the Job that needs them;
every `nohup <binary> &` becomes a **Deployment**.

**Secret backends.** **All four components — VTA, Mediator, DID Hosting daemon, and
VTC — store their secrets in the farm Vault, all via the same mechanism**: kubernetes
auth. Each pod presents its `vta` ServiceAccount JWT against the same per-user
kubernetes-auth role the VTA already uses; there's no per-session token to mint or
inject, and no plaintext fallback anywhere. See [§9](#9-secrets-handling).

The mediator's *message* storage is still **fjall** (file-backed on its PVC) — Vault
only holds the mediator's secrets, not its message store. No Redis/Valkey.

> Status: **implemented** (`internal/setup/orchestrator_fullstack.go` +
> `internal/setup/orchestrator_vtc.go` + the `internal/k8s/component_*.go`
> helpers). This document is the authoritative design reference; the §5 state machine
> matches the code. One **external** prerequisite is standing: the VTC image must be
> published built with `--features vault-secrets`
> ([§9](#9-secrets-handling)/[§15](#15-implementation-checklist) item 2).
> [Appendix C](#appendix-c--verified-against-source) records what was verified against
> the actual `vtc-service` / `vta-service` / `did-hosting-daemon` sources.

---

## 1. How this differs from the existing flow

The other mode (`vta_only`) and Full Stack differ in **who owns the mediator, the DID
host, and whether there's a community layer at all**:

| | `vta_only` | `full_stack` (this design) |
| --- | --- | --- |
| Mediator | **Shared**, external. VTA points at `MEDIATOR_DID` via `[messaging] kind = "existing"`. | **Per-deployment**, in-cluster. VTA *creates* it via `[messaging] kind = "create_mediator"`, then the mediator is provisioned + run in the same namespace. |
| DID host | **Shared**, external (`DID_HOSTING_SERVER_URL`). API uploads the VTA DID log to it via the control client. | **Per-deployment**, in-cluster. A fresh `did-hosting-daemon` is provisioned + run in the same namespace; the VTA + mediator DID logs are uploaded to *it*. |
| VTC | none | **Always provisioned.** A `vtc-service` wired to *this session's own* VTA, mediator, and dids daemon. |
| URLs returned | 1 (`vta-{vta_name}.{domain}`) | 4 (vta / mediator / dids / vtc — see [§3](#3-urls--dns)) |
| Subdomain | `vta-{vta_name}` | four name-derived hosts (see [§3](#3-urls--dns)) |
| Secret backend | HashiCorp Vault (`[secrets] backend = "vault"`) | **All four — VTA, mediator, dids, vtc — Vault, kubernetes auth** (same SA, same role) |
| Mediator storage | n/a (shared) | secrets in **Vault**; messages in **fjall** (file-backed, on the mediator PVC) — no Redis/Valkey |
| K8s resources / session | 1 PVC + Deployment + Service + Ingress | 4× (PVC + Deployment + Service + Ingress) |

The existing building blocks are **reused unchanged**: per-user namespace +
ServiceAccounts (`EnsureUserEnvironment`), the Cloudflare A-record helpers, the Job
watch/log-capture helpers (`WaitForJob`, `JobLogs`, `StreamJobLogs`), and the
`---ARTIFACT:…---` stdout-marker trick for pulling files out of a finished Job.
`EnsureUserAccess` (the per-user Vault policy + kubernetes-auth role) is **also reused**
for the VTA — `full_stack` only extends the same policy to also grant the mediator's,
dids daemon's, and VTC's KV prefixes, and every component authenticates against the same
role via the same `vta` ServiceAccount. No per-session token to mint, no plaintext
fallback.

---

## 2. Component topology (one user namespace)

```text
namespace: vtafarm-user-{userID}
┌────────────────────────────────────────────────────────────────────────────────────────┐
│   services & ingresses (created up front; 503 until pods come up)                       │
│   ┌───────────────┐  ┌───────────────────┐  ┌────────────────┐  ┌───────────────┐       │
│   │ fs-{sid}-vta  │  │ fs-{sid}-mediator │  │ fs-{sid}-dids  │  │ fs-{sid}-vtc  │       │
│   │ :8100         │  │ :7037             │  │ :8534          │  │ :8200         │       │
│   └───────┬───────┘  └─────────┬─────────┘  └───────┬────────┘  └───────┬───────┘       │
│           │ Ingress            │ Ingress            │ Ingress           │ Ingress       │
│   ┌───────▼───────┐  ┌─────────▼─────────┐  ┌───────▼────────┐  ┌───────▼───────┐       │
│   │ Deployment    │  │ Deployment        │  │ Deployment     │  │ Deployment    │       │
│   │ /work/vta     │  │ /work/mediator    │  │ /work/dids     │  │ /app/vtc      │       │
│   └───────────────┘  └───────────────────┘  └────────────────┘  └───────────────┘       │
│                                                                                         │
│   Setup Jobs mount whichever PVC(s) they touch; cross-component steps mount two         │
│   PVCs at once (e.g. /work/vta + /work/mediator). Steps run strictly in sequence,       │
│   so plain RWO PVCs are never mounted concurrently. Ingress host is the session's        │
│   name-derived subdomain (see §3), not the literal component name shown above.           │
└────────────────────────────────────────────────────────────────────────────────────────┘
```

How the four are wired to each other:

```text
   vta ──── vta_did ──── mediator          (VTA creates the mediator: [messaging] kind = "create_mediator")
   vta ──── did-mgmt servers add ──── dids (§6 step_vta_register_dids — registers the daemon in the VTA's registry)
   vtc ──── [webvh] server_id = "dids" ──► dids  (the VTC's did:webvh is hosted by the session's own daemon)
   vtc ──── [messaging].mediator_did ────► mediator (the VTC routes DIDComm through the same mediator)
   vtc ──── ephemeral setup key ─────────► vta  (§4b — provisioning handshake, then discarded)
```

Each component's PVC, Service, Ingress, and Deployment all share **one name**:
`fs-{sid}-vta` / `fs-{sid}-mediator` / `fs-{sid}-dids` / `fs-{sid}-vtc`
(`internal/k8s/fullstack_names.go` — `FSVtaName`, `FSMediatorName`, `FSDidsName`,
`FSVtcName`).

| Component | Port | Public URL | Mount | Storage | Notes |
| --- | --- | --- | --- | --- | --- |
| VTA | 8100 | `vta-{vta_name}.{domain}` | `/work/vta` | PVC 200Mi + **Vault** seed | reuses `vta_only`'s VTA resources + Vault path |
| Mediator | 7037 | `mediator-{vta_name}.{domain}` | `/work/mediator` | PVC 1Gi + **Vault** (`vault://`, kubernetes auth) | config + `fjall` message store on the PVC; no Redis/Valkey |
| DID Hosting daemon | 8534 | `dids-{vta_name}.{domain}` | `/work/dids` | PVC 200Mi + **Vault** (kubernetes auth) | standard (integrated) topology |
| VTC | 8200 | `vtc-{vtc_name}.{domain}` | `/app/vtc` | PVC 200Mi + **Vault** (kubernetes auth) | mounted at `/app/vtc` to match the image's own `WORKDIR`/entrypoint |

Every component is one PVC + one Deployment + one Service + one Ingress, built on the
generic `ComponentJobSpec` / `ComponentDeploymentSpec` helpers — no per-component K8s
primitives. Nothing is shared between them at runtime; the cross-component file handoffs
happen only during setup ([§4](#4-cross-component-file-handoffs)).

Per-component resource costs (CPU **requests** only — deliberately no CPU limits — and
memory limits) are mirrored in `internal/capacity/capacity.go` for admission/capacity
planning; keep the two in sync.

---

## 3. URLs & DNS

`domain` is always `CLUSTER_DOMAIN` (never a request field, mirroring `vta_only`). The
backend derives four hostnames from the session's **names**, not from a random ID
(`internal/setup/subdomain.go`): three from `vta_name`, and the VTC's from its own
`vtc_name`, so the community's URL reads independently of the VTA it happens to sit on.

```text
vta-{vta_name}.{domain}
mediator-{vta_name}.{domain}
dids-{vta_name}.{domain}
vtc-{vtc_name}.{domain}
```

In development (`APP_ENV=development`) each gets a `dev-` prefix —
`dev-vta-{vta_name}`, `dev-mediator-{vta_name}`, `dev-dids-{vta_name}`,
`dev-vtc-{vtc_name}` — distinguishing local DNS records from production. It's a
prefix rather than an infix so every dev record sorts together in the zone; see
`setup.EnvPrefix` and `docs/custom-domain-design.md` §2.

`vta_name` and `vtc_name` are both validated by `setup.ValidateName`: lowercase letters
and digits joined by single hyphens, at most 48 characters (the longest component prefix,
`dev-mediator-`, is 13 chars against DNS's 63-char label limit). `vtc_name` is
additionally **unique across all sessions** — it becomes a public community identity, so
a collision is a 409 at `POST /setup`, not a silently shared name.

Four Cloudflare A-records are created **before any Job runs**, all pointing at
`CLUSTER_INGRESS_IP`, `proxied=true`:

```text
A   vta-{vta_name}.{domain}        →  {CLUSTER_INGRESS_IP}
A   mediator-{vta_name}.{domain}   →  {CLUSTER_INGRESS_IP}
A   dids-{vta_name}.{domain}       →  {CLUSTER_INGRESS_IP}
A   vtc-{vtc_name}.{domain}        →  {CLUSTER_INGRESS_IP}
```

DNS must exist first because the rendered recipes embed the final `https://…` URLs
(`public_url`, `webvh_url`, `[identity].public_url`, the VTC's `base_url`) into the DID
documents that get published. TLS is the cluster-wide wildcard, served by Traefik as its
default certificate (same as today's VTA Ingress — no per-host `tls:` block needed).

`CreateARecord` already returns a record ID; store all four for teardown.

Each hosted DID's **path component** (`did:webvh:<scid>:<dids host>:<path>`) is derived
by the same file: `{vta_name}-vta`, `{vta_name}-mediator`, `{vtc_name}-vtc`. Every
producer must agree on these — the webvh URLs rendered into the setup TOMLs mint the DIDs
at these paths, and `step_dids_load_did`'s `load-did --path` must load each log at the
**same** path or webvh resolution 404s. The `-vtc` suffix keeps the community's DID
distinct from the VTA's even when `vtc_name == vta_name`.

---

## 4. Cross-component file handoffs

On the single host, the bootstrap chain hands files between home dirs:
`mediator-setup` writes `~/mediator/bootstrap-request.json`, the VTA (running in
`~/vta`) reads it and writes `~/mediator/bundle.armor`, and `mediator-setup` phase 2
reads that back. Same pattern for the DID Hosting daemon via `~/dids/`.

On K8s there is no shared home filesystem, but the mapping stays simple: **mount each
component's PVC at its working dir, and give every cross-component Job both PVCs it
touches.** Because the setup steps run strictly in sequence (one Job finishes before
the next starts), a plain `ReadWriteOnce` PVC is never mounted by two pods at once — no
`ReadWriteMany`, no separate exchange volume, no stdout-capture plumbing needed.

| Job | PVCs mounted | Reads / writes across components |
| --- | --- | --- |
| `step_mediator_reprov` | `/work/vta` + `/work/mediator` | reads `/work/mediator/bootstrap-request.json`, writes `/work/mediator/bundle.armor` |
| `step_dids_provision` | `/work/vta` + `/work/dids` | reads `/work/dids/bootstrap-request.json`, writes `/work/dids/bundle.armor` |
| `step_dids_load_did` | `/work/vta` + `/work/dids` | reads `/work/vta/data/vta/did-logs/{VTA,mediator}-did.jsonl`, loads both into the dids daemon's local store |

All other setup Jobs mount only their own component's PVC. **The VTC's steps are no
exception** — `step_vtc_setup_key` writes `setup-key.json` onto the VTC's own PVC and
`step_vtc_setup` reads it back from the same PVC; nothing crosses a component boundary as
a *file* on the VTC side. The one value that does cross (the ephemeral setup key's
`did:key`) travels through the orchestrator as a parsed string, not a file — see
[§4b](#4b-the-vtcs-own-ephemeral-setup-key-handshake).

The recipes use **relative paths** for on-PVC files (`config.toml`, `data/vta`,
`conf/mediator.toml`, `setup-key.json`, …) exactly as the reference does; with
`workingDir` set to the mount path they resolve identically to the home-dir layout, so
the recipe bodies are essentially verbatim. (The mediator's `[secrets].storage` is the
exception — a `vault://` URL, not a PVC path.)

> The VTA's two DID logs (`VTA-did.jsonl`, `mediator-did.jsonl`) are loaded into the dids
> daemon's local store **offline**, before it ever starts — `step_dids_load_did`
> ([§6](#6-per-step-jobs)) mounts the vta PVC alongside the dids PVC and reads them
> straight off disk (`did-hosting-daemon load-did`), the same way
> `step_mediator_reprov`/`step_dids_provision` above mount two PVCs at once. No marker
> trick or in-memory round-trip through the orchestrator needed. The VTC's DID log is
> **not** loaded here — the VTC has no DID yet at that point in the pipeline, and when it
> does get one (`step_vtc_setup`, near the end) it is published over the daemon's live,
> authenticated hosted-publish path instead ([§4a](#4a-registering-the-sessions-own-dids-daemon-with-the-vta)).

### 4a. Registering the session's own dids daemon with the VTA

`vtc-service`'s non-interactive setup TOML can route the VTC's `did:webvh` through a
**registered** hosting server (`[webvh] server_id = "..."`) instead of self-hosting at
`base_url`. Making that server the session's own `fs-{sid}-dids` daemon requires the VTA
to know that server exists — the same registration `vta_only` already performs for its
optional external "DID Hosting Control" server:

```go
// internal/k8s/setup_jobs.go — CreateProvisionJob (vta_only)
cmd := fmt.Sprintf("vta import-did --did %s --role admin", adminDid)
if controlDid != "" {
    cmd += fmt.Sprintf(" && vta did-mgmt servers add --id control --did %s --label 'DID Hosting Control Plane'", controlDid)
}
```

`full_stack` performs the identical registration against the session's *own* daemon —
`step_vta_register_dids` ([§6](#6-per-step-jobs)), after `deploy_mediator` and before the
`awaiting_admin_did` gate:

```sh
vta did-mgmt servers add --id dids --did {{3d}} --label 'Session DID Hosting Daemon'
```

(`3d` = `SetupSession.DIDHostingDid`.) Placement is constrained from both sides:
`servers add` **live-resolves** the server DID and requires a hosting service in its
document (`vta-service/src/operations/did_webvh/servers.rs::validate_server_did`), so it
must run after `deploy_dids`; and it writes the VTA's fjall store, so it must run before
`deploy_vta`. The VTC then consumes it with `[webvh] server_id = "dids"` in its setup
TOML ([§7](#7-recipe-templates)).

Beyond the VTC, this step is what gives the VTA a publication target for any future DID
work the user does themselves (integrations provisioned with `--var WEBVH_SERVER=dids`,
promoting a DID to server-managed via `did-mgmt dids register --server dids`, …).
Nothing parsed; exit code 0 is success. (The Ubuntu reference flow has no equivalent step
— its uploads went through the daemon's admin browser UI — so this is K8s-mapping-only,
motivated by `vta_only` parity, not by Appendix A.)

**No separate step grants the VTA an ACL entry on its own daemon.** Hosted-mode
publication is a **live, authenticated** call: at `step_vtc_setup` time the running VTA
authenticates to the dids daemon *as its own `vta_did`* (challenge-response;
`operations/did_webvh/mod.rs::authenticated_server_transport` →
`load_vta_webvh_signing_identity`) and pushes the VTC's `did.jsonl`
(`transport.publish_did`). The daemon's ACL is **deny-by-default** —
`did-hosting-common/src/server/acl.rs::check_acl` returns `Forbidden: DID not in ACL` for
any DID without an entry — but as of upstream commit "a VTA-provisioned daemon trusts its
provisioning VTA to publish" (`affinidi-webvh-service`/`webvh-build-pipeline` `24ad22d`),
`step_dids_p2`'s offline-complete finalizer already seeds an idempotent **Admin**-role
entry for the provisioning VTA on its own — the same authorization `vta_only` gets from
`didHosting.CreateAcl(vtaDid, "service")` against its external host, just automatic here
instead of a Job. Earlier drafts of this design added a `step_dids_grant_vta`
(`did-hosting-daemon add-acl --role service`) to do it explicitly; it's gone — it collided
with the daemon's own auto-seeded entry ("ACL entry already exists — delete it first to
change the role") and failed the Job, and `Admin` is a strict superset of the `service`
role it requested, so nothing is lost.

### 4b. The VTC's own ephemeral setup-key handshake

Provisioning a VTC authenticates to the VTA with an ephemeral `did:key`
(`vta_sdk::provision_client::EphemeralSetupKey`) that must already hold an admin ACL entry
at the VTA *before* `vtc setup --from <toml>` runs — see `vtc-service`'s module docs
(`vtc-service/src/setup/from_toml.rs`) and `docs/03-vtc/getting-started.md`
§"Non-interactive setup" (both in the `verifiable-trust-infrastructure` repo). The
interactive wizard generates this key inline and pauses for the operator to grant it; a
K8s Job can't pause for a TTY prompt, so this pipeline mints and grants it itself,
offline, as its own pair of steps (`step_vtc_setup_key` / `step_vtc_acl_grant`,
[§5](#5-state-machine)/[§6](#6-per-step-jobs)):

```text
vtc setup --setup-key-out <path> [--context <id>]
```

Mints a fresh ephemeral `did:key`, persists it (0600, the same
`EphemeralSetupKey::persist_to` format `vtc setup --from` loads via `setup_key_file`), and
prints a block to **stderr** via the shared
`vta_sdk::provision_client::driver::run_phase1_init` helper (the same one
`mediator-setup --setup-key-out` and `did-hosting-daemon setup --setup-key-out` use):

```text
  Setup DID (ephemeral):
    did:key:z6Mk...

  Key stored at <path> (0600)

  Using your Personal Network Manager (PNM) connected to this VTA,
  create the vtc context and grant admin access to the setup DID:

    pnm contexts create --id <context> --name "VTC" \
      --admin-did did:key:z6Mk... --admin-expires 1h
  ...
```

(`--from` and `--setup-key-out` are mutually exclusive; `--context` defaults to
`"default"` and only shapes the printed grant command above — this pipeline passes the
same value as the phase-2 TOML's `context`, i.e. `s.VtcName`.) `ParseVtcSetupKeyDid`
([§8](#8-output-parsing-regex)) extracts the DID from the line following
`Setup DID (ephemeral):` — K8s Job logs capture stderr same as stdout, so no
command-level redirect is needed. (`vtc create-did-key` is *not* a substitute — it writes
an ACL entry into the VTC's own store and prints a credential; it does not persist the
`setup_key_file` JSON.)

The orchestrator then performs the grant the printed hint asks a human for, as
`step_vtc_acl_grant`, and the key is never used again after `step_vtc_setup` consumes it.

---

## 5. State machine

```text
pending
  → dns_provision          create 4 Cloudflare A-records
  → env_provision          EnsureUserEnvironment (ns + SAs + Role/RoleBinding) + EnsureUserAccess
                           (Vault policy/role covering the vta + mediator + dids + vtc KV prefixes)
  → k8s_provision          PVCs, Services, Ingresses for vta/mediator/dids/vtc
  → step_vta_setup         vta setup --from vta-setup.toml           → 1a VTA DID, 1b Mediator DID, DID logs
  → step_mediator_p1       mediator-setup (phase 1)                  → mediator/bootstrap-request.json
  → step_mediator_reprov   vta contexts reprovision                  → mediator/bundle.armor, 2a digest, 2b admin DID
  → step_mediator_p2       mediator-setup --bundle --digest          → 2c admin priv key
  → step_dids_p1           did-hosting-daemon setup (offline-prepare)→ dids/bootstrap-request.json
  → step_dids_provision    vta bootstrap provision-integration       → dids/bundle.armor, 3a digest
  → step_dids_p2           did-hosting-daemon setup (offline-complete)→ 3b admin DID, 3c admin priv key, 3d daemon DID
  → step_dids_invite       did-hosting-daemon invite --role admin    → 3e dids admin-enroll URL (returned to user)
  → step_dids_load_did     did-hosting-daemon load-did (mediator + VTA DID logs, offline, into the dids
                           local store — not the VTC's, which has no DID yet; §4)
  → deploy_dids            Deployment dids-daemon (start it)
  → deploy_mediator        Deployment mediator (start it)
  → step_vta_register_dids vta did-mgmt servers add --id dids --did {{3d}}   (daemon live+resolvable; VTA store still free)
  → awaiting_admin_did     ⏸ gate: wait for user's PNM admin DID (skip if admin_did given at POST /setup)
  → step_import_admin_did  vta import-did --role admin --label pnm-bootstrap --did {{admin_did}}
  → step_vtc_setup_key     vtc setup --setup-key-out                 → 5a ephemeral setup did:key      (§4b)
  → step_vtc_acl_grant     vta contexts create --id {{vtc_name}} --name "VTC"
                           --admin-did {{5a}} --admin-expires 1h                                       (§4b)
  → deploy_vta             Deployment vta (start it)
  → step_vtc_setup         LIVE vtc setup --from vtc-setup.toml (VTA + mediator + dids all reachable)
                           → 5b vtc DID, 5c vtc admin DID, 5d install URL, 5e claim code
  → deploy_vtc             Deployment fs-{sid}-vtc (start it)
  → running                return 4 URLs + 1a VTA DID + 3e dids admin-enroll URL + 5b vtc DID
                           + 5d/5e (once)
        ↓ (any step)
     failed
```

**Why this order.** Setup/config order is VTA → mediator → dids → vtc (each later recipe
consumes a DID or bundle an earlier one produced). But the *start* order is **dids first,
then mediator, then VTA, then VTC** — the first three exactly as the reference: the dids
daemon must already have the VTA's and mediator's DIDs loaded into its local store
*before* it starts serving (`step_dids_load_did` runs offline, right before
`deploy_dids`), so that when the mediator and VTA boot and resolve their own
`did:webvh:...` identities against `dids-{vta_name}.{domain}`, the documents are already
there — otherwise their first-boot DID resolution 404s. The mediator must also be
reachable before the VTA boots, and the VTA must have the admin DID imported before it
starts. The `vta contexts reprovision`, `vta bootstrap provision-integration`,
`vta import-did`, and `vta contexts create` commands are **CLI operations against the
VTA's PVC**, not HTTP calls, so they run as Jobs without the VTA server running.

The VTC comes last for the opposite reason: `step_vtc_setup` is the **only genuinely
live, network-dependent step in the whole pipeline** — a real round-trip against the VTA
(plus mediator resolution via `[messaging]` and the daemon-hosted publish via `[webvh]`)
— so everything it depends on must already be up. Its two *offline* prerequisites
(`step_vtc_setup_key`, `step_vtc_acl_grant`) go in the post-gate VTA-store window
instead: after `step_import_admin_did`, before `deploy_vta`, as one contiguous block
right before the components that consume them.

**Why the grant sits post-gate.** `step_vtc_acl_grant`'s `--admin-expires 1h` starts
ticking the moment it runs. Placing it *after* the `awaiting_admin_did` gate (rather than
before) keeps the grant-to-use window at minutes no matter how long a human takes at the
gate, so the TTL never needs thinking about.

**Why no ACL-grant gate and no service downtime.** Every offline step above runs against a
store whose daemon isn't running at that moment — the same trick `step_mediator_reprov`,
`step_dids_load_did`, and `step_import_admin_did` already rely on. Because `full_stack`
**always builds the whole stack fresh in the same run** (it never attaches a VTC to an
already-running session — see [Appendix B](#appendix-b--vtc-design-rationale)):

- No `awaiting_*_grant` pause — nothing needs a human in the loop for the VTC, since
  vtafarm-api both generates the ephemeral key *and* grants its ACL itself.
- No scaling any Deployment to 0 first — each offline step lands in a window where its
  target store has never been claimed yet.

Each `step_*` is a `WaitForJob` + `JobLogs` + parse cycle exactly like `vta_only`'s
`runSetup`. The DB `Status` column carries the state-machine value; `Resume` re-attaches
in-flight steps on restart. Re-running a phase from its top re-creates Jobs idempotently
(AlreadyExists ignored, `WaitForJob` re-attaches by name); `step_vtc_acl_grant`
additionally needs its Conflict case tolerated ([§6](#6-per-step-jobs)).

**Shared steps with `vta_only`.** `dns_provision`, `step_vta_setup`, `awaiting_admin_did`,
`step_import_admin_did`, `deploy_vta`, and the terminal `running` status are the **same
steps, same names** as the VTA-Only machine (`vta-setup-design.md`, Mode A). `full_stack`
adds the mediator/dids steps between `step_vta_setup` and the gate, the VTC steps around
`deploy_vta`, and splits `env_provision` / `k8s_provision` out of `step_vta_setup` (four
components to provision instead of one). In `vta_only` those shared names map onto the
implemented DB statuses `dns_provisioned` / `vta_setup_running` → `vta_setup_complete` /
`provisioning` → `running`.

**PNM binding is user-local — exactly as `vta_only`.** The orchestrator never runs `pnm
setup` or `pnm setup continue`; those happen on the user's own machine. The only
PNM-related thing the API does is `vta import-did --role admin --did <admin_did>`, where
`admin_did` is the DID the user's local `pnm setup` produced (`4a`). Mirroring `vta_only`,
`admin_did` reaches the API one of two ways: as a field on `POST /setup` (the machine runs
straight through), or — when omitted — via `POST /setup/:id/admin` after the rest of the
stack is up, which is why the machine parks at `awaiting_admin_did`. The user then
completes the bind locally with `pnm setup continue <name> --vta-did <1a>`, so the API
must hand back the **VTA DID (`1a`)**.

---

## 6. Per-step Jobs

All Jobs: `RestartPolicy: Never`, `BackoffLimit: 0`, TTL 3600s. Each mounts its component
PVC at its working dir with `workingDir` set to the same path (so the recipe's relative
paths resolve), plus the recipe as a ConfigMap at `/config`. Cross-component Jobs mount a
second PVC ([§4](#4-cross-component-file-handoffs)). All of them use
`internal/k8s/component_jobs.go`'s `ComponentJobSpec` / `CreateComponentJob` — one generic
helper, no per-component K8s plumbing.

**ServiceAccount is per-Job, not per-component**: anything that touches the Vault-backed
secret store — `step_mediator_p1`/`p2`, `deploy_mediator`, `step_dids_p1`/`p2`,
`deploy_dids`, `step_vtc_setup`, `deploy_vtc`, plus every VTA-side `vta …` Job — runs as
SA `vta` (bound to the per-user kubernetes-auth role). The Jobs that never touch secrets
(`step_dids_invite`, `step_dids_load_did`, `step_vtc_setup_key`) stay on SA
`pod-operator`. Image per component comes from the request (see
[§10](#10-data-model-changes)).

### Step `step_vta_setup` — VTA Job (workingDir `/work/vta`)

Renders `vta-setup.toml` exactly like `vta_only` — **same Vault `[secrets]` block** — but
with `[messaging] kind = "create_mediator"` instead of `kind = "existing"`. That
`[messaging]` block is the only difference from the live `vta_only` template. Command
appends both DID logs behind markers:

```sh
vta setup --from /config/vta-setup.toml \
  && echo '---ARTIFACT:VTA-did.jsonl---'      && cat data/vta/did-logs/VTA-did.jsonl \
  && echo '---ARTIFACT:mediator-did.jsonl---' && cat data/vta/did-logs/mediator-did.jsonl
```

Parse from stdout: **1a** VTA DID (`(?i)vta did:\s*(did:\S+)`), **1b** Mediator DID
(`(?i)mediator:\s*(did:\S+)`), and both DID-log JSONL blobs (kept for the load step).

### Step `step_mediator_p1` — Mediator Job (workingDir `/work/mediator`, SA `vta`)

No Vault env vars needed — auth is kubernetes auth, baked into the recipe's
`vault://…?auth=kubernetes&role=…` URL ([§7](#7-recipe-templates)); the mediator binary
exchanges its own pod's ServiceAccount JWT for a Vault token, same mechanism as the VTA.

```sh
mediator-setup --from /config/mediator-recipe.toml
```

Writes `bootstrap-request.json` to the mediator PVC and the `fjall` store under
`data/mediator/`, and stores the ephemeral HPKE seed **in Vault** under the mediator's
`vault://…` prefix — no `conf/secrets.json` is produced (secrets live in Vault).

### Step `step_mediator_reprov` — VTA Job (mounts `/work/vta` + `/work/mediator`, workingDir `/work/vta`)

```sh
vta contexts reprovision \
  --id mediator \
  --recipient /work/mediator/bootstrap-request.json \
  --out /work/mediator/bundle.armor
```

Parse: **2a** SHA-256 digest (`SHA-256 digest:\s+(\S+)`), **2b** Admin DID
(`Admin DID:\s+(did:\S+)`).

### Step `step_mediator_p2` — Mediator Job (workingDir `/work/mediator`, SA `vta`)

Same kubernetes-auth Vault access as Phase 1 — Phase 2 reads back the seed Phase 1
stored in Vault, re-authenticating fresh (no token to keep alive between the two Jobs).

```sh
mediator-setup --from /config/mediator-recipe.toml \
  --bundle bundle.armor \
  --digest {{2a}}
```

Provisions the unified secret backend **into Vault** (`mediator_jwt_secret`,
`mediator_operating_secrets`, `mediator_admin_credential`,
`mediator/vta/last_known_bundle`) and writes `conf/mediator.toml` +
`conf/atm-functions.lua` to the PVC. The success line reads
`Provisioning unified secret backend: vault://…` (not `file://`), and no
`conf/secrets.json` is produced. Parse **2c** Admin private key
(`Private key \(multibase\):\s+(\S+)`) — already stored in Vault too — and return it to
the user once for offline backup ([§9](#9-secrets-handling)).

### Step `step_dids_p1` — DIDS Job (offline-prepare, workingDir `/work/dids`, SA `vta`)

```sh
did-hosting-daemon setup --from /config/webvh-recipe.toml
```

Recipe `vta_mode = "offline-prepare"`, `[vta] request_path = "bootstrap-request.json"`.
Writes that request file + stores the offline-bootstrap seed in Vault (kubernetes auth,
same mechanism as the VTA and mediator). Nothing to parse (the `client_did` line is
informational).

### Step `step_dids_provision` — VTA Job (mounts `/work/vta` + `/work/dids`, workingDir `/work/vta`)

```sh
vta bootstrap provision-integration \
  --request /work/dids/bootstrap-request.json \
  --out /work/dids/bundle.armor \
  --create-context
```

Parse **3a** SHA-256 digest.

### Step `step_dids_p2` — DIDS Job (offline-complete, workingDir `/work/dids`, SA `vta`)

Re-render the recipe (second ConfigMap) with `vta_mode = "offline-complete"` and the
`[vta]` block carrying `bundle_path = "bundle.armor"`, `expect_digest = {{3a}}`. Writes
the daemon's final secrets to Vault (kubernetes auth), and seeds the idempotent Admin ACL
entry for the provisioning VTA ([§4a](#4a-registering-the-sessions-own-dids-daemon-with-the-vta)).

```sh
did-hosting-daemon setup --from /config/webvh-recipe.toml \
  && echo '---ARTIFACT:server_did---' && grep '^server_did' config.toml
```

Parse: **3b** Admin DID (`Generated admin did:key:\s+(did:\S+)`), **3c** Admin private
key (`Private key \(save now, not re-shown\):\s+(\S+)` → return to user once), **3d**
Daemon DID (the `server_did` value from `config.toml`).

### Step `step_dids_grant_farm` — DIDS Job (`did-hosting-daemon add-acl`, workingDir `/work/dids`, SA `pod-operator`)

Puts vtafarm-api's own client DID (`DID_HOSTING_DID`) in this daemon's ACL as
`admin`, before the daemon ever starts:

```sh
did-hosting-daemon list-acl 2>&1 | grep -qF {{DID_HOSTING_DID}} \
  || did-hosting-daemon add-acl --did {{DID_HOSTING_DID}} --role admin --label vtafarm
```

Runs for **every** `full_stack` session. The farm operates these deployments on
its customers' behalf, and managing the `did.jsonl` documents a daemon serves
means holding an ACL entry on it — `admin` specifically, since that is the role
that bypasses the per-DID ownership check on the publish endpoints. It does not
widen the trust boundary: the pod, its PVC and its Vault access are already
ours, so this only makes control we necessarily have reachable through the API
rather than only through the cluster.

The platform stack additionally **depends** on it, because that daemon is the
**shared** DID host: every `vta_only` session's DID log is uploaded to it by
this API under the same keypair, and without the entry those sessions provision
and then silently fail to publish.

The daemon's own finalizer seeds an entry for the provisioning VTA (§4a), never
for this one — it derives that DID from the armor bundle and knows nothing about
the farm's keypair.

It has to be offline, in the same no-pod window as `step_dids_invite` and
`step_dids_load_did`: the control API authenticates callers *from* the ACL, so
enrolling over HTTP would require already being enrolled.

The `list-acl` probe is not decoration — `add-acl` **fails** on an existing DID
("ACL entry already exists — delete it first to change the role") rather than
treating it as satisfied, so without the probe a resumed or retried run would
fail the whole stack. This is the same sharp edge that killed the earlier
`step_dids_grant_vta` (§4a).

### Step `step_dids_invite` — DIDS Job (`did-hosting-daemon invite`, workingDir `/work/dids`, SA `pod-operator`)

Mints **3e**, the dids admin-panel enrollment URL — the value the user logs in with:

```sh
did-hosting-daemon invite --role admin --did {{3b}}
```

It opens the local store directly, so it must run **before** `deploy_dids` (while no
daemon holds the PVC). Parse **3e** from the output and return it to the user as a
**required** output ([§12](#12-api-surface)) — not optional. The automated DID-log load
(`step_dids_load_did`, next) removes the *functional* need to log in, but this URL is what
lets the user reach the dids admin web UI afterward. It is single-use; regenerate via the
reissue endpoint.

### Step `step_dids_load_did` — DIDS Job (mounts `/work/vta` + `/work/dids`, workingDir `/work/dids`, SA `pod-operator`)

Loads the VTA's and mediator's DID logs into the dids daemon's local store **offline**,
reading them straight off the vta PVC where `step_vta_setup` wrote them — no round-trip
through the orchestrator, no HTTP control-API call, no running daemon required:

```sh
did-hosting-daemon load-did --path {{vta_name}}-mediator --did-log /work/vta/data/vta/did-logs/mediator-did.jsonl \
  && did-hosting-daemon load-did --path {{vta_name}}-vta --did-log /work/vta/data/vta/did-logs/VTA-did.jsonl
```

`--path` must match the path component the DID was minted with
([§3](#3-urls--dns)) or webvh resolution 404s. Like `step_dids_invite`, it opens the local
store directly, so it must run **before** `deploy_dids` — while no daemon pod holds the
dids PVC. This is what makes both `dids` and `mediator` able to resolve their own
`did:webvh:...` identities successfully on their very first boot: by the time either
process starts, the documents are already in the store. (An earlier draft of this design
tried registering the DID logs over the daemon's control API *after* `deploy_dids` — that
requires the daemon to already be running and reachable, which is backwards: the
mediator/dids themselves need the DIDs resolvable *before* they start, not after.)
Nothing to parse — success is just exit code 0.

### `deploy_dids` — start the daemon Deployment

Deployment with `workingDir = /work/dids`, command `["did-hosting-daemon"]`, SA `vta`
(reads its Vault-backed secrets at every boot), mounting the dids PVC, plus the
Service + Ingress for `dids-{vta_name}.{domain}` (created in `k8s_provision`, now backed
by a running pod). **Waits for Ready** (`WaitForComponentDeploymentReady`, 2 min timeout)
before the step returns — `step_vta_register_dids` right after `deploy_mediator`
live-resolves this daemon over HTTPS, and a `Deployment` object existing is not the same
as the pod (and its Service/Ingress endpoint) actually serving traffic. *(This wait was
documented from the start but not actually wired up until a live session hit the resulting
503 race — see checklist item 15.)*

### `deploy_mediator` — start the mediator Deployment

Deployment with `workingDir = /work/mediator`, command `["mediator"]`, SA `vta`, mounting
the mediator PVC (it reads `conf/mediator.toml` and the `fjall` message store from there).
No Vault env vars — the mediator reads its secrets from Vault at startup and *probes*
(write→read→delete a sentinel) using kubernetes auth, re-authenticating with its pod's own
ServiceAccount JWT on every restart. Service + Ingress for
`mediator-{vta_name}.{domain}`. No Redis/Valkey dependency. **Waits for Ready**, same as
`deploy_dids` — `step_vtc_setup` resolves the mediator later in the run, and the invariant
"a `deploy_*` step doesn't return until its component is actually up" should hold
uniformly.

### Step `step_vta_register_dids` — VTA Job (workingDir `/work/vta`)

Registers the session's dids daemon (`3d`) in the VTA's webvh server registry — the
`full_stack` counterpart of the `--id control` registration `vta_only`'s provision job
chains after `import-did`:

```sh
vta did-mgmt servers add --id dids --did {{3d}} --label 'Session DID Hosting Daemon'
```

`servers add` resolves the server DID **live** at add time and requires a
`WebVHHosting`-family service in the resolved document
(`vta-service/src/operations/did_webvh/servers.rs::validate_server_did`), which is why
this runs **after** `deploy_dids`/`deploy_mediator` — the daemon must already be serving
its own `did.jsonl`. It writes the VTA's fjall store offline, so it still runs **before**
`deploy_vta` (same window as `step_import_admin_did`). Nothing to parse. Rationale and the
VTC's consumption of it: [§4a](#4a-registering-the-sessions-own-dids-daemon-with-the-vta).

### Step `step_import_admin_did` — VTA Job (workingDir `/work/vta`)

PNM binding is the **user's local job**, exactly as in `vta_only` — the orchestrator never
runs `pnm setup` or `pnm setup continue`. The user runs `pnm setup --name <name>` on their
own machine, which mints their PNM **admin DID** (`4a`), and hands that DID to the API (as
`admin_did` on `POST /setup`, or later via `POST /setup/:id/admin` — see
[§12](#12-api-surface)). The API's only PNM-related action is to grant that DID admin
access on the VTA — identical to the `vta_only` provision job:

```sh
vta import-did --role admin --label pnm-bootstrap --did {{admin_did}}
```

`vta import-did` is CWD-based (no `--config`); `workingDir = /work/vta` supplies it. It
mutates the VTA PVC, so it runs as a Job **before** `deploy_vta`, while no VTA pod holds
the PVC (the same reason `vta_only` imports before creating the Deployment). Nothing is
parsed here — `admin_did` is a user input, not a value extracted from logs.

The user finishes the binding locally with `pnm setup continue <name> --vta-did {{1a}}`,
which is why the **VTA DID (`1a`)** must be returned to them ([§12](#12-api-surface)).

### Step `step_vtc_setup_key` — VTC Job (workingDir `/app/vtc`, SA `pod-operator`)

Phase 1 of `vtc-service`'s headless two-phase setup ([§4b](#4b-the-vtcs-own-ephemeral-setup-key-handshake)).

```go
k8s.ComponentJobSpec{
    Name:           k8s.FSJobVtcSetupKey(s.ID),
    Image:          s.VtcImage,
    Command:        []string{"sh", "-c", fmt.Sprintf(
        "vtc setup --setup-key-out /app/vtc/setup-key.json --context %s",
        shellQuote(s.VtcName),
    )},
    WorkingDir:     "/app/vtc",
    ServiceAccount: k8s.PodOperatorServiceAccount, // no Vault access needed for this step
    PVCMounts:      []k8s.PVCMount{{Name: "vtc-data", ClaimName: k8s.FSVtcName(s.ID), MountPath: "/app/vtc"}},
    Env:            fsNoColorEnv(),
}
```

`/app/vtc` matches the vtc image's own Dockerfile `WORKDIR` — see `deploy_vtc` below.
Parse **5a** from the line following `Setup DID (ephemeral):` —
`Setup DID \(ephemeral\):\s+(did:\S+)` — rather than a machine `key=value` line; persist to
`SetupSession.VtcSetupKeyDid` (for debuggability/audit — nothing downstream reads it back
from the DB, `step_vtc_acl_grant` gets it straight from this Job's own logs in the same
orchestrator run).

### Step `step_vtc_acl_grant` — VTA Job (workingDir `/work/vta`, post-gate, before `deploy_vta`)

```go
k8s.ComponentJobSpec{
    Name:           k8s.FSJobVtcAclGrant(s.ID),
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

`s.VtcName` doubles as the VTA context id the VTC's community lives under — using it
rather than a fixed `"default"` avoids collisions if the VTA ever ends up hosting more
than one context. Nothing to parse.

`vta contexts create` is offline (fjall) with exactly these flags — `--id`, `--name`,
`--admin-did` (atomically writes an admin ACL entry scoped to the new context),
`--admin-expires N[s|m|h|d|w]` (swept by the running VTA's ACL sweeper). Because this runs
post-gate, minutes before `step_vtc_setup` consumes it, `1h` is comfortable
([§5](#5-state-machine)).

**Resume:** on re-run the create errors `Conflict: context already exists`; since resume
re-mints a *fresh* setup key first, the fallback grant for an existing context is
`vta import-did --did <5a> --role admin --context {{vtc_name}} --label vtc-setup`
(offline, upserts, context-scoped — the same command family the PNM import already uses;
no expiry flag, acceptable for the retry path). The implemented command wraps both:
it runs `create`, and only on an `already exists` failure falls through to the regrant —
any other non-zero exit still fails the Job with its original code.

### `deploy_vta` — start the VTA Deployment

Deployment with `workingDir = /work/vta`, command `["vta"]`, mounting the vta PVC, plus
Service + Ingress for `vta-{vta_name}.{domain}`. **Waits for Ready** before the pipeline
moves on — `step_vtc_setup` immediately afterwards makes live authenticated calls against
this VTA, so "the Deployment object exists" is not good enough.

### Step `step_vtc_setup` — VTC Job (workingDir `/app/vtc`, SA `vta`)

The one genuinely live step of the whole pipeline.

```go
k8s.ComponentJobSpec{
    Name:           k8s.FSJobVtcSetup(s.ID),
    Image:          s.VtcImage,
    Command:        []string{"sh", "-c", "vtc setup --from /config/vtc-setup.toml"},
    WorkingDir:     "/app/vtc",
    ServiceAccount: k8s.VtaServiceAccount, // needs Vault (kubernetes auth) — §9
    PVCMounts:      []k8s.PVCMount{{Name: "vtc-data", ClaimName: k8s.FSVtcName(s.ID), MountPath: "/app/vtc"}},
    ConfigMapName:  k8s.FSJobVtcSetup(s.ID),
    ConfigMapKey:   "vtc-setup.toml",
    ConfigMapData:  toml, // §7
    Env:            fsNoColorEnv(),
}
```

`setup-key.json` (written by `step_vtc_setup_key`, on the same PVC, already ACL-granted by
`step_vtc_acl_grant`) is loaded via the TOML's `setup_key_file = "setup-key.json"`
(relative path, resolved by `workingDir`). Parse the terse completion block
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

One parser, `ParseVtcSetupOutput(logs string) (VtcSetupOutcome, error)`, extracts all five
`key=value` lines in one pass (`(?m)^vtc_did=(.+)$` etc.) — unlike this pipeline's other
parsed values, these aren't embedded in prose, so there's nothing per-field to
disambiguate. **`admin_did` here (`5c`) is the VTC's own pre-claim install admin, not the
PNM `admin_did` (`4a`)** that `step_import_admin_did` uses — it gets its own column
([§10](#10-data-model-changes)); don't overload the existing one.

> **`install_url` expires fast.** The setup-minted install token uses the VTC's default
> TTL — `INSTALL_TOKEN_DEFAULT_TTL_SECS` = **15 minutes**
> (`vtc-service/src/install/token.rs`). Users polling `GET /setup/:id` after a
> long-running pipeline will usually find it already dead, which is why the reissue
> endpoint ([§12](#12-api-surface)) is a **required** part of this mode, not a
> nice-to-have — the frontend should offer "mint a fresh install link" as the normal path
> whenever the token's mint time is stale.

### `deploy_vtc` — start the VTC Deployment

```go
k8s.ComponentDeploymentSpec{
    Name:           k8s.FSVtcName(s.ID),
    Image:          s.VtcImage,
    Command:        nil, // image entrypoint — see below
    WorkingDir:     "/app/vtc",
    ServiceAccount: k8s.VtaServiceAccount, // reads its Vault key bundle at every boot
    PVCMounts:      []k8s.PVCMount{{Name: "vtc-data", ClaimName: k8s.FSVtcName(s.ID), MountPath: "/app/vtc"}},
    Port:           8200,
    Labels:         fsLabels("vtc", s.ID),
}
```

Unlike `fsDeployVta`/`Mediator`/`Dids` (which set `Command` explicitly and never invoke
their images' `entrypoint.sh` at all), `Command` is deliberately left `nil` here — the vtc
image's entrypoint gates startup on a data/config presence check before `exec`ing the
binary, which is worth keeping. That wrapper checks `/app/vtc/{data,config.toml}`,
matching every one of this farm's images' Dockerfile `WORKDIR` (`/app/<name>` — vta,
mediator, did-hosting-daemon, vtc all agree), so the PVC is mounted at `/app/vtc` here too
— same path across all three VTC Job/Deployment specs, so the image's own entrypoint finds
what `step_vtc_setup`/`step_vtc_setup_key` wrote. Note this Deployment has no readiness
probe (a running container is Ready by default absent one), so
`WaitForComponentDeploymentReady` only confirms the process started, not that port 8200 is
actually accepting connections.

Service + Ingress for `fs-{sid}-vtc` are created up front in `k8s_provision`
([§2](#2-component-topology-one-user-namespace)), same pattern as the other three.

---

## 7. Recipe templates

PVCs mount at each component's working dir with matching `workingDir`, so the recipes keep
the reference's **relative paths verbatim**. Templated fields in `{{…}}`.

<!-- markdownlint-disable MD033 -->

<details><summary><b>vta-setup.toml</b></summary>

```toml
config_path = "config.toml"
data_dir    = "data/vta"
vta_name    = "{{ .VtaName }}"
public_url  = "{{ .VtaPublicURL }}"        # https://vta-{vta_name}.{domain}

[services]
rest    = true
didcomm = true
tsp     = true

[server]
host = "0.0.0.0"
port = 8100

[log]
level  = "info"
format = "text"

[secrets]                              # identical to vta_only — the VTA seed lives in Vault
backend     = "vault"
addr        = "{{ .Vault.Addr }}"
secret_path = "{{ .Vault.SecretPath }}"
kv_mount    = "{{ .Vault.KVMount }}"
secret_key  = "seed"
auth_method = "kubernetes"
k8s_role    = "{{ .Vault.K8sRole }}"
skip_verify = {{ .Vault.SkipVerify }}

[messaging]                            # ← full_stack: CREATE the mediator (vta_only uses kind="existing")
kind      = "create_mediator"
context   = "mediator"
url       = "{{ .MediatorURL }}"           # https://mediator-{vta_name}.{domain}/mediator/v1
webvh_url = "{{ .MediatorWebvhURL }}"      # https://dids-{vta_name}.{domain}/{vta_name}-mediator

[vta_did]
kind               = "create_webvh"
url                = "{{ .VtaDidWebvhURL }}" # https://dids-{vta_name}.{domain}/{vta_name}-vta
portable           = {{ .Portable }}
pre_rotation_count = {{ .PreRotationCount }}
```

</details>

<details><summary><b>mediator-recipe.toml</b></summary>

```toml
[deployment]
type      = "server"
protocols = ["didcomm", "tsp"]
use_vta   = true
vta_mode  = "sealed-export"

[vta]
context = "mediator"

[secrets]
# Vault KV v2: vault://<host[:port]>/<kv-mount>/<prefix>?auth=kubernetes&role=<role>.
# Kubernetes auth — the mediator exchanges its own pod's ServiceAccount JWT for a
# Vault token, same mechanism as the VTA. No VAULT_TOKEN env var, no per-session
# token to mint. `?insecure=1` for the self-signed in-cluster CA. The mediator image
# MUST be built with the `secrets-vault` feature. e.g.
# vault://vault.vault.svc:8200/secret/mediator/user-12/session-34?auth=kubernetes&role=vta-user-12&insecure=1
storage = "vault://{{ .Vault.HostPort }}/{{ .Vault.KVMount }}/{{ .Vault.MediatorPrefix }}?auth=kubernetes&role={{ .Vault.K8sRole }}&insecure=1"

[security]
ssl          = "none"
admin        = "generate"
jwt_mode     = "generate"
network_mode = "open"
cors         = "any"    # mediator-setup emits cors_allow_origin = "*" — browser clients (VTA Wallet) need it

[database]
url = "redis://127.0.0.1/"      # required field; unused with fjall storage (no Redis deployed)

[storage]
backend  = "fjall"
data_dir = "./data/mediator"    # file-backed store on the mediator PVC

[output]
config_path    = "conf/mediator.toml"
listen_address = "0.0.0.0:7037"
```

</details>

<details><summary><b>webvh-recipe.toml</b> (phase 1 → phase 3 differs only in <code>[deployment].vta_mode</code> and the <code>[vta]</code> block)</summary>

```toml
[deployment]
service  = "daemon"
vta_mode = "offline-prepare"          # phase 3: "offline-complete"

[output]
config_path = "config.toml"

[server]
host       = "0.0.0.0"
port       = 8534
log_level  = "info"
log_format = "text"
data_dir   = "data/daemon"

[identity]
public_url   = "{{ .PublicURL }}"      # https://dids-{vta_name}.{domain}
mediator_did = "{{ .MediatorDid }}"    # 1b
transport    = "both"

[vta]
request_path  = "bootstrap-request.json"   # phase 1 only
# phase 3 replaces the line above with:
# bundle_path   = "bundle.armor"
# expect_digest = "{{ .DidsDigest }}"        # 3a

[daemon]
enable_control = true
enable_server  = true
enable_witness = true
enable_watcher = false

[secrets]                              # identical mechanism to the VTA/mediator — kubernetes auth
backend           = "vault"
vault_addr        = "{{ .Vault.Addr }}"
vault_kv_mount    = "{{ .Vault.KVMount }}"
vault_secret_path = "{{ .Vault.SecretPath }}"   # dids/user-<id>/session-<id>/server-secrets
vault_auth_method = "kubernetes"
vault_k8s_role    = "{{ .Vault.K8sRole }}"
vault_skip_verify = {{ .Vault.SkipVerify }}

[admin]
mode = "generate"

[reprovision]
force = false
```

</details>

<details><summary><b>vtc-setup.toml</b></summary>

Renders the schema `vtc-service/src/setup/from_toml.rs::VtcWizardInputs` deserializes.
**`[secrets]` matches the VTA's own setup TOML shape**: the VTC's `SecretsConfig`
(`vtc-service/src/config.rs` — mirrors `vti_secrets::SecretsConfig`'s field names, and is
the same type the runtime `config.toml` uses) takes an optional tagged `backend = "vault"`
selector, same as `vta setup --from`'s. Setting `backend = "vault"` selects the store
explicitly and fails closed if `vault_addr` is missing, rather than relying on
`vault_addr`'s mere presence to activate Vault implicitly (omitting `backend` still falls
back to that implicit resolution). Every `vault_*` field is verified against that struct
(defaults: `vault_kv_mount = "secret"`, `vault_secret_key = "seed"`,
`vault_auth_method = "kubernetes"`).

```toml
config_path    = "config.toml"
base_url       = "{{ .VtcPublicURL }}"        # https://vtc-{vtc_name}.{domain}
vta_did        = "{{ .VtaDid }}"              # 1a
context        = "{{ .VtcName }}"
setup_key_file = "setup-key.json"

[webvh]
server_id = "dids"                            # registered by step_vta_register_dids (§4a)
path      = "{{ .VtcName }}-vtc"              # DID path — did:webvh:<scid>:<dids host>:<vtc_name>-vtc

[messaging]
mediator_did = "{{ .MediatorDid }}"           # 1b — the same shared mediator
mediator_url = "{{ .MediatorURL }}"           # informational only; endpoint is resolved from the DID doc
transports   = ["tsp", "didcomm"]

[secrets]
backend           = "vault"
vault_addr        = "{{ .Vault.Addr }}"
vault_kv_mount    = "{{ .Vault.KVMount }}"
vault_secret_path = "{{ .Vault.SecretPath }}"  # vtc/user-<id>/session-<id>/key-bundle
vault_secret_key  = "bundle"
vault_auth_method = "kubernetes"
vault_k8s_role    = "{{ .Vault.K8sRole }}"     # vta-user-<id> — reused, see §9
vault_skip_verify = {{ .Vault.SkipVerify }}
```

`[webvh].server_id = "dids"` matches the `--id dids` registered by `step_vta_register_dids`
([§4a](#4a-registering-the-sessions-own-dids-daemon-with-the-vta)) — the VTA resolves it
against its own registry. `path = "<vtc_name>-vtc"` pins the DID's path component instead
of letting the daemon auto-assign a random one, matching the `-vta`/`-mediator` convention
([§3](#3-urls--dns)). `domain` is left unset: the daemon resolves its default.

</details>

<!-- markdownlint-enable MD033 -->

---

## 8. Output parsing (regex)

| Step | Value | Regex / source |
| --- | --- | --- |
| `step_vta_setup` | 1a VTA DID | `(?i)vta did:\s*(did:\S+)` |
| `step_vta_setup` | 1b Mediator DID | `(?i)mediator:\s*(did:\S+)` |
| `step_vta_setup` | VTA + mediator DID logs | content after `---ARTIFACT:…---` markers |
| `step_mediator_reprov` | 2a digest | `SHA-256 digest:\s+(\S+)` |
| `step_mediator_reprov` | 2b admin DID | `Admin DID:\s+(did:\S+)` |
| `step_mediator_p2` | 2c admin priv key | `Private key \(multibase\):\s+(\S+)` |
| `step_dids_provision` | 3a digest | `SHA-256 digest:\s+(\S+)` |
| `step_dids_p2` | 3b admin DID | `Generated admin did:key:\s+(did:\S+)` |
| `step_dids_p2` | 3c admin priv key | `Private key \(save now, not re-shown\):\s+(\S+)` |
| `step_dids_p2` | 3d daemon DID | `server_did` value from `config.toml` |
| `step_dids_invite` | 3e dids admin-enroll URL | enrollment URL from `invite` output |
| `step_vtc_setup_key` | 5a ephemeral setup DID | `Setup DID \(ephemeral\):\s+(did:\S+)` (stderr; §4b) |
| `step_vtc_setup` | 5b VTC DID | `(?m)^vtc_did=(.+)$` |
| `step_vtc_setup` | 5c VTC install admin DID | `(?m)^admin_did=(.+)$` — **not** the PNM `4a` |
| `step_vtc_setup` | 5d install URL | `(?m)^install_url=(.+)$` |
| `step_vtc_setup` | 5e claim code | `(?m)^claim_code=(.+)$` |
| `POST /setup/:id/vtc/reissue-install` | 5d/5e reissued | `Install URL (one-shot):` / `Claim code …:` lines from `vtc admin invite` |

`5b`–`5e` come out of one parser pass (`ParseVtcSetupOutput`) over the terse completion
block; the others are prose-embedded and matched individually.

The bundles and bootstrap-request files are handoff **files**, not parsed values — they
live on the producing component's PVC and the consuming Job mounts that PVC
([§4](#4-cross-component-file-handoffs)).

**`4a` (PNM admin DID) is *not* parsed** — it is a user input (the user's local `pnm
setup` output) handed to the API and passed straight to `vta import-did`
([§6](#6-per-step-jobs)).

---

## 9. Secrets handling

**All four components use Vault, all four via kubernetes auth, all four through the same
per-user policy and role.** There is no token-auth path and no plaintext fallback anywhere
in `full_stack` — a deliberate simplification over an earlier design that gave the mediator
a separately-minted `VAULT_TOKEN`. Each pod authenticates as itself:

- **VTA** — `vta` ServiceAccount JWT against the per-user kubernetes-auth role
  `vta-user-<id>`. Master seed at `secret/vta/user-<id>/session-<id>/master-seed`.
  Unchanged from `vta_only`.
- **Mediator** — same SA, same role. Its recipe's `[secrets].storage` is a
  `vault://<host:port>/<kv-mount>/<prefix>?auth=kubernetes&role=<role>` URL
  ([§7](#7-recipe-templates)); the mediator binary reads its pod's own ServiceAccount JWT
  and exchanges it for a Vault token, exactly like the VTA does internally. Secrets land at
  `secret/data/mediator/user-<id>/session-<id>/{mediator_jwt_secret,…}`. Must be **built
  with the `secrets-vault` feature**
  (`--features "didcomm,redis-backend,fjall-backend,secrets-vault"`).
- **DID Hosting daemon** — same SA, same role. Its recipe's `[secrets]` block is a tagged
  `backend = "vault"` ([§7](#7-recipe-templates)), matching the VTA's own non-interactive
  schema, with secrets at `secret/data/dids/user-<id>/session-<id>/server-secrets`. Must be
  **built with the `vault-secrets` feature**.
- **VTC** — same SA, same role. Natively supports kubernetes auth (`vault_auth_method`
  defaults to `"kubernetes"`; its config's own doc comment notes the field names mirror the
  VTA's "so the shared Vault builder is reused"). Its key bundle lands at
  `secret/data/vtc/user-<id>/session-<id>/key-bundle` under field `bundle`. Must be **built
  with the `vault-secrets` feature** — see the note below.

> **VTC image build requirement.** `vault-secrets` is **not** a default feature of
> `vtc-service` (`Cargo.toml`: `vault-secrets = ["vti-secrets/vault-secrets"]`, listed as
> non-default in `docs/03-vtc/feature-flags.md`). The published VTC image must be built
> with `--features vault-secrets`, exactly analogous to the mediator's `secrets-vault` and
> the dids daemon's `vault-secrets` build requirements above. Without it,
> `create_secret_store` fails at setup with a feature-not-compiled error.

`vault_secret_key = "bundle"` stores the VTC's serialized `VtcKeyBundle` bytes under that
field name — `vti_secrets`'s generic seed store is byte-agnostic (the field is called
"seed" by default because the VTA uses it for a BIP-32 seed; the VTC reuses the same
abstraction for a different payload shape).

Because every component authenticates the same way as the same identity, `EnsureUserAccess`
only has to widen **one** policy, and mints nothing:

```hcl
path "secret/data/mediator/user-<id>/*"     { capabilities = ["create", "update", "read", "delete"] }
path "secret/metadata/mediator/user-<id>/*" { capabilities = ["read", "list", "delete"] }
path "secret/data/dids/user-<id>/*"         { capabilities = ["create", "update", "read", "delete"] }
path "secret/metadata/dids/user-<id>/*"     { capabilities = ["read", "list", "delete"] }
path "secret/data/vtc/user-<id>/*"          { capabilities = ["create", "update", "read", "delete"] }
path "secret/metadata/vtc/user-<id>/*"      { capabilities = ["read", "delete"] }
```

Per-prefix helpers in `internal/vault/client.go`: `MediatorPrefix`, `DidsPrefix`,
`VtcPrefix` (each `<component>/user-<id>/session-<id>`), with matching
`DeleteMediatorSecrets` / `DeleteDidsSecrets` / `DeleteVtcSecrets` (KV v2 metadata delete)
for teardown ([§13](#13-teardown)).

- **No new ServiceAccount** for any component — every Job/Deployment that touches Vault
  runs as the same `vta` SA in the same per-user namespace.
- **No new kubernetes-auth role** — the existing per-user role (`vault.UserName(userID)`,
  bound to SA `vta` + the user's namespace) is reused as-is; only the policy widens.
- **No K8s Secret, no token minting/injection anywhere in the mode.**

No renewal task on the API side, no `VAULT_TOKEN`/`VAULT_SKIP_VERIFY` env vars on any Job
or Deployment. Every binary re-authenticates with its own pod's ServiceAccount JWT on every
restart — kubelet-rotated JWTs are handled transparently, the same guarantee the VTA
already had.

> **Which Jobs actually need Vault.** Only the ones that touch a secret store:
> `step_mediator_p1`/`p2`, `deploy_mediator`, `step_dids_p1`/`p2`, `deploy_dids`,
> `step_vtc_setup`, `deploy_vtc`, and every `vta …` Job. Verified against the daemon's own
> source: `load-did` and `invite` (`step_dids_load_did`/`step_dids_invite`) only ever open
> the DIDs/sessions keyspaces directly — never the secret store — so they stay on SA
> `pod-operator`. `step_vtc_setup_key` only mints and writes a local key file, so it does
> too ([§6](#6-per-step-jobs)).

**Reveal-once secrets (2c, 3c, 5d, 5e).** The mediator and webvh admin private keys are
captured from the setup logs and surfaced to the user **once** via `GET /setup/:id` for
offline backup; both are also in Vault by the time they're shown, and teardown deletes both
KV prefixes ([§13](#13-teardown)), so this reveal is the only copy the user keeps. The
VTC's `install_url` + `claim_code` are reveal-once for a different reason — they're
single-use credentials for the admin-claim ceremony ([§14](#14-manual-gates-that-remain)),
not backups.

---

## 10. Data model changes

Extend `SetupSession` (all additive; migrations `000009_full_stack_fields`,
`000010_dids_enroll_used`, `000012_vtc_fields`, `000013_unique_names`,
`000014_vtc_name_default`):

```go
// full_stack's VTA component reuses Subdomain/CFRecordID as-is — same fields
// vta_only already uses — rather than getting its own vta_subdomain/
// cf_record_vta columns. Only mediator/dids/vtc need their own subdomains, and
// they follow Subdomain's own `NOT NULL DEFAULT ''` convention:
MediatorSubdomain string
DidsSubdomain     string
VtcSubdomain      string

// Cloudflare record ids for mediator/dids/vtc — nullable, matching CFRecordID's
// own convention (the one column in the original schema that's genuinely
// nullable rather than NOT NULL DEFAULT '').
CFRecordMediator *string
CFRecordDids     *string
CFRecordVtc      *string

// Per-component images (VtaImage exists today). Empty ('') until the session
// picks one — all three are required request fields, not env defaults, same
// convention as VtaImage.
MediatorImage string
DidsImage     string
VtcImage      string

// VtcName is the VTC's user-chosen name: it drives the vtc subdomain, the VTC's
// did:webvh path (<vtc_name>-vtc), and doubles as the VTA context id the
// community lives under (§6/§7). Unique across all sessions (000013).
VtcName string

// Collected outputs (VtaDid, AdminDid exist today). AdminDid holds the
// user-supplied PNM admin DID (4a) — reused from vta_only as-is, no parsing.
// Empty ('') until the corresponding setup step completes:
MediatorDid        string  // 1b  (NB: this column holds the *shared* mediator DID for vta_only)
MediatorAdminDid   string  // 2b  → json: mediator_admin_did
DIDHostingAdminDid string  // 3b  → json: did_hosting_admin_did
DIDHostingDid      string  // 3d  → json: did_hosting_did
VtcSetupKeyDid     string  // 5a  ephemeral did:key — audit/debug only, never read back
VtcDid             string  // 5b  the VTC's own did:webvh
VtcAdminDid        string  // 5c  the VTC's pre-claim install admin — NOT the PNM AdminDid

// Reveal-once values, returned to the user once; stored plaintext in the DB.
MediatorAdminKey string  // 2c
WebvhAdminKey    string  // 3c
DidsEnrollURL    string  // 3e  dids admin-enroll URL — REQUIRED output, single-use (regenerable)
VtcInstallURL    string  // 5d  install URL — 15-minute TTL, regenerable (§12)
VtcClaimCode     string  // 5e  claim code — delivered over a logically separate channel

// DidsEnrollUsed / VtcInstallUsed are set by the frontend once the user opens the
// corresponding link, so GET /setup/:id stops re-offering a credential that is
// single-use at the component level and will fail if clicked again. Purely a UI
// affordance — both components enforce single-use themselves.
DidsEnrollUsed bool
VtcInstallUsed bool
```

```go
func (s *SetupSession) MediatorFQDN() string { return s.MediatorSubdomain + "." + s.Domain }
func (s *SetupSession) DidsFQDN() string     { return s.DidsSubdomain + "." + s.Domain }
func (s *SetupSession) VtcFQDN() string      { return s.VtcSubdomain + "." + s.Domain }
```

Every column matches its closest analog in the original schema precisely: `TEXT NOT NULL
DEFAULT ''` for output/optional TEXT columns (same as `vta_image`/`vta_did`/`admin_did`),
`VARCHAR(100) NOT NULL DEFAULT ''` for the subdomains (same family as `subdomain`), and
nullable with no default only for the `cf_record_*` columns (matching `cf_record_id`, the
sole nullable column in the original schema).

**Transient values are not columns.** The bundle digests (`2a`, `3a`), the `*.armor`
bundles, and the `bootstrap-request.json` files are consumed by the very next step and
never read again, so they are *not* persisted — digests live as local variables in the
orchestrator goroutine, and the bundles/requests live on the producing component's PVC
([§4](#4-cross-component-file-handoffs)). `5a` is the one borderline case: it's persisted
for audit even though `step_vtc_acl_grant` reads it straight from the producing Job's logs
in the same orchestrator run.

`FQDN()`/`PublicURL()` stay for the VTA. All `full_stack` columns are nullable/defaulted so
`vta_only` rows are unaffected.

---

## 11. Config / env additions

| Var | Purpose |
| --- | --- |
| `GITHUB_MEDIATOR_PACKAGE_NAME` | GHCR package for `GET /setup/images?component=mediator` (default `mediator`) |
| `GITHUB_DID_HOSTING_DAEMON_PACKAGE_NAME` | GHCR package for `GET /setup/images?component=dids` (default `did-hosting-daemon`) |
| `GITHUB_VTC_PACKAGE_NAME` | GHCR package for `GET /setup/images?component=vtc` (default `vtc`) |

All three reuse the same `GITHUB_PACKAGE_OWNER`/`GITHUB_TOKEN` as the VTA's GHCR listing.
`mediator_image` / `dids_image` / `vtc_image` are **required** request fields on `POST
/setup` (same as `vta_image`), selected from
`GET /setup/images?component=vta|mediator|dids|vtc`. Each image must be built with its
respective Vault feature flag (`secrets-vault` for the mediator, `vault-secrets` for the
dids daemon and the VTC).

Reuse existing: `CLUSTER_INGRESS_IP`, `CLUSTER_DOMAIN`, `CLOUDFLARE_*`, and all `VAULT_*`.
Every component's secrets are Vault-backed via kubernetes auth under the same per-user
policy/role — no dedicated token-role config needed. The mediator's `vault://` host is
derived from `VAULT_VTA_ADDR` (in-cluster `vault.vault.svc:8200`); the KV prefixes are
`mediator/user-<id>/session-<id>`, `dids/user-<id>/session-<id>`, and
`vtc/user-<id>/session-<id>`, all under `VAULT_KV_MOUNT`. `MEDIATOR_DID` and
`DID_HOSTING_*` remain **only** for `vta_only`; `full_stack` ignores them (it grows its
own mediator + dids).

**No ClusterRole changes for any of this.** The existing rule set (namespaces, SAs, pods,
pods/log, configmaps, PVCs, services, roles, rolebindings, jobs, deployments, ingresses)
covers all four components — **no `secrets` verb needed**. An earlier design added one for
the mediator's per-session `VAULT_TOKEN` Secret; kubernetes auth has no such Secret to
create, so that grant was removed.

---

## 12. API surface

`mode = "full_stack"` on `POST /api/v1/setup` selects this path. It is gated on
`users.beta_access` (see `CLAUDE.md` § Beta Access) — an admin-only switch the user can't
flip themselves.

| Method | Path | Change vs `vta_only` |
| --- | --- | --- |
| `POST` | `/setup/validate` | assert all four hosts are creatable |
| `POST` | `/setup` | accept `mode=full_stack`; require `mediator_image`, `dids_image`, `vtc_image`; optional `vtc_name` (default `personal-vtc`, **unique across all sessions** — 409 if taken); optional `admin_did` (user's local PNM admin DID — when present, the machine runs straight through the gate); create 4 DNS records; start the §5 machine |
| `POST` | `/setup/:id/admin` | **reused from `vta_only`** — supply the PNM `admin_did` once the stack is up (`awaiting_admin_did`); resumes the machine at `step_import_admin_did` |
| `GET` | `/setup/:id` | four URLs + **VTA DID (1a)** + **dids admin-enroll URL (3e)** + **VTC DID (5b)** + `dids_enroll_used` / `vtc_install_used` + per-step status + (once) the admin keys and the VTC install credentials |
| `GET` | `/setup/:id/logs` | `?source=` gains `mediator_p1\|mediator_p2\|dids_p1\|dids_p2\|dids_invite\|dids_load_did\|vta_register_dids\|import_admin_did\|vtc_setup_key\|vtc_acl_grant\|vtc_setup\|mediator\|dids\|vtc` |
| `DELETE` | `/setup/:id` | tear down all 4 DNS records + all four components' resources ([§13](#13-teardown)) |
| `POST` | `/setup/:id/dids/reissue-enroll` | regenerate the single-use dids admin enrollment URL (`did-hosting-daemon invite`); scales the dids Deployment to 0, waits for its pod gone, runs the invite Job, then scales back to 1 (always, even on failure) |
| `POST` | `/setup/:id/dids/enroll-ack` | frontend marks `dids_enroll_used = true` once the user opens the enrollment URL |
| `POST` | `/setup/:id/vtc/reissue-install` | **required, not a nice-to-have** — the setup-minted install token lives 15 minutes ([§6](#6-per-step-jobs)). Remints a fresh install URL **and claim code** via `vtc admin invite --did <5c>`, mirroring `ReissueDidsEnroll`: scale `fs-{sid}-vtc` to 0, wait pod gone, run the Job, scale back to 1 (via `defer`, so the VTC restarts even on failure). Parses the `Install URL (one-shot):` and `Claim code …:` lines (stderr; K8s captures both streams), updates `vtc_install_url` + `vtc_claim_code`, resets `vtc_install_used` |
| `POST` | `/setup/:id/vtc/install-ack` | frontend marks `vtc_install_used = true` once the user opens `install_url` — mirrors `AckDidsEnroll` |

`GET /setup/:id` response sketch:

```jsonc
{
  "id": "ab12cd34",
  "mode": "full_stack",
  "status": "running",
  "urls": {
    "vta":      "https://vta-myvta.example.com",
    "mediator": "https://mediator-myvta.example.com",
    "dids":     "https://dids-myvta.example.com",
    "vtc":      "https://vtc-myvtc.example.com"
  },
  "collected": {
    "vta_did":               "did:webvh:…:dids-myvta.example.com:myvta-vta",     // 1a — REQUIRED: user feeds it to `pnm setup continue --vta-did`
    "mediator_did":          "did:webvh:…:dids-myvta.example.com:myvta-mediator",
    "did_hosting_did":       "did:webvh:…:dids-myvta.example.com",
    "mediator_admin_did":    "did:key:z6Mk…",
    "did_hosting_admin_did": "did:key:z6Mk…",
    "vtc_did":               "did:webvh:…:dids-myvta.example.com:myvtc-vtc"      // 5b
  },
  "action_required": {
    "dids_admin_enroll_url": "https://dids-myvta.example.com/enroll/…",          // 3e — single-use, visit to set a passkey
    "install_url":           "https://vtc-myvtc.example.com/admin/install?token=…", // 5d — 15-min TTL, reissue endpoint is the normal path
    "claim_code":            "ABCD-1234",                                        // 5e
    "reveal_keys_once":      true                                                // 2c / 3c shown once
  },
  "dids_enroll_used": false,
  "vtc_install_used": false
}
```

**PNM handshake (user-local, mirrors `vta_only`).** `full_stack` does *not* run `pnm setup`
/ `pnm setup continue`. The user (1) runs `pnm setup --name <name>` locally to mint their
admin DID, (2) hands it to the API as `admin_did` (on `POST /setup` or `POST
/setup/:id/admin`) so the API can `vta import-did` it before booting the VTA, then (3) runs
`pnm setup continue <name> --vta-did <1a>` locally with the returned VTA DID. The API owns
only step (2).

**Every new route must be added to `internal/apidocs/openapi.yaml`** with the `User` tag
(per the API Docs Rule in CLAUDE.md).

---

## 13. Teardown

`DELETE /setup/:id` for `full_stack`, in order:

1. `orch.Cancel(sid)` — stop the goroutine.
2. Delete 4 Cloudflare records (`CFRecordID` / `CFRecordMediator` / `CFRecordDids` /
   `CFRecordVtc`).
3. Delete component resources: Deployments, Services, Ingresses, PVCs for
   vta/mediator/dids/vtc (`DeleteComponentResources` per component), and all setup
   Jobs/ConfigMaps (`DeleteAllComponentJobs` over the session's job-name list).
4. Delete Vault material: `TeardownVaultSeed` (VTA master seed), `TeardownMediatorVault`,
   `TeardownDidsVault`, `TeardownVtcVault` (each deleting its KV prefix, best-effort). No
   token to revoke anywhere — kubernetes auth leaves nothing to clean up beyond the KV data
   itself.
5. Delete the `SetupSession` row.
6. If this was the user's last session, `DeleteNamespace` + `TeardownVaultUserAccess`
   (remove the per-user Vault policy/role).

No external DID-host cleanup is needed (unlike `vta_only`) — all four pods are namespaced
and die with everything else. No de-registration of the `dids` server entry from the VTA's
registry either: the VTA is being deleted in the same teardown, so nothing is left holding
a stale registration.

Admins can trigger the identical teardown for any user's session via
`DELETE /api/v1/admin/setup-sessions/:id`.

---

## 14. Manual gates that remain

The chain is fully automatable **except** these user-local touchpoints:

1. **PNM binding (`4a`)** — the only one that *blocks* the pipeline. Mirrors `vta_only`:
   the user runs `pnm setup --name <name>` and `pnm setup continue <name> --vta-did <1a>` on
   their own machine. They supply the resulting admin DID to the API as `admin_did`, and
   the API runs only `vta import-did` before `deploy_vta`. If `admin_did` is not supplied at
   `POST /setup`, the machine pauses at `awaiting_admin_did` until the user POSTs it to
   `/setup/:id/admin`.
2. **DIDS admin-panel passkey enrollment (`3e`).** The offline DID load
   ([§6](#6-per-step-jobs) `step_dids_load_did`) removes the *functional* need to log in,
   but the user still gets the single-use enrollment URL to register a passkey for the dids
   admin UI. Surfaced under `action_required.dids_admin_enroll_url` until the frontend posts
   `/setup/:id/dids/enroll-ack`; regenerable via the reissue endpoint.
3. **VTC WebAuthn admin-claim ceremony (`5d` + `5e`).** `install_url` and `claim_code` are
   delivered over conceptually separate channels — the VTC refuses a claim without both.
   Surfaced once the session reaches `running`; doesn't block deployment; single-use. Since
   the setup-minted token expires after 15 minutes, `POST /setup/:id/vtc/reissue-install` is
   the **expected** way users actually claim, not an edge case.
4. **Reveal-once secrets (`2c`, `3c`).** The mediator + webvh admin private keys are shown
   to the user once for offline backup.

Only (1) gates anything from starting; (2)–(4) are post-`running` conveniences. There is
**no** ACL-grant gate anywhere in the mode — the VTC's ephemeral key is minted *and*
granted by the API itself ([§5](#5-state-machine),
[Appendix B](#appendix-b--vtc-design-rationale)).

---

## 15. Implementation checklist

1. Migrations: `000009_full_stack_fields`, `000010_dids_enroll_used`, `000012_vtc_fields`,
   `000013_unique_names` (unique `vtc_name`), `000014_vtc_name_default` — all additive
   ([§10](#10-data-model-changes)).
2. **Image pipeline (external):** publish the `vtc-service` image built with
   `--features vault-secrets` (non-default — [§9](#9-secrets-handling)) under
   `GITHUB_VTC_PACKAGE_NAME`. The mediator (`secrets-vault`) and dids daemon
   (`vault-secrets`) images have the same requirement.
3. `model.SetupSession` — all `full_stack` fields + `MediatorFQDN()` / `DidsFQDN()` /
   `VtcFQDN()`.
4. `internal/setup/subdomain.go` — `FullStackHosts` (four hosts), `ValidateName`, and the
   `VtaDidPath` / `MediatorDidPath` / `VtcDidPath` helpers ([§3](#3-urls--dns)).
5. `internal/k8s/fullstack_names.go` — `FSVtaName` / `FSMediatorName` / `FSDidsName` /
   `FSVtcName`, the `FSJob*` helpers (incl. `FSJobVtaRegisterDids`, `FSJobVtcSetupKey`,
   `FSJobVtcAclGrant`, `FSJobVtcSetup`, `FSJobVtcInvite`), and `allFSJobNames` for
   teardown.
6. `internal/setup/templates_fullstack.go` + `templates_vtc.go` — the
   `create_mediator` VTA variant, the mediator recipe (fjall message store, Vault
   `vault://` secrets, kubernetes auth), the webvh p1/p3 recipes, and `RenderVtcSetupTOML`
   ([§7](#7-recipe-templates)).
7. `internal/setup/parser_fullstack.go` + `parser_vtc.go` — the regexes from
   [§8](#8-output-parsing-regex), including `ParseVtcSetupKeyDid`, the five-field
   `ParseVtcSetupOutput`, and the reissue parser for `vtc admin invite`. (`4a` is a user
   input, not parsed.)
8. `internal/k8s/component_jobs.go` + `component_resources.go` — the generic
   `ComponentJobSpec`/`CreateComponentJob` and `ComponentDeploymentSpec` helpers (image,
   command, one-or-two PVC mounts, workingDir, SA, recipe ConfigMap) plus
   Deployment/Service/Ingress creation and `WaitForComponentDeploymentReady`. No K8s Secret
   anywhere, no ClusterRole change.
9. `internal/vault/client.go` — `MediatorPrefix` / `DidsPrefix` / `VtcPrefix`,
   `DeleteMediatorSecrets` / `DeleteDidsSecrets` / `DeleteVtcSecrets`, and the widened
   `EnsureUserAccess` policy ([§9](#9-secrets-handling)).
10. `internal/setup/orchestrator_fullstack.go` — `runFullStack`: the pre-gate pipeline
    through `step_vta_register_dids`, the shared `fsStepImportAdminDid`/`fsDeployVta`
    helpers, the `Teardown*Vault` wrappers, and `Resume` for every `full_stack` status.
11. `internal/setup/orchestrator_vtc.go` — `runFullStackFinish`: the
    post-gate finish `step_import_admin_did` → `step_vtc_setup_key` → `step_vtc_acl_grant`
    → `deploy_vta` → `step_vtc_setup` → `deploy_vtc` → `running`, plus `TeardownVtcVault`
    and the context-grant Conflict fallback ([§6](#6-per-step-jobs)).
12. `internal/handler/setup.go` + `setup_fullstack.go` + `setup_vtc.go` — mode
    dispatch; 4 DNS records; required `mediator_image`/`dids_image`/`vtc_image`; unique
    `vtc_name`; the reveal-once fields and required `vta_did` / dids-enroll-URL / vtc DID in
    `GET /setup/:id`; the dids reissue/ack and vtc reissue/ack endpoints; teardown
    ([§13](#13-teardown)).
13. `internal/capacity/capacity.go` — the four-component `FullStack` planning mode, kept in
    sync with the actual per-component requests/limits and PVC sizes
    ([§2](#2-component-topology-one-user-namespace)).
14. `internal/config/config.go` + `.env.example` — [§11](#11-config--env-additions) vars.
15. `internal/apidocs/openapi.yaml` — document every route with the `User` tag.

**Post-implementation fixes worth remembering:**

- *`vta_only` parity* — `step_vta_register_dids` (the `--id dids` server registration) was
  added after the initial implementation. A paired `step_dids_grant_vta` (daemon-side
  `service` ACL for the VTA) was added and then removed in the same pass; see
  [§4a](#4a-registering-the-sessions-own-dids-daemon-with-the-vta) for why.
- *Mediator/dids Vault simplification* — both switched from token-auth/plaintext to
  kubernetes auth once the upstream binaries gained parity with the VTA. Removed
  `MintMediatorToken`/`RevokeToken`/`MediatorTokenRole`, `FSMediatorTokenSecret`, the
  generic `CreateComponentSecret`/`GetComponentSecretValue`/`DeleteComponentSecret` K8s
  helpers, the ClusterRole `secrets` grant, and the `VAULT_MEDIATOR_TOKEN_ROLE`
  config/helm values.
- *Live-session bug fix* — `fsDeployDids` / `fsDeployMediator` / `deploy_vta` now call
  `WaitForComponentDeploymentReady` (2 min timeout) before returning. Found via a real
  session where `step_vta_register_dids` 503'd resolving the dids daemon's `did.jsonl`
  because it ran immediately after `deploy_dids`, before the pod (and its Service/Ingress
  endpoint) was actually serving.
- *Mode consolidation* — `full_stack` and `full_stack_with_vtc` were once two modes. The
  VTC-less variant is retired and the `full_stack_with_vtc` identifier is gone; `full_stack`
  now always means all four components.

---

## Appendix A — Verified Ubuntu reference flow

> **Superseded on secrets backends.** This appendix documents the single-host flow as
> originally verified: mediator secrets via Vault **token auth**, dids daemon on
> **plaintext**. Both upstream binaries have since gained kubernetes-auth /
> `vault-secrets` support at parity with the VTA, and the K8s mapping above
> ([§9](#9-secrets-handling)) has moved to it for both — no token to mint, no plaintext
> fallback. Command ordering, recipe shapes, and everything else below is otherwise still
> accurate and is kept as the source of truth for those.
>
> **Scope.** The reference flow stops at PNM binding (Step 4) — it has **no VTC step**, so
> everything VTC in this design is K8s-mapping-only, sourced from `vtc-service`'s own docs
> and code instead ([Appendix B](#appendix-b--vtc-design-rationale),
> [Appendix C](#appendix-c--verified-against-source)).

The bare-metal flow this design automates (the "Automated VTI Setup" guide), verified
end-to-end on Ubuntu Server with `fjall` mediator message storage. Kept as the source of
truth for command ordering and recipe contents; the K8s mapping above supersedes the
home-dir / `nohup` mechanics, and now the secrets-backend choice too.

**Saved-value cross-reference (reference ID → where this design captures it):**

| ID | Value | Captured at |
| --- | --- | --- |
| 1a | VTA DID | `step_vta_setup` |
| 1b | Mediator DID | `step_vta_setup` |
| 2a | Mediator bundle digest | `step_mediator_reprov` |
| 2b | Mediator admin DID | `step_mediator_reprov` |
| 2c | Mediator admin private key | `step_mediator_p2` (returned once) |
| 3a | WebVH bundle digest | `step_dids_provision` |
| 3b | WebVH admin DID | `step_dids_p2` |
| 3c | WebVH admin private key | `step_dids_p2` (returned once) |
| 3d | DID Hosting daemon DID | `step_dids_p2` |
| 3e | DIDS admin enrollment URL | `step_dids_invite` (required) before `deploy_dids` |
| 4a | PNM admin DID | **user input** (local `pnm setup`) → `step_import_admin_did` |
| 5a–5e | VTC setup key / DID / admin DID / install URL / claim code | no reference equivalent — `step_vtc_setup_key` + `step_vtc_setup` |

**Reference points preserved in the K8s mapping:**

- **Mediator message storage is self-contained** (`[storage].backend = "fjall"`,
  file-backed on its PVC) — no Redis/Valkey is deployed. The `[database].url` field stays
  in the recipe as a required-but-unused value. **Mediator *secrets* live in Vault**
  (`[secrets].storage = "vault://…"`, mediator built with the `secrets-vault` feature);
  Vault does not replace the fjall message store. → §2, §7, §9.
- **Start order = dids → mediator → vta**, after all setup/config steps (the VTC then
  follows the VTA). → §5.
- **PNM binding is user-local** — the user runs `pnm setup` / `pnm setup continue`
  themselves; the API only runs `vta import-did` with the supplied admin DID, before the
  VTA starts (exactly as `vta_only`). → §5, §6, §14.
- **DID logs reach the dids daemon** (reference: manual browser upload to a *running*
  daemon, via the admin UI). The K8s mapping does this **offline instead** — §6
  `step_dids_load_did` loads both DID logs into the local store *before* the daemon ever
  starts, which is what lets the daemon (and the mediator) resolve their own DIDs
  successfully on first boot, rather than requiring a running daemon to upload to.
- **The DIDS enrollment URL is single-use** — regenerate by stopping the daemon and
  re-running `did-hosting-daemon invite`. → §14, reissue endpoint.

**Mediator Vault backend — reference setup (single-host):**

- **Build** with the feature: `cargo build --release -p affinidi-messaging-mediator
  --no-default-features --features "didcomm,redis-backend,fjall-backend,secrets-vault"`.
  `mediator-setup` already bundles `secrets-vault`.
- **Vault prep:** a KV v2 mount (e.g. `vault secrets enable -path=secret kv-v2`) and a
  `VAULT_TOKEN` valid for **both** `mediator-setup` (phase 1 & 2 writes) and the mediator
  runtime (startup reads).
- **Recipe:** `[secrets].storage = "vault://<host[:port]>/<kv-mount>/<prefix>"`, e.g.
  `vault://vault.yourdomain.com:8200/secret/mediator`. Secrets land at
  `secret/mediator/{mediator_jwt_secret,mediator_operating_secrets,mediator_admin_credential,mediator_vta_last_known_bundle}`.
- **`export VAULT_TOKEN=…`** before phase 1, phase 2, and before starting the mediator
  (replaces the old `MEDIATOR_FILE_BACKEND_PASSPHRASE`). No `conf/secrets.json` is
  produced; the success line reads `Provisioning unified secret backend: vault://…`.
- **Policy** (the probe writes/reads/deletes a sentinel, so read alone is not enough):

  ```hcl
  path "secret/data/mediator/*"     { capabilities = ["create","update","read","delete"] }
  path "secret/metadata/mediator/*" { capabilities = ["read","list","delete"] }
  path "secret/metadata/mediator"   { capabilities = ["read","list"] }
  ```

- **Unchanged:** VTA + dids backends, PNM, and `[database]`/`[storage]` — Vault stores
  only mediator secrets, not the fjall message store.

The K8s mapping (§9) keeps the per-tenant `mediator/user-<id>/session-<id>` prefix but
drops the static `VAULT_TOKEN` entirely: the mediator now authenticates via
`?auth=kubernetes&role=<role>` in the recipe's `storage` URL, exchanging its own pod's
ServiceAccount JWT — no token to export, mint, or inject, and `?insecure=1` in the URL
takes the place of the `VAULT_SKIP_VERIFY` env var.

> The full step-by-step reference guide — including the recipe edits, the
> `vta contexts reprovision` / `vta bootstrap provision-integration` transcripts, the
> PNM bind transcript, and the health checks — is the upstream source for this appendix.
> This document captures everything from it that the automated K8s flow depends on.

---

## Appendix B — VTC design rationale

Why the VTC is shaped the way it is, and why it costs so little to add to the pipeline.

### B1. Why a VTC needs a mediator and DID-hosting

A VTC is a community layer on top of a VTA — it never mints keys itself, it receives a
sealed key bundle from the VTA at setup (`docs/03-vtc/getting-started.md`,
`vtc-service/README.md` in the `verifiable-trust-infrastructure` repo). Deployed against a
mediator-less, DID-hosting-less VTA it would technically start, but:

- **No mediator → no DIDComm.** The VTC's join-request/credential-issuance surfaces are
  REST-first, but a VTC with no `[messaging]` configured can't participate in any
  DIDComm-based flows (join requests submitted over DIDComm, cross-community recognition,
  etc.) — it just logs a one-line "messaging not configured" warning at startup and stays
  REST-only.
- **No DID-hosting integration → the VTC has to self-host its own `did:webvh` log**,
  duplicating hosting capability the stack already has running two components away.

`full_stack` already stands up exactly the mediator + dids daemon a VTC needs. Wiring the
VTC to *those same instances* — rather than requiring the user to separately operate (or
vtafarm-api to separately provision) a second mediator and a second dids daemon just for
the VTC — is the only sensible shape. That's the whole argument for the VTC being
mandatory rather than a variant: if you have the first three, the fourth is nearly free and
the stack is strictly more useful with it.

### B2. Why there's no standalone `vtc_only` mode

An earlier draft explored "attach a bare VTC to an arbitrary/already-running VTA." That
design needed a manual `awaiting_grant` gate (the operator must grant the ephemeral setup
key an admin ACL on a VTA the farm may not control) or — for a farm-hosted target — a
scale-to-0 / Job / scale-back-to-1 dance to work around the target VTA's fjall lock, the
same constraint `ReissueDidsEnroll` already works around for the dids daemon.

`full_stack` needs **neither**, because it never attaches to something already running.
Every dependency the VTC needs — a VTA, a context with its ACL granted, a registered dids
server the VTA is authorized to publish to, a reachable mediator — is minted in the *same*
pipeline run, in the right order, before the VTC ever asks for any of it. The only
genuinely-live, network-based step in the whole mode is `step_vtc_setup` itself, and by
construction everything it depends on is already up by the time it runs.

### B3. Why the ephemeral setup key needs two steps and no human

`vtc setup --from` authenticates to the VTA with an ephemeral `did:key` that must
*already* hold an admin ACL entry there. The interactive wizard mints that key inline and
then blocks, printing a `pnm contexts create …` command for a human to run. A K8s Job can't
block on a TTY.

So the pipeline splits the wizard's single interactive step into the two offline Jobs
described in [§4b](#4b-the-vtcs-own-ephemeral-setup-key-handshake) —
`step_vtc_setup_key` (mint, persist to the VTC PVC, print the DID) and
`step_vtc_acl_grant` (run the grant the wizard would have asked a human for, as
`vta contexts create` against the VTA's offline store). The orchestrator carries the
`did:key` between them as a parsed string. Nothing waits on a person, and the key is dead
weight the moment `step_vtc_setup` finishes.

Both land in the **post-gate VTA-store window** (after `step_import_admin_did`, before
`deploy_vta`) rather than pre-gate, which keeps the grant's `--admin-expires 1h` window at
minutes regardless of how long a human takes at the `awaiting_admin_did` gate.

---

## Appendix C — Verified against source

Claims about the VTC that were checked directly against the upstream codebases
(`verifiable-trust-infrastructure` for vtc/vta, `webvh-build-pipeline` for the dids
daemon), rather than taken from docs:

| Claim | Evidence |
| --- | --- |
| `vtc setup --from <toml>` exists and is fully non-interactive | `vtc-service/src/setup/from_toml.rs` — parses `VtcWizardInputs`, feeds the same `apply()` as the wizard; terse `key=value` summary incl. `install_url`/`claim_code` |
| Standalone setup-key generator exists (§4b) | `vtc-service/src/main.rs::Commands::Setup{setup_key_out, context, ..}` + `vtc-service/src/setup/phase1.rs::run_setup_phase1`; prints via shared `vta_sdk::provision_client::driver::run_phase1_init`, same as mediator/did-hosting. `CreateDidKey` remains a non-substitute — writes the VTC store/credential, not the `setup_key_file` JSON |
| Setup-key JSON shape | `vta-sdk/src/provision_client/setup_key.rs::PersistedKey` — `{version: 1, did, private_key_multibase, note}`, 0600 |
| `[secrets]` field names + defaults + `deny_unknown_fields` | `vtc-service/src/config.rs::SecretsConfig` (mirrors `vti_secrets`; kv_mount `secret`, secret_key `seed`, auth `kubernetes`) |
| `vault-secrets` is a non-default vtc feature | `vtc-service/Cargo.toml` + `docs/03-vtc/feature-flags.md` |
| `[messaging]` fields (`mediator_did` required, `mediator_url` optional) | `vti-common/src/config.rs::MessagingConfig` |
| `[messaging].transports` required, `["tsp","didcomm"]`, not persisted | `vtc-service/src/setup/from_toml.rs::MessagingSetup` + `wizard.rs::Transport` |
| `[webvh].server_id` → `WEBVH_SERVER` template var | `vtc-service/src/setup/wizard.rs` (`WebvhTarget`, `build_template_vars`) |
| `vta contexts create` offline, `--admin-did`/`--admin-expires N[s\|m\|h\|d\|w]`, atomic ACL; Conflict on existing id | `vta-service/src/main.rs::ContextCommands::Create`; `operations/contexts.rs` (`Conflict: context already exists`) |
| `vta import-did --role admin --context <ctx>` as the exists-tolerant regrant | `vta-service/src/main.rs::ImportDid` (`--context Vec<String>`) |
| `servers add` resolves the DID at add time (placement constraint, §4a) | `vta-service/src/operations/did_webvh/servers.rs::add_webvh_server` → `validate_server_did` (live resolve + `WebVHHosting`/`DIDCommMessaging` service required; Conflict on duplicate id) |
| Hosted publish = live authed call as `vta_did` | `operations/did_webvh/mod.rs::authenticated_server_transport` + `transport.publish_did` in `create_did_webvh`; provision-integration selects it via `WEBVH_SERVER` (`operations/provision_integration/{mod,webvh}.rs`) |
| Daemon ACL is deny-by-default, but the offline-complete finalizer auto-seeds an idempotent Admin-role entry for the provisioning VTA — no separate grant step needed (§4a) | `did-hosting-common/src/server/acl.rs::check_acl` (`Forbidden: DID not in ACL`); upstream commit "a VTA-provisioned daemon trusts its provisioning VTA to publish" (`24ad22d`, `acl::seed_provisioning_vta_acl`, idempotent, gated on `config.vta.did.is_some()`); **confirmed live** — a real session's `step_dids_p2` log printed `provisioning-VTA ACL entry added for did:webvh:...:vta`, and a follow-up `add-acl` attempt for the same DID 409'd with "ACL entry already exists" |
| Daemon offline `add-acl` CLI exists (roles admin/owner/service) but isn't used — the auto-seed above makes it redundant | `did-hosting-daemon/src/main.rs::Command::AddAcl` → `did-hosting-common/src/server/cli_acl.rs` |
| Hosted register resolves a domain; first boot seeds `public_url` host as default domain | `did-hosting-control/src/routes/did_manage.rs::register_did` → `resolve_request_domain`; `did-hosting-common/src/server/domain/seed.rs` (tier-2 legacy `public_url` seed, sets default) — so no explicit `domain` needed in §7's `[webvh]` |
| Install token TTL 15 min; `vtc admin invite` remints URL + claim code on a stopped daemon | `vtc-service/src/install/token.rs::INSTALL_TOKEN_DEFAULT_TTL_SECS`; `vtc-service/src/main.rs::run_invite_cli` |

One assumption **not** yet verified end-to-end: the daemon's own DID document (created by
its recipe setup) advertising a `WebVHHosting`-family service that `validate_server_did`
accepts, and the full authenticate-and-publish round-trip against it. Both ends are this
workspace's own code and the `vta_only` external-host flow exercises the same wire path
today, but integration coverage should hit `step_vta_register_dids` + `step_vtc_setup`
explicitly.
