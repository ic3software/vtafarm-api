# Full-Stack VTI Setup — Design

Design for the **Full Stack** setup mode: vtafarm-api provisions a complete,
self-contained VTI stack — **VTA + DIDComm Mediator + WebVH DID Hosting daemon** —
inside one user namespace, wired together, and returns three live URLs:

```text
https://fpp-xxxx.{domain}        ← VTA REST + DIDComm
https://mediator-xxxx.{domain}   ← DIDComm Mediator
https://dids-xxxx.{domain}       ← WebVH DID Hosting daemon (admin panel + DID resolution)
```

This is the K8s/API automation of the manual single-host flow verified on Ubuntu
(reproduced in [Appendix A](#appendix-a--verified-ubuntu-reference-flow)). Every
`<binary> setup --from <recipe>` command in that flow becomes a one-off K8s **Job**;
every cross-component file handoff (the `~/vta`, `~/mediator`, `~/dids` home dirs on a
single host) is reproduced by mounting the relevant PVCs into the Job that needs them;
every `nohup <binary> &` becomes a **Deployment**.

**Secret backends.** **All three components — VTA, Mediator, and the DID Hosting
daemon — store their secrets in the farm Vault, all via the same mechanism**:
kubernetes auth. Each pod presents its `vta` ServiceAccount JWT against the same
per-user kubernetes-auth role the VTA already uses; there's no per-session token to
mint or inject, and no plaintext fallback for the daemon. See
[§9](#9-secrets-handling).

The mediator's *message* storage is still **fjall** (file-backed on its PVC) — Vault
only holds the mediator's secrets, not its message store. No Redis/Valkey.

**Scope:** VTA + Mediator + DID Hosting only. The reference flow stops at PNM binding
(Step 4); there is no VTC step, so VTC is out of scope here too.

> Status: **implemented** (`internal/setup/orchestrator_fullstack.go` + the
> `internal/k8s/component_*.go` helpers). This document is the authoritative design
> reference; the §5 state machine matches the code, including
> `step_vta_register_dids` — added after the initial implementation for parity with
> `vta_only`'s external-host registration (§5 explains why there's no matching
> "grant the VTA an ACL entry on its own daemon" step: the daemon does that itself now).

---

## 1. How this differs from the existing flow

The flow you have been running (`vta_only`) and Full Stack differ in **who owns the
mediator and the DID host**:

| | `vta_only` (today) | `full_stack` (this design) |
| --- | --- | --- |
| Mediator | **Shared**, external. VTA points at `MEDIATOR_DID` via `[messaging] kind = "existing"`. | **Per-deployment**, in-cluster. VTA *creates* it via `[messaging] kind = "create_mediator"`, then the mediator is provisioned + run in the same namespace. |
| DID host | **Shared**, external (`DID_HOSTING_SERVER_URL`). API uploads the VTA DID log to it via the control client. | **Per-deployment**, in-cluster. A fresh `did-hosting-daemon` is provisioned + run in the same namespace; the VTA + mediator DID logs are uploaded to *it*. |
| URLs returned | 1 (`fpp-xxxx.{domain}`) | 3 (`fpp-xxxx`/`mediator-xxxx`/`dids-xxxx.{domain}`) |
| Subdomain | random `fpp-xxxx` prefix | three random-prefixed hosts sharing one ID (see [§3](#3-urls--dns)) |
| Secret backend | HashiCorp Vault (`[secrets] backend = "vault"`) | **All three — VTA, mediator, dids — Vault, kubernetes auth** (same SA, same role) |
| Mediator storage | n/a (shared) | secrets in **Vault**; messages in **fjall** (file-backed, on the mediator PVC) — no Redis/Valkey |
| K8s resources / session | 1 PVC + Deployment + Service + Ingress | 3× (PVC + Deployment + Service + Ingress) |

The existing building blocks are **reused unchanged**: per-user namespace +
ServiceAccounts (`EnsureUserEnvironment`), the Cloudflare A-record helpers, the Job
watch/log-capture helpers (`WaitForJob`, `JobLogs`, `StreamJobLogs`), and the
`---DID_LOG_START---` stdout-marker trick for pulling files out of a finished Job.
`EnsureUserAccess` (the per-user Vault policy + kubernetes-auth role) is **also reused**
for the VTA — full_stack only extends the same policy to also grant the mediator's and
dids daemon's KV prefixes, and every component authenticates against the same role via
the same `vta` ServiceAccount. No per-session token to mint, no plaintext fallback.

---

## 2. Component topology (one user namespace)

```text
namespace: vtafarm-user-{userID}
┌──────────────────────────────────────────────────────────────────────────────┐
│   services & ingresses (created up front; 503 until pods come up)            │
│   ┌────────────────────┐   ┌────────────────────┐   ┌────────────────────┐   │
│   │ fs-{sid}-vta       │   │ fs-{sid}-mediator  │   │ fs-{sid}-dids      │   │
│   │ :8100              │   │ :7037              │   │ :8534              │   │
│   └──────────┬─────────┘   └──────────┬─────────┘   └──────────┬─────────┘   │
│              │ Ingress                │ Ingress                │ Ingress     │
│              │                        │                        │             │
│   ┌──────────▼─────────┐   ┌──────────▼─────────┐   ┌──────────▼─────────┐   │
│   │ Deployment         │   │ Deployment         │   │ Deployment         │   │
│   │ fs-{sid}-vta       │   │ fs-{sid}-mediator  │   │ fs-{sid}-dids      │   │
│   │ /work/vta          │   │ /work/mediator     │   │ /work/dids         │   │
│   └────────────────────┘   └────────────────────┘   └────────────────────┘   │
│                                                                              │
│   Setup Jobs mount whichever PVC(s) they touch; cross-component steps        │
│   mount two PVCs at once (e.g. /work/vta + /work/mediator). Steps run        │
│   strictly in sequence, so plain RWO PVCs are never mounted concurrently.    │
│   Ingress host is the session's randomized subdomain — fpp-xxxx /            │
│   mediator-xxxx / dids-xxxx.{domain} (see §3) — not the literal              │
│   component name shown above.                                                │
└──────────────────────────────────────────────────────────────────────────────┘
```

Each component's PVC, Service, Ingress, and Deployment all share **one name**:
`fs-{sid}-vta` / `fs-{sid}-mediator` / `fs-{sid}-dids` (`internal/k8s/fullstack_names.go`).

| Component | Port | Public URL | Storage | Notes |
| --- | --- | --- | --- | --- |
| VTA | 8100 | `fpp-xxxx.{domain}` | PVC `fs-{sid}-vta` + **Vault** seed | reuses today's VTA resources + Vault path (mounted at `/work/vta`) |
| Mediator | 7037 | `mediator-xxxx.{domain}` | secrets in **Vault** (`vault://`, kubernetes auth); config + `fjall` message store on PVC `fs-{sid}-mediator` | no Redis/Valkey |
| DID Hosting daemon | 8534 | `dids-xxxx.{domain}` | secrets in **Vault** (kubernetes auth); PVC `fs-{sid}-dids` for everything else | standard (integrated) topology |

Each component is one PVC + one Deployment + one Service + one Ingress. Nothing is
shared between them at runtime — the cross-component file handoffs happen only during
setup ([§4](#4-cross-component-file-handoffs)).

---

## 3. URLs & DNS

`domain` is always `CLUSTER_DOMAIN` (never a request field, mirroring `vta_only`). The
backend derives three random-prefixed hostnames sharing one ID, so multiple full_stack
VTIs can coexist under the same cluster domain (`internal/setup.FullStackHosts`):

```text
fpp-xxxx.{domain}        (vta — back-compat with vta_only's bare "fpp-xxxx" naming)
mediator-xxxx.{domain}
dids-xxxx.{domain}
```

In development (`APP_ENV=development`) each gets a `-local-` infix —
`fpp-local-xxxx`, `mediator-local-xxxx`, `dids-local-xxxx` — matching
`GenerateSubdomain`'s existing dev-vs-prod distinction.

Three Cloudflare A-records are created **before any Job runs**, all pointing at
`CLUSTER_INGRESS_IP`, `proxied=true`:

```text
A   fpp-xxxx.{domain}        →  {CLUSTER_INGRESS_IP}
A   mediator-xxxx.{domain}   →  {CLUSTER_INGRESS_IP}
A   dids-xxxx.{domain}       →  {CLUSTER_INGRESS_IP}
```

DNS must exist first because the rendered recipes embed the final `https://…` URLs
(`public_url`, `webvh_url`, `[identity].public_url`) into the DID documents that get
published. TLS is the cluster-wide wildcard default-ssl-certificate on nginx-ingress
(same as today's VTA Ingress — no per-host `tls:` block needed).

`CreateARecord` already returns a record ID; store all three for teardown.

---

## 4. Cross-component file handoffs

On the single host, the bootstrap chain hands files between home dirs:
`mediator-setup` writes `~/mediator/bootstrap-request.json`, the VTA (running in
`~/vta`) reads it and writes `~/mediator/bundle.armor`, and `mediator-setup` phase 2
reads that back. Same pattern for the DID Hosting daemon via `~/dids/`.

On K8s there is no shared home filesystem, but the mapping stays simple: **mount each
component's PVC at `/work/<component>`, and give every cross-component Job both PVCs it
touches.** Because the setup steps run strictly in sequence (one Job finishes before
the next starts), a plain `ReadWriteOnce` PVC is never mounted by two pods at once — no
`ReadWriteMany`, no separate exchange volume, no stdout-capture plumbing needed.

| Job | PVCs mounted | Reads / writes across components |
| --- | --- | --- |
| `step_mediator_reprov` | `/work/vta` + `/work/mediator` | reads `/work/mediator/bootstrap-request.json`, writes `/work/mediator/bundle.armor` |
| `step_dids_provision` | `/work/vta` + `/work/dids` | reads `/work/dids/bootstrap-request.json`, writes `/work/dids/bundle.armor` |
| `step_dids_load_did` | `/work/vta` + `/work/dids` | reads `/work/vta/data/vta/did-logs/{VTA,mediator}-did.jsonl`, loads both into the dids daemon's local store |

All other setup Jobs mount only their own component's PVC. The recipes use **relative
paths** for on-PVC files (`config.toml`, `data/vta`, `conf/mediator.toml`, …) exactly as
the reference does; with `workingDir = /work/<component>` they resolve identically to the
home-dir layout, so the recipe bodies are essentially verbatim. (The mediator's
`[secrets].storage` is the exception — a `vault://` URL, not a PVC path.)

> The VTA's two DID logs (`VTA-did.jsonl`, `mediator-did.jsonl`) are loaded into the dids
> daemon's local store **offline**, before it ever starts — `step_dids_load_did`
> ([§6](#6-per-step-jobs), `step_dids_load_did`) mounts the vta PVC alongside the dids PVC and
> reads them straight off disk (`did-hosting-daemon load-did`), the same way
> `step_mediator_reprov`/`step_dids_provision` above mount two PVCs at once. No marker
> trick or in-memory round-trip through the orchestrator needed.

---

## 5. State machine

```text
pending
  → dns_provision         create 3 Cloudflare A-records
  → env_provision         EnsureUserEnvironment (ns + SAs + Role/RoleBinding) + EnsureUserAccess (Vault policy/role for VTA + mediator + dids KV prefixes)
  → k8s_provision         PVCs, Services, Ingresses for vta/mediator/dids
  → step_vta_setup        vta setup --from vta-setup.toml          → 1a VTA DID, 1b Mediator DID, DID logs
  → step_mediator_p1      mediator-setup (phase 1)                 → mediator/bootstrap-request.json
  → step_mediator_reprov  vta contexts reprovision                 → mediator/bundle.armor, 2a digest, 2b admin DID
  → step_mediator_p2      mediator-setup --bundle --digest         → 2c admin priv key
  → step_dids_p1          did-hosting-daemon setup (offline-prepare)→ dids/bootstrap-request.json
  → step_dids_provision   vta bootstrap provision-integration      → dids/bundle.armor, 3a digest
  → step_dids_p2          did-hosting-daemon setup (offline-complete)→ 3b admin DID, 3c admin priv key, 3d daemon DID
  → step_dids_invite      did-hosting-daemon invite --role admin   → 3e dids admin-enroll URL (returned to user)
  → step_dids_load_did    did-hosting-daemon load-did (mediator + VTA DID logs, offline, into the dids local store)
  → deploy_dids           Deployment dids-daemon (start it)
  → deploy_mediator       Deployment mediator (start it)
  → step_vta_register_dids vta did-mgmt servers add --id dids --did {{3d}} (daemon live+resolvable; VTA store still free)
  → awaiting_admin_did    ⏸ gate: wait for user's PNM admin DID (skip if admin_did given at POST /setup)
  → step_import_admin_did vta import-did --role admin --label pnm-bootstrap --did {{admin_did}}
  → deploy_vta            Deployment vta (start it)
  → running               return 3 URLs + 1a VTA DID + 3e dids admin-enroll URL
        ↓ (any step)
     failed
```

**Why this order.** Setup/config order is VTA → mediator → dids (each later recipe
consumes a DID or bundle the earlier one produced). But the *start* order is **dids
first, then mediator, then VTA** — exactly as the reference: the dids daemon must already
have the VTA's and mediator's DIDs loaded into its local store *before* it starts serving
(`step_dids_load_did` runs offline, right before `deploy_dids`), so that when the mediator
and VTA boot and resolve their own `did:webvh:...` identities against `dids-xxxx.{domain}`, the
documents are already there — otherwise their first-boot DID resolution 404s. The mediator
must also be reachable before the VTA boots, and the VTA must have the admin DID imported
before it starts. The `vta contexts reprovision`, `vta bootstrap provision-integration`,
and `vta import-did` commands are **CLI operations against the VTA's PVC**, not HTTP
calls, so they run as Jobs without the VTA server running.

**Daemon wiring (`step_vta_register_dids`).** This step gives `full_stack` the same
VTA↔host registry wiring `vta_only` gets from `did-mgmt servers add --id control`:
without it the VTA has no publication target for future DID work (e.g. integrations the
user later provisions with `--var WEBVH_SERVER=dids`, or promoting a DID to
server-managed via `did-mgmt dids register --server dids`). `servers add`
**live-resolves** the daemon's DID and requires a hosting service in its DID document,
so it must run **after `deploy_dids`** — and it writes the VTA's fjall store, so still
before `deploy_vta`. Nothing parsed; exit code 0 is success. (The Ubuntu reference flow
has no equivalent step — its uploads went through the daemon's admin browser UI — so
this is K8s-mapping-only, motivated by `vta_only` parity, not by Appendix A.)

**No separate step grants the VTA an ACL entry on its own daemon** — an earlier draft of
this design added `step_dids_grant_vta` (`did-hosting-daemon add-acl --role service`) for
exactly that, mirroring `vta_only`'s `didHosting.CreateAcl(vtaDid, "service")` against its
external host. It's gone: `did-hosting-daemon`'s offline-complete finalizer
(`step_dids_p2`) already seeds an idempotent **Admin**-role ACL entry for its
provisioning VTA as of upstream commit "a VTA-provisioned daemon trusts its provisioning
VTA to publish" (`affinidi-webvh-service`/`webvh-build-pipeline` `24ad22d`) — a dedicated
grant step just collides with that entry ("ACL entry already exists — delete it first to
change the role") and fails the Job. `Admin` is a strict superset of the `service` role
the removed step requested, so nothing is lost.

**PNM binding is user-local — exactly as `vta_only`.** The orchestrator never runs `pnm
setup` or `pnm setup continue`; those happen on the user's own machine. The only
PNM-related thing the API does is `vta import-did --role admin --did <admin_did>`, where
`admin_did` is the DID the user's local `pnm setup` produced (`4a`). Mirroring `vta_only`,
`admin_did` reaches the API one of two ways: as a field on `POST /setup` (the machine runs
straight through, importing just before `deploy_vta`), or — when omitted — via `POST
/setup/:id/admin` after the rest of the stack is up, which is why the machine parks at
`awaiting_admin_did`. The user then completes the bind locally with `pnm setup continue
<name> --vta-did <1a>`, so the API must hand back the **VTA DID (`1a`)**.

Each `step_*` is a `WaitForJob` + `JobLogs` + parse cycle exactly like today's
`runSetup`. The DB `Status` column carries the state-machine value; `Resume` re-attaches
in-flight steps on restart (extend the existing `Resume` queries to the new statuses).

**Shared steps with `vta_only`.** `dns_provision`, `step_vta_setup`, `awaiting_admin_did`,
`step_import_admin_did`, `deploy_vta`, and the terminal `running` status are the **same
steps, same names** as the VTA-Only machine (`vta-setup-design.md`, Mode A). `full_stack`
only adds the mediator/dids
steps between `step_vta_setup` and the gate, and splits `env_provision` / `k8s_provision` out
of `step_vta_setup` (three components to provision instead of one). In `vta_only` those shared
names map onto the implemented DB statuses `dns_provisioned` / `vta_setup_running` →
`vta_setup_complete` / `provisioning` → `running`.

---

## 6. Per-step Jobs

All Jobs: `RestartPolicy: Never`, `BackoffLimit: 0`, TTL 3600s. Each mounts its
component PVC at `/work/<component>` with `workingDir` set to the same path (so the
recipe's relative paths resolve), plus the recipe as a ConfigMap at `/config`.
Cross-component Jobs mount a second PVC ([§4](#4-cross-component-file-handoffs)).
**ServiceAccount is per-Job, not per-component**: anything that touches the Vault-backed
secret store — `step_mediator_p1`/`p2`, `deploy_mediator`, `step_dids_p1`/`p2`,
`deploy_dids`, plus every VTA-side `vta …` Job — runs as SA `vta` (bound to the
per-user kubernetes-auth role). The two dids Jobs that never touch secrets
(`step_dids_invite`, `step_dids_load_did`) stay on SA `pod-operator`, same as before.
Image per component comes from the request (see [§10](#10-data-model-changes)).

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
(`(?i)mediator:\s*(did:\S+)`), and both DID-log JSONL blobs (kept for the upload step).

### Step `step_mediator_p1` — Mediator Job (workingDir `/work/mediator`, SA `vta`)

No Vault env vars needed — auth is kubernetes auth, baked into the recipe's
`vault://…?auth=kubernetes&role=…` URL (§7); the mediator binary exchanges its own
pod's ServiceAccount JWT for a Vault token, same mechanism as the VTA.

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
Writes that request file + stores the offline-bootstrap seed in Vault (kubernetes
auth, same mechanism as the VTA and mediator). Nothing to parse (the `client_did` line
is informational).

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
the daemon's final secrets to Vault (kubernetes auth).

```sh
did-hosting-daemon setup --from /config/webvh-recipe.toml \
  && echo '---ARTIFACT:server_did---' && grep '^server_did' config.toml
```

Parse: **3b** Admin DID (`Generated admin did:key:\s+(did:\S+)`), **3c** Admin private
key (`Private key \(save now, not re-shown\):\s+(\S+)` → return to user once), **3d**
Daemon DID (the `server_did` value from `config.toml`).

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
did-hosting-daemon load-did --path mediator --did-log /work/vta/data/vta/did-logs/mediator-did.jsonl \
  && did-hosting-daemon load-did --path vta --did-log /work/vta/data/vta/did-logs/VTA-did.jsonl
```

Like `step_dids_invite`, it opens the local store directly, so it must run **before**
`deploy_dids` — while no daemon pod holds the dids PVC. This is what makes both `dids` and
`mediator` able to resolve their own `did:webvh:...` identities successfully on their very
first boot: by the time either process starts, the documents are already in the store.
(An earlier draft of this design tried registering the DID logs over the daemon's control
API *after* `deploy_dids` — that requires the daemon to already be running and reachable,
which is backwards: the mediator/dids themselves need the DIDs resolvable *before* they
start, not after.) Nothing to parse — success is just exit code 0.

### `deploy_dids` — start the daemon Deployment

Deployment with `workingDir = /work/dids`, command `["did-hosting-daemon"]`, SA `vta`
(reads its Vault-backed secrets at every boot), mounting the dids PVC, plus the
Service + Ingress for `dids-xxxx.{domain}` (created in `k8s_provision`, now backed by
a running pod). **Waits for Ready** (`WaitForComponentDeploymentReady`, 2 min timeout)
before the step returns — `step_vta_register_dids` right after `deploy_mediator`
live-resolves this daemon over HTTPS, and a `Deployment` object existing is not the
same as the pod (and its Service/Ingress endpoint) actually serving traffic. *(This
wait was documented from the start but not actually wired up until a live session hit
the resulting 503 race — see checklist item 15.)*

### `deploy_mediator` — start the mediator Deployment

Deployment with `workingDir = /work/mediator`, command `["mediator"]`, SA `vta`,
mounting the mediator PVC (it reads `conf/mediator.toml` and the `fjall` message store
from there). No Vault env vars — the mediator reads its secrets from Vault at startup
and *probes* (write→read→delete a sentinel) using kubernetes auth, re-authenticating
with its pod's own ServiceAccount JWT on every restart. Service + Ingress for
`mediator-xxxx.{domain}`. No Redis/Valkey dependency. **Waits for Ready**, same as
`deploy_dids` — nothing in plain `full_stack` resolves the mediator right after this
step, but `full_stack_with_vtc`'s `step_vtc_setup` does, and the invariant "a `deploy_*`
step doesn't return until its component is actually up" should hold uniformly.

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
this runs **after** `deploy_dids`/`deploy_mediator` — the daemon must already be
serving its own `did.jsonl`. It writes the VTA's fjall store offline, so it still runs
**before** `deploy_vta` (same window as `step_import_admin_did`). Nothing to parse.

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

### `deploy_vta` — start the VTA Deployment

Deployment with `workingDir = /work/vta`, command `["vta"]`, mounting the vta PVC, plus
Service + Ingress for `fpp-xxxx.{domain}` (generalize today's `CreateVtaDeployment` /
`CreateVtaService` / `CreateVtaIngress` to the `/work/vta` mount path). **Waits for
Ready** before the session flips to `running` — otherwise `running` would mean "the
Deployment object exists," not "the three returned URLs actually respond."

---

## 7. Recipe templates

PVCs mount at `/work/<component>` with matching `workingDir`, so the recipes keep the
reference's **relative paths verbatim**. Templated fields in `{{…}}`.

<!-- markdownlint-disable MD033 -->

<details><summary><b>vta-setup.toml</b> (full_stack)</summary>

```toml
config_path = "config.toml"
data_dir    = "data/vta"
vta_name    = "{{ .VtaName }}"
public_url  = "{{ .VtaPublicURL }}"        # https://fpp-xxxx.{domain} — session's VTA FQDN

[services]
rest    = true
didcomm = true

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
url       = "{{ .MediatorURL }}"           # https://mediator-xxxx.{domain}/mediator/v1
webvh_url = "{{ .MediatorWebvhURL }}"      # https://dids-xxxx.{domain}/mediator

[vta_did]
kind               = "create_webvh"
url                = "{{ .VtaDidWebvhURL }}" # https://dids-xxxx.{domain}/vta
portable           = {{ .Portable }}
pre_rotation_count = {{ .PreRotationCount }}
```

</details>

<details><summary><b>mediator-recipe.toml</b></summary>

```toml
[deployment]
type      = "server"
protocols = ["didcomm"]
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
public_url   = "{{ .PublicURL }}"      # https://dids-xxxx.{domain}
mediator_did = "{{ .MediatorDid }}"    # 1b

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

The bundles and bootstrap-request files are handoff **files**, not parsed values — they
live on the producing component's PVC and the consuming Job mounts that PVC
([§4](#4-cross-component-file-handoffs)).

**`4a` (PNM admin DID) is *not* parsed** — it is a user input (the user's local `pnm
setup` output) handed to the API and passed straight to `vta import-did`
([§6](#6-per-step-jobs)).

---

## 9. Secrets handling

**All three components use Vault, all three via kubernetes auth, all three through the
same per-user policy and role.** There is no token-auth path and no plaintext fallback
anywhere in `full_stack` — a deliberate simplification over an earlier design that gave
the mediator a separately-minted `VAULT_TOKEN`. Each pod authenticates as itself:

- **VTA** — `vta` ServiceAccount JWT against the per-user kubernetes-auth role
  `vta-user-<id>`. Master seed at `secret/vta/user-<id>/session-<id>/master-seed`.
  Unchanged from `vta_only`.
- **Mediator** — same SA, same role. Its recipe's `[secrets].storage` is a
  `vault://<host:port>/<kv-mount>/<prefix>?auth=kubernetes&role=<role>` URL (§7); the
  mediator binary reads its pod's own ServiceAccount JWT and exchanges it for a Vault
  token, exactly like the VTA does internally. Secrets land at
  `secret/data/mediator/user-<id>/session-<id>/{mediator_jwt_secret,…}`. Must be
  **built with the `secrets-vault` feature**
  (`--features "didcomm,redis-backend,fjall-backend,secrets-vault"`).
- **DID Hosting daemon** — same SA, same role. Its recipe's `[secrets]` block is now a
  tagged `backend = "vault"` (§7), matching the VTA's own non-interactive schema, with
  secrets at `secret/data/dids/user-<id>/session-<id>/server-secrets`. Must be **built
  with the `vault-secrets` feature**.

Because every component authenticates the same way as the same identity, `EnsureUserAccess`
only has to widen **one** policy, not mint anything:

```hcl
path "secret/data/mediator/user-<id>/session-<id>/*" {
  capabilities = ["create", "update", "read", "delete"]
}
path "secret/metadata/mediator/user-<id>/session-<id>/*" {
  capabilities = ["read", "list", "delete"]
}
path "secret/data/dids/user-<id>/session-<id>/*" {
  capabilities = ["create", "update", "read", "delete"]
}
path "secret/metadata/dids/user-<id>/session-<id>/*" {
  capabilities = ["read", "list", "delete"]
}
```

No token minting, no renewal task on the API side, no per-session K8s Secret, no
`VAULT_TOKEN`/`VAULT_SKIP_VERIFY` env vars on any Job or Deployment. Both the mediator
and the dids binary re-authenticate with their own pod's ServiceAccount JWT on every
restart — kubelet-rotated JWTs are handled transparently, the same guarantee the VTA
already had.

> **Which Jobs actually need this.** Only the ones that touch the secret store:
> `step_mediator_p1`/`p2`, `deploy_mediator`, `step_dids_p1`/`p2`, `deploy_dids`, and
> every `vta …` Job. Verified against the daemon's own source: `load-did` and `invite`
> (`step_dids_load_did`/`step_dids_invite`) only ever open the DIDs/sessions keyspaces
> directly — never the secret store — so they stay on SA `pod-operator` and need no
> Vault access at all (§6).

**Admin private keys (2c, 3c).** Still captured from the setup logs and surfaced to the
user **once** via `GET /setup/:id` for offline backup. Both are also already in Vault
by the time they're shown — teardown deletes both KV prefixes (§13), so this reveal is
the only copy the user gets afterward.

---

## 10. Data model changes

Extend `SetupSession` (additive columns; new migration `000009_full_stack_fields`):

```go
// full_stack's VTA component reuses Subdomain/CFRecordID as-is — same fields
// vta_only already uses — rather than getting its own vta_subdomain/
// cf_record_vta columns. Only mediator/dids need their own subdomains, and
// they follow Subdomain's own `NOT NULL DEFAULT ''` convention:
MediatorSubdomain string
DidsSubdomain     string

// Cloudflare record ids for mediator/dids — nullable, matching CFRecordID's
// own convention (the one column in the original schema that's genuinely
// nullable rather than NOT NULL DEFAULT '').
CFRecordMediator *string
CFRecordDids     *string

// Per-component images (VtaImage exists today). Empty ('') until the
// full_stack session picks one (required request fields, not env defaults) —
// same convention as VtaImage.
MediatorImage string
DidsImage     string

// Collected outputs (VtaDid, AdminDid exist today). AdminDid already holds the
// user-supplied PNM admin DID (4a) in vta_only — full_stack reuses it as-is (no new
// column, no parsing). full_stack adds exactly two more admin DIDs. Empty ('')
// until the corresponding setup step completes, same convention as VtaDid:
MediatorDid        string  // 1b  (NB: today this column holds the *shared* mediator DID for vta_only)
MediatorAdminDid   string  // 2b  → json: mediator_admin_did
DIDHostingAdminDid string  // 3b  → json: did_hosting_admin_did
DIDHostingDid      string  // 3d  → json: did_hosting_did

// Admin private keys, returned to the user once (2c, 3c); stored plaintext in the DB.
MediatorAdminKey string  // 2c
WebvhAdminKey    string  // 3c
DidsEnrollURL    string  // 3e dids admin-enroll URL — REQUIRED output, single-use (regenerable)

// DidsEnrollUsed is set by POST /setup/:id/dids/enroll-ack once the user opens
// DidsEnrollURL — single-use at the daemon level, so this just stops the UI from
// re-offering a link that will fail if clicked again. Added later, in migration
// 000010_dids_enroll_used (not part of the original 000009_full_stack_fields above).
DidsEnrollUsed bool
```

Every new column matches its closest analog in the original schema precisely: `TEXT
NOT NULL DEFAULT ''` for output/optional TEXT columns (same as `vta_image`/`vta_did`/
`admin_did`), `VARCHAR(100) NOT NULL DEFAULT ''` for the subdomains (same family as
`subdomain`), and nullable with no default only for `cf_record_mediator`/`cf_record_dids`
(matching `cf_record_id`, the sole nullable column in the original schema).

**Transient values are not columns.** The bundle digests (`2a`, `3a`), the `*.armor`
bundles, and the `bootstrap-request.json` files are consumed by the very next step and
never read again, so they are *not* persisted — digests live as local variables in the
orchestrator goroutine, and the bundles/requests live on the producing component's PVC
([§4](#4-cross-component-file-handoffs)). Only values the user or a later run still needs
are stored above.

`FQDN()`/`PublicURL()` stay for the VTA; add `MediatorFQDN()`, `DidsFQDN()`. Keep all
new columns nullable/defaulted so existing `vta_only` rows are unaffected.

---

## 11. Config / env additions

| Var | Purpose |
| --- | --- |
| `GITHUB_MEDIATOR_PACKAGE_NAME` | GHCR package for `GET /setup/images?component=mediator` (default `mediator`) |
| `GITHUB_DID_HOSTING_DAEMON_PACKAGE_NAME` | GHCR package for `GET /setup/images?component=dids` (default `did-hosting-daemon`) |

Reuse existing: `CLUSTER_INGRESS_IP`, `CLUSTER_DOMAIN`, `CLOUDFLARE_*`, and all
`VAULT_*`. The VTA seed, the mediator secrets, and the dids daemon secrets are all
Vault-backed via kubernetes auth, all under the same per-user policy/role — no
dedicated token-role config needed. The mediator's `vault://` host is derived from
`VAULT_VTA_ADDR` (in-cluster `vault.vault.svc:8200`); its KV prefix is
`mediator/user-<id>/session-<id>` and the dids daemon's is `dids/user-<id>/session-<id>`,
both under `VAULT_KV_MOUNT`. `MEDIATOR_DID` and `DID_HOSTING_*` remain **only** for
`vta_only`; `full_stack` ignores them (it grows its own mediator + dids, both on Vault).

`GITHUB_MEDIATOR_PACKAGE_NAME`/`GITHUB_DID_HOSTING_DAEMON_PACKAGE_NAME` reuse the same
`GITHUB_PACKAGE_OWNER`/`GITHUB_TOKEN` as the VTA's GHCR listing. `mediator_image`/
`dids_image` are **required** request fields on `POST /setup` (same as `vta_image`),
selected from `GET /setup/images?component=vta|mediator|dids`. Both images must be
built with their respective Vault feature flag (`secrets-vault` for the mediator,
`vault-secrets` for the dids daemon).

The existing ClusterRole covers all of this mode (namespaces, SAs, pods, pods/log,
configmaps, pvcs, services, roles, rolebindings, jobs, deployments, ingresses) per
CLAUDE.md — **no `secrets` verb needed**. An earlier design added one for the
mediator's per-session `VAULT_TOKEN` Secret; kubernetes auth has no such Secret to
create, so that grant was removed.

---

## 12. API surface

Mostly the existing endpoints, generalized for three components. `mode = "full_stack"`
on `POST /api/v1/setup` selects this path.

| Method | Path | Change |
| --- | --- | --- |
| `POST` | `/setup/validate` | also assert all three hosts are creatable |
| `POST` | `/setup` | accept `mode=full_stack`, optional `mediator_image`/`dids_image`, optional `admin_did` (user's local PNM admin DID — when present, auto-runs import + `deploy_vta`); create 3 DNS records; start the §5 machine |
| `POST` | `/setup/:id/admin` | **reused from `vta_only`** — supply the user's PNM `admin_did` once the stack is up (`awaiting_admin_did`); triggers `vta import-did` + `deploy_vta` |
| `GET` | `/setup/:id` | return the three URLs + **VTA DID (1a)** + **dids admin-enroll URL (3e)** + `dids_enroll_used` + per-step status + (once) the admin keys |
| `GET` | `/setup/:id/logs` | `?source=` gains `mediator_p1\|mediator_p2\|dids_p1\|dids_p2\|dids_invite\|dids_load_did\|vta_register_dids\|import_admin_did\|mediator\|dids` |
| `DELETE` | `/setup/:id` | tear down all 3 DNS records + all component resources |
| `POST` | `/setup/:id/dids/reissue-enroll` *(new, optional)* | regenerate the single-use dids admin enrollment URL (`did-hosting-daemon invite`); scales the dids Deployment to 0, waits for its pod gone, runs the invite Job, then scales back to 1 (always, even on failure) |
| `POST` | `/setup/:id/dids/enroll-ack` *(new, optional)* | frontend marks `dids_enroll_used = true` once the user opens the enrollment URL, so `GET /setup/:id` stops re-offering a link the daemon will refuse a second time |

`GET /setup/:id` (full_stack) response sketch:

```jsonc
{
  "id": "ab12cd34",
  "mode": "full_stack",
  "status": "running",
  "urls": {
    "vta":      "https://fpp-a1b2c3d4.example.com",
    "mediator": "https://mediator-a1b2c3d4.example.com",
    "dids":     "https://dids-a1b2c3d4.example.com"
  },
  "collected": {
    "vta_did": "did:webvh:…:dids-a1b2c3d4.example.com:vta",     // 1a — REQUIRED: user feeds it to `pnm setup continue --vta-did`
    "mediator_did": "did:webvh:…:dids-a1b2c3d4.example.com:mediator",
    "did_hosting_did": "did:webvh:…:dids-a1b2c3d4.example.com",
    "mediator_admin_did": "did:key:z6Mk…",
    "did_hosting_admin_did": "did:key:z6Mk…"
  },
  "action_required": {
    "dids_admin_enroll_url": "https://dids-a1b2c3d4.example.com/enroll/…",  // 3e — REQUIRED output, single-use, visit to set a passkey
    "reveal_keys_once": true                                       // 2c / 3c shown once
  }
}
```

**PNM handshake (user-local, mirrors `vta_only`).** `full_stack` does *not* run `pnm
setup` / `pnm setup continue`. The user (1) runs `pnm setup --name <name>` locally to mint
their admin DID, (2) hands it to the API as `admin_did` (on `POST /setup` or `POST
/setup/:id/admin`) so the API can `vta import-did` it before booting the VTA, then (3)
runs `pnm setup continue <name> --vta-did <1a>` locally with the returned VTA DID. The API
owns only step (2).

**Every new route must be added to `internal/apidocs/openapi.yaml`** with the `User`
tag (per the API Docs Rule in CLAUDE.md).

---

## 13. Teardown

`DELETE /setup/:id` for `full_stack`, in order:

1. `orch.Cancel(sid)` — stop the goroutine.
2. Delete 3 Cloudflare records (`CFRecordID/Mediator/Dids`).
3. Delete component resources: Deployments, Services, Ingresses, PVCs for
   vta/mediator/dids, and all setup Jobs/ConfigMaps.
4. Delete Vault material: `TeardownVaultSeed` (VTA master seed), `TeardownMediatorVault`
   (mediator's KV prefix), and `TeardownDidsVault` (dids daemon's KV prefix). No token
   to revoke anywhere — kubernetes auth leaves nothing to clean up beyond the KV data
   itself.
5. Delete the `SetupSession` row.
6. If this was the user's last session, `DeleteNamespace` + `TeardownVaultUserAccess`
   (remove the per-user Vault policy/role) — both already implemented.

No external DID-host cleanup is needed (unlike `vta_only`) — the mediator + dids pods are
namespaced and die with everything else; only the Vault material (VTA seed, mediator
secrets, dids secrets, and the per-user Vault access on the last session) needs explicit
removal.

---

## 14. Manual gates that remain

The chain is fully automatable **except** these user-local touchpoints:

1. **PNM binding (`4a`).** Mirrors `vta_only`: the user runs `pnm setup --name <name>` and
   `pnm setup continue <name> --vta-did <1a>` on their own machine. They supply the
   resulting admin DID to the API as `admin_did`, and the API runs only `vta import-did`
   before `deploy_vta`. The API returns the **VTA DID (`1a`)** the user needs for `pnm
   setup continue`. This is a genuine gate: if `admin_did` is not supplied at `POST
   /setup`, the machine pauses at `awaiting_admin_did` until the user POSTs it to
   `/setup/:id/admin`.
2. **DIDS admin-panel passkey enrollment (`3e`).** The offline DID load (§6
   `step_dids_load_did`) removes the *functional* need to log in, but the user still gets
   the single-use enrollment URL (always minted in `step_dids_invite`) to register a
   passkey for the dids admin UI. Surfaced under `action_required.dids_admin_enroll_url`
   until the frontend posts `/setup/:id/dids/enroll-ack` (sets `dids_enroll_used`, which
   then hides it); single-use and regenerable via the reissue endpoint.
3. **Reveal-once secrets (`2c`, `3c`).** The mediator + webvh admin private keys are shown
   to the user once for offline backup.

Only (1) gates the VTA from starting; (2) and (3) are conveniences once the session
reaches the terminal `running` status.

---

## 15. Implementation checklist

1. Migration `000009_full_stack_fields` — additive columns (§10).
2. `model.SetupSession` — new fields + `MediatorFQDN()` / `DidsFQDN()` +
   `setup.FullStackHosts()` helper.
3. `internal/setup/templates.go` — `create_mediator` VTA template variant (reuses the
   existing Vault `[secrets]` block) + mediator (fjall message store, **Vault `vault://`
   secrets, kubernetes auth**) + webvh (p1/p3, **Vault `backend = "vault"`, kubernetes
   auth**) renderers; add `Vault.HostPort`/`Vault.MediatorPrefix`/`Vault.K8sRole` (mediator)
   and `Vault.Addr`/`Vault.SecretPath`/`Vault.K8sRole` (webvh) to the render data.
4. `internal/setup/parser.go` — regexes from §8 (incl. `3e` enroll-URL). (`4a` is a user
   input, not parsed.)
5. `internal/k8s/` — generic `CreateComponentSetupJob` (image, command, one-or-two PVC
   mounts at `/work/<c>`, workingDir, SA, recipe ConfigMap); mediator + dids
   Deployment/Service/Ingress helpers (generalize the VTA ones to a `/work/<c>` mount).
   No K8s Secret needed anywhere, no ClusterRole change.
6. ~~`internal/didhosting/` per-session client constructor~~ — not needed. DID logs are
   loaded offline via `step_dids_load_did` (`did-hosting-daemon load-did`, §6) instead of
   an HTTP control-API call, so there's no per-session `didhosting.Client` at all.
7. `internal/vault/` — extend the per-user policy to grant
   `secret/{data,metadata}/{mediator,dids}/user-<id>/session-<id>/*`; add
   `MediatorPrefix(userID, sid)`/`DidsPrefix(userID, sid)` and
   `DeleteMediatorSecrets`/`DeleteDidsSecrets` for teardown. No token minting/revocation
   anywhere — every component authenticates via the same kubernetes-auth role.
8. `internal/setup/orchestrator.go` — `runFullStack` state machine (§5); extend
   `Resume` for the new statuses. **Reuse the `vta_only` admin-DID handshake unchanged**:
   `admin_did` is a user input, `Provision` / `POST /setup/:id/admin` triggers it, and
   `vta import-did` runs before `deploy_vta` (`awaiting_admin_did` gate when not supplied
   upfront). Reuse `EnsureUserAccess` + `TeardownVaultSeed` for the VTA seed; mediator and
   dids Jobs/Deployments run as SA `vta` and need no env wiring beyond `NO_COLOR`;
   `TeardownMediatorVault`/`TeardownDidsVault` delete their KV prefixes on teardown.
9. `internal/handler/setup.go` — branch on `mode`; 3 DNS records; **reuse `POST
   /setup/:id/admin` for the PNM admin DID**; generalized teardown; reveal-once keys +
   required `vta_did` / dids-enroll-URL in `GET /setup/:id`; reissue-enroll endpoint.
10. `internal/config/config.go` + `.env.example` — §11 vars.
11. `internal/apidocs/openapi.yaml` — document new/changed routes (`User` tag).
12. Update the stale **Full Stack** section of
    [`vta-setup-design.md`](vta-setup-design.md) to point here.
13. *(added post-implementation, `vta_only` parity)* `step_vta_register_dids` — the
    `--id dids` server registration (§5/§6): `FSJobVtaRegisterDids` in
    `fullstack_names.go` (+ `allFSJobNames`), the orchestrator step + resume status,
    the `?source=` value, and the openapi status/source lists. (A paired
    `step_dids_grant_vta` — daemon-side `service` ACL for the VTA — was added and then
    removed in the same pass; see §5's "No separate step grants the VTA an ACL entry"
    note for why.)
14. *(added post-implementation, mediator/dids Vault simplification, §9)* Mediator and
    dids daemon switched from token-auth/plaintext to kubernetes auth, both upstream
    binaries having gained parity with the VTA's own auth method: removed
    `MintMediatorToken`/`RevokeToken`/`MediatorTokenRole`, `FSMediatorTokenSecret`, and
    the generic `CreateComponentSecret`/`GetComponentSecretValue`/`DeleteComponentSecret`
    K8s helpers (all now dead); added `vault.DidsPrefix`/`DeleteDidsSecrets` and
    `TeardownDidsVault`; widened `EnsureUserAccess`'s policy to cover `dids/*`; switched
    `step_mediator_p1`/`p2`/`deploy_mediator`/`step_dids_p1`/`p2`/`deploy_dids` to SA
    `vta`; updated the mediator and webvh recipe templates (§7); removed the ClusterRole
    `secrets` grant and `VAULT_MEDIATOR_TOKEN_ROLE` config/helm values.
15. *(added post-implementation, live-session bug fix)* `fsDeployDids` /
    `fsDeployMediator` / the VTA's `deploy_vta` block now call
    `WaitForComponentDeploymentReady` (2 min timeout) before returning — found via a
    real session where `step_vta_register_dids` 503'd resolving the dids daemon's
    `did.jsonl` because it ran immediately after `deploy_dids`, before the pod (and its
    Service/Ingress endpoint) was actually serving. §6's `deploy_dids`/`deploy_mediator`/
    `deploy_vta` entries updated to match.

---

## Appendix A — Verified Ubuntu reference flow

> **Superseded on secrets backends.** This appendix documents the single-host flow as
> originally verified: mediator secrets via Vault **token auth**, dids daemon on
> **plaintext**. Both upstream binaries have since gained kubernetes-auth /
> `vault-secrets` support at parity with the VTA, and the K8s mapping above (§9) has
> moved to it for both — no token to mint, no plaintext fallback. Command ordering,
> recipe shapes, and everything else below is otherwise still accurate and is kept as
> the source of truth for those.

The bare-metal flow this design automates (the "Automated VTI Setup" guide), verified
end-to-end on Ubuntu Server with `fjall` mediator message storage. Kept as the source of
truth for command ordering and recipe contents; the K8s mapping above supersedes the
home-dir / `nohup` mechanics, and now the secrets-backend choice too (see the note
above). Both the **VTA and the Mediator** store secrets in **Vault**
(the VTA via kubernetes auth as in `vta_only`; the mediator via token auth with a
`secrets-vault` build in *this* verified reference — see below); only the dids daemon
stays plaintext *in this reference*.

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

**Reference points preserved in the K8s mapping:**

- **Mediator message storage is self-contained** (`[storage].backend = "fjall"`,
  file-backed on its PVC) — no Redis/Valkey is deployed. The `[database].url` field stays
  in the recipe as a required-but-unused value. **Mediator *secrets* live in Vault**
  (`[secrets].storage = "vault://…"`, mediator built with the `secrets-vault` feature);
  Vault does not replace the fjall message store. → §2, §7, §9.
- **Start order = dids → mediator → vta**, after all setup/config steps. → §5.
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
> PNM bind transcript, and the health checks — is the upstream source for this
> appendix. This document captures everything from it that the automated K8s flow
> depends on.
</content>
