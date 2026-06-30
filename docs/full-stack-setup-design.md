# Full-Stack VTI Setup — Design

Design for the **Full Stack** setup mode: vtafarm-api provisions a complete,
self-contained VTI stack — **VTA + DIDComm Mediator + WebVH DID Hosting daemon** —
inside one user namespace, wired together, and returns three live URLs:

```text
https://vta.{domain}        ← VTA REST + DIDComm
https://mediator.{domain}   ← DIDComm Mediator
https://dids.{domain}       ← WebVH DID Hosting daemon (admin panel + DID resolution)
```

This is the K8s/API automation of the manual single-host flow verified on Ubuntu
(reproduced in [Appendix A](#appendix-a--verified-ubuntu-reference-flow)). Every
`<binary> setup --from <recipe>` command in that flow becomes a one-off K8s **Job**;
every cross-component file handoff (the `~/vta`, `~/mediator`, `~/dids` home dirs on a
single host) is reproduced by mounting the relevant PVCs into the Job that needs them;
every `nohup <binary> &` becomes a **Deployment**.

**Secret backends.** The **VTA and Mediator both store their secrets in the farm
Vault**; only the DID Hosting daemon stays on a `plaintext` backend (on its PVC). The
two Vault integrations differ in *how they authenticate*:

- **VTA** — kubernetes auth: the VTA pod presents its `vta` ServiceAccount JWT against
  the per-user kubernetes-auth role. Unchanged from `vta_only`.
- **Mediator** — token auth: the mediator binary (built with the `secrets-vault`
  feature) reads a `VAULT_TOKEN` from its environment and writes/reads its secrets under
  a `vault://…` KV v2 prefix. The orchestrator mints that token (scoped to the
  mediator's per-session prefix) and injects it; see [§9](#9-secrets-handling).

The mediator's *message* storage is still **fjall** (file-backed on its PVC) — Vault
only holds the mediator's secrets, not its message store. No Redis/Valkey.

**Scope:** VTA + Mediator + DID Hosting only. The reference flow stops at PNM binding
(Step 4); there is no VTC step, so VTC is out of scope here too.

> Status: **design only**. No code in this repo implements `full_stack` yet — the
> orchestrator currently runs the VTA-only path for every session regardless of
> `mode`. This document specifies what `full_stack` should do.

---

## 1. How this differs from the existing flow

The flow you have been running (`vta_only`) and Full Stack differ in **who owns the
mediator and the DID host**:

| | `vta_only` (today) | `full_stack` (this design) |
| --- | --- | --- |
| Mediator | **Shared**, external. VTA points at `MEDIATOR_DID` via `[messaging] kind = "existing"`. | **Per-deployment**, in-cluster. VTA *creates* it via `[messaging] kind = "create_mediator"`, then the mediator is provisioned + run in the same namespace. |
| DID host | **Shared**, external (`DID_HOSTING_SERVER_URL`). API uploads the VTA DID log to it via the control client. | **Per-deployment**, in-cluster. A fresh `did-hosting-daemon` is provisioned + run in the same namespace; the VTA + mediator DID logs are uploaded to *it*. |
| URLs returned | 1 (`fpp-xxxx.{domain}`) | 3 (`vta`/`mediator`/`dids.{domain}`) |
| Subdomain | random `fpp-xxxx` prefix | three named hosts (see [§3](#3-urls--dns)) |
| Secret backend | HashiCorp Vault (`[secrets] backend = "vault"`) | **VTA: Vault** (k8s auth, same as `vta_only`). **Mediator: Vault** (token auth, `vault://`). **dids: plaintext** on its PVC |
| Mediator storage | n/a (shared) | secrets in **Vault**; messages in **fjall** (file-backed, on the mediator PVC) — no Redis/Valkey |
| K8s resources / session | 1 PVC + Deployment + Service + Ingress | 3× (PVC + Deployment + Service + Ingress) |

The existing building blocks are **reused unchanged**: per-user namespace +
ServiceAccounts (`EnsureUserEnvironment`), the Cloudflare A-record helpers, the Job
watch/log-capture helpers (`WaitForJob`, `JobLogs`, `StreamJobLogs`), and the
`---DID_LOG_START---` stdout-marker trick for pulling files out of a finished Job.
`EnsureUserAccess` (the per-user Vault policy + kubernetes-auth role) is **also reused**
for the VTA. The mediator needs a small extension: the same per-user policy also grants
the mediator's KV prefix, and the orchestrator mints a `VAULT_TOKEN` for the mediator
(token auth). Only the dids daemon stays off Vault.

---

## 2. Component topology (one user namespace)

```text
namespace: vtafarm-user-{userID}
┌──────────────────────────────────────────────────────────────────────────┐
│    services & ingresses (created up front; 503 until pods come up)         │
│   ┌────────────┐   ┌────────────────┐   ┌────────────────┐                 │
│   │ vta-{sid}  │   │ mediator-{sid} │   │ dids-{sid}     │                 │
│   │ :8100      │   │ :7037          │   │ :8534          │                 │
│   └─────┬──────┘   └──────┬─────────┘   └──────┬─────────┘                 │
│  Ingress│vta.{d}   Ingress│mediator.{d} Ingress│dids.{d}                   │
│         │                 │                    │                           │
│   ┌─────▼──────┐   ┌──────▼─────────┐   ┌──────▼─────────┐                 │
│   │ Deployment │   │ Deployment     │   │ Deployment     │                 │
│   │ vta        │   │ mediator       │   │ dids-daemon    │                 │
│   │ PVC vta    │   │ PVC mediator   │   │ PVC dids       │                 │
│   │ /work/vta  │   │ /work/mediator │   │ /work/dids     │                 │
│   └────────────┘   └────────────────┘   └────────────────┘                 │
│                                                                            │
│   Setup Jobs mount whichever PVC(s) they touch; cross-component steps      │
│   mount two PVCs at once (e.g. /work/vta + /work/mediator). Steps run      │
│   strictly in sequence, so plain RWO PVCs are never mounted concurrently.  │
└──────────────────────────────────────────────────────────────────────────┘
```

| Component | Port | Public URL | Storage | Notes |
| --- | --- | --- | --- | --- |
| VTA | 8100 | `vta.{domain}` | PVC `vta-data-{sid}` + **Vault** seed | reuses today's VTA resources + Vault path (mounted at `/work/vta`) |
| Mediator | 7037 | `mediator.{domain}` | secrets in **Vault** (`vault://`); config + `fjall` message store on PVC `mediator-data-{sid}` | token auth via injected `VAULT_TOKEN`; no Redis/Valkey |
| DID Hosting daemon | 8534 | `dids.{domain}` | PVC `dids-data-{sid}` (`plaintext` secrets) | standard (integrated) topology |

Each component is one PVC + one Deployment + one Service + one Ingress. Nothing is
shared between them at runtime — the cross-component file handoffs happen only during
setup ([§4](#4-cross-component-file-handoffs)).

---

## 3. URLs & DNS

The user supplies a base `domain`; the backend derives three hostnames. Two naming
strategies, chosen by an env flag (see [§11](#11-config--env-additions)):

| Strategy | Hosts | When |
| --- | --- | --- |
| **Named** (default for a dedicated VTI) | `vta.{domain}`, `mediator.{domain}`, `dids.{domain}` | one stack per domain — the three URLs you asked for |
| **Prefixed** (multi-tenant farm) | `fpp-xxxx.{domain}`, `mediator-xxxxx.{domain}`, `dids-xxxxx.{domain}` | many stacks under one shared farm domain; reuses `GenerateSubdomain` |

Three Cloudflare A-records are created **before any Job runs**, all pointing at
`CLUSTER_INGRESS_IP`, `proxied=true`:

```text
A   vta.{domain}        →  {CLUSTER_INGRESS_IP}
A   mediator.{domain}   →  {CLUSTER_INGRESS_IP}
A   dids.{domain}       →  {CLUSTER_INGRESS_IP}
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

All other setup Jobs mount only their own component's PVC. The recipes use **relative
paths** for on-PVC files (`config.toml`, `data/vta`, `conf/mediator.toml`, …) exactly as
the reference does; with `workingDir = /work/<component>` they resolve identically to the
home-dir layout, so the recipe bodies are essentially verbatim. (The mediator's
`[secrets].storage` is the exception — a `vault://` URL, not a PVC path.)

> The VTA's two DID logs (`VTA-did.jsonl`, `mediator-did.jsonl`) still need to reach the
> running daemon for the upload step ([§6](#step-step_upload_didlogs--control-api-call-no-job)).
> The orchestrator captures them from the `step_vta_setup` Job's stdout via the
> existing `---DID_LOG_START---`/`---ARTIFACT:…---` marker trick, so it doesn't have to
> re-mount the VTA PVC later.

---

## 5. State machine

```text
pending
  → dns_provision         create 3 Cloudflare A-records
  → env_provision         EnsureUserEnvironment (ns + SAs + Role/RoleBinding) + EnsureUserAccess (Vault policy/role for VTA + mediator KV prefix; mint mediator VAULT_TOKEN)
  → k8s_provision         PVCs, Services, Ingresses for vta/mediator/dids
  → step_vta_setup        vta setup --from vta-setup.toml          → 1a VTA DID, 1b Mediator DID, DID logs
  → step_mediator_p1      mediator-setup (phase 1)                 → mediator/bootstrap-request.json
  → step_mediator_reprov  vta contexts reprovision                 → mediator/bundle.armor, 2a digest, 2b admin DID
  → step_mediator_p2      mediator-setup --bundle --digest         → 2c admin priv key
  → step_dids_p1          did-hosting-daemon setup (offline-prepare)→ dids/bootstrap-request.json
  → step_dids_provision   vta bootstrap provision-integration      → dids/bundle.armor, 3a digest
  → step_dids_p2          did-hosting-daemon setup (offline-complete)→ 3b admin DID, 3c admin priv key, 3d daemon DID
  → step_dids_invite      did-hosting-daemon invite --role admin   → 3e dids admin-enroll URL (returned to user)
  → deploy_dids           Deployment dids-daemon (start it)
  → step_upload_didlogs   register mediator + VTA DID logs on the running daemon (control API)
  → deploy_mediator       Deployment mediator (start it)
  → awaiting_admin_did    ⏸ gate: wait for user's PNM admin DID (skip if admin_did given at POST /setup)
  → step_import_admin_did vta import-did --role admin --label pnm-bootstrap --did {{admin_did}}
  → deploy_vta            Deployment vta (start it)
  → completed             return 3 URLs + 1a VTA DID + 3e dids admin-enroll URL
        ↓ (any step)
     failed
```

**Why this order.** Setup/config order is VTA → mediator → dids (each later recipe
consumes a DID or bundle the earlier one produced). But the *start* order is **dids
first, then mediator, then VTA** — exactly as the reference: the DID logs must be
uploaded to a *running* daemon, the mediator must be reachable before the VTA boots, and
the VTA must have the admin DID imported before it starts. The `vta contexts reprovision`,
`vta bootstrap provision-integration`, and `vta import-did` commands are **CLI operations
against the VTA's PVC**, not HTTP calls, so they run as Jobs without the VTA server
running.

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
`step_import_admin_did`, `deploy_vta`, and `completed` are the **same steps, same names** as
the VTA-Only machine (`vta-setup-design.md`, Mode A). `full_stack` only adds the mediator/dids
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
VTA-side Jobs run as SA `vta`; mediator/dids Jobs run as SA `pod-operator`. Image per
component comes from the request (see [§10](#10-data-model-changes)).

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

### Step `step_mediator_p1` — Mediator Job (workingDir `/work/mediator`)

Env: `VAULT_TOKEN` (`secretKeyRef` → the per-session token Secret) + `VAULT_SKIP_VERIFY=true`
(self-signed in-cluster CA).

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

### Step `step_mediator_p2` — Mediator Job (workingDir `/work/mediator`)

Same `VAULT_TOKEN` + `VAULT_SKIP_VERIFY` env as Phase 1 — Phase 2 reads back the seed
Phase 1 stored in Vault, so the token must still be valid (and point at the same
`vault://…` prefix).

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

### Step `step_dids_p1` — DIDS Job (offline-prepare, workingDir `/work/dids`)

```sh
did-hosting-daemon setup --from /config/webvh-recipe.toml
```

Recipe `vta_mode = "offline-prepare"`, `[vta] request_path = "bootstrap-request.json"`.
Writes that request file + stores the seed in the dids plaintext backend. Nothing to
parse (the `client_did` line is informational).

### Step `step_dids_provision` — VTA Job (mounts `/work/vta` + `/work/dids`, workingDir `/work/vta`)

```sh
vta bootstrap provision-integration \
  --request /work/dids/bootstrap-request.json \
  --out /work/dids/bundle.armor \
  --create-context
```

Parse **3a** SHA-256 digest.

### Step `step_dids_p2` — DIDS Job (offline-complete, workingDir `/work/dids`)

Re-render the recipe (second ConfigMap) with `vta_mode = "offline-complete"` and the
`[vta]` block carrying `bundle_path = "bundle.armor"`, `expect_digest = {{3a}}`.

```sh
did-hosting-daemon setup --from /config/webvh-recipe.toml \
  && echo '---ARTIFACT:server_did---' && grep '^server_did' config.toml
```

Parse: **3b** Admin DID (`Generated admin did:key:\s+(did:\S+)`), **3c** Admin private
key (`Private key \(save now, not re-shown\):\s+(\S+)` → return to user once), **3d**
Daemon DID (the `server_did` value from `config.toml`).

### Step `step_dids_invite` — DIDS Job (`did-hosting-daemon invite`, workingDir `/work/dids`)

Mints **3e**, the dids admin-panel enrollment URL — the value the user logs in with:

```sh
did-hosting-daemon invite --role admin --did {{3b}}
```

It opens the local store directly, so it must run **before** `deploy_dids` (while no
daemon holds the PVC). Parse **3e** from the output and return it to the user as a
**required** output ([§12](#12-api-surface)) — not optional. The automated DID-log upload
(`step_upload_didlogs`) removes the *functional* need to log in, but this URL is what lets
the user reach the dids admin web UI afterward. It is single-use; regenerate via the
reissue endpoint.

### `deploy_dids` — start the daemon Deployment

Deployment with `workingDir = /work/dids`, command `["did-hosting-daemon"]`, mounting
the dids PVC, plus the Service + Ingress for `dids.{domain}` (created in
`k8s_provision`, now backed by a running pod). Wait for Ready.

### Step `step_upload_didlogs` — control-API call (no Job)

Replaces the **manual browser upload** in the reference (the "Upload DID logs" UI step).
The orchestrator constructs a per-session `didhosting.Client` pointed at the daemon's
**in-cluster** control URL (`http://dids-{sid}.{ns}.svc:8534`) authenticated with the
captured admin DID/key (**3b**/**3c**), then calls `RegisterDid` twice:

```text
RegisterDid(path="mediator", didLog=<mediator-did.jsonl from step_vta_setup>)
RegisterDid(path="vta",      didLog=<VTA-did.jsonl from step_vta_setup>)
```

> **Implementation note:** the existing `didhosting.New` expects a base64 32-byte
> Ed25519 seed, but **3c** is multibase (`z3u2…`). Add a constructor that decodes the
> multibase admin key. Talking to the ClusterIP avoids waiting on public DNS/TLS.
>
> **Fallback = keep it manual:** if per-session control auth isn't wired yet, surface
> `dids.{domain}` + the enrollment URL (**3e**) and the two DID-log blobs to the user
> and let them upload via the admin UI, exactly as the verified flow does.

### `deploy_mediator` — start the mediator Deployment

Deployment with `workingDir = /work/mediator`, command `["mediator"]`, mounting the
mediator PVC (it reads `conf/mediator.toml` and the `fjall` message store from there).
Env: `VAULT_TOKEN` (`secretKeyRef` → the per-session token Secret) + `VAULT_SKIP_VERIFY`
— the mediator reads its secrets from Vault at startup and *probes* (write→read→delete a
sentinel), so the token's policy must allow all four. Service + Ingress for
`mediator.{domain}`. No Redis/Valkey dependency.

> **Token lifetime:** the Deployment is long-running and re-reads secrets on every pod
> restart, so the injected token must be **periodic/renewable or long-TTL** — a
> short-TTL token would leave the mediator unable to read its secrets after a restart.
> Provision it as a periodic token scoped to the mediator policy.

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
Service + Ingress for `vta.{domain}` (generalize today's `CreateVtaDeployment` /
`CreateVtaService` / `CreateVtaIngress` to the `/work/vta` mount path).

---

## 7. Recipe templates

PVCs mount at `/work/<component>` with matching `workingDir`, so the recipes keep the
reference's **relative paths verbatim**. Templated fields in `{{…}}`.

<details><summary><b>vta-setup.toml</b> (full_stack)</summary>

```toml
config_path = "config.toml"
data_dir    = "data/vta"
vta_name    = "{{ .VtaName }}"
public_url  = "https://vta.{{ .Domain }}"

[services]
rest    = true
didcomm = true

[server]
host = "0.0.0.0"
port = 8100

[log]
level  = "{{ .LogLevel }}"
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
url       = "https://mediator.{{ .Domain }}/mediator/v1"
webvh_url = "https://dids.{{ .Domain }}/mediator"

[vta_did]
kind               = "create_webvh"
url                = "https://dids.{{ .Domain }}/vta"
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
# Vault KV v2: vault://<host[:port]>/<kv-mount>/<prefix>. Auth = the injected VAULT_TOKEN
# env (+ VAULT_SKIP_VERIFY for the self-signed in-cluster CA). The mediator image MUST be
# built with the `secrets-vault` feature. e.g. vault://vault.vault.svc:8200/secret/mediator/user-12/session-34
storage = "vault://{{ .Vault.HostPort }}/{{ .Vault.KVMount }}/{{ .Vault.MediatorPrefix }}"

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
log_level  = "{{ .LogLevel }}"
log_format = "text"
data_dir   = "data/daemon"

[identity]
public_url   = "https://dids.{{ .Domain }}"
mediator_did = "{{ .MediatorDID }}"   # 1b

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

[secrets]
backend           = "plaintext"
confirm_plaintext = true

[admin]
mode = "generate"

[reprovision]
force = false
```
</details>

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

The **VTA and Mediator use Vault**; the DID Hosting daemon stays plaintext.

**VTA — kubernetes auth (unchanged from `vta_only`).** The master seed lives at
`secret/vta/user-<id>/session-<id>/master-seed`; the VTA pod authenticates with its
`vta` ServiceAccount JWT against the per-user kubernetes-auth role `vta-user-<id>`. The
API only provisions the policy/role (`EnsureUserAccess`) and deletes the seed on
teardown — it never reads it.

**Mediator — token auth (`vault://`).** The mediator binary must be **built with the
`secrets-vault` feature** (`--features "didcomm,redis-backend,fjall-backend,secrets-vault"`).
Its recipe points `[secrets].storage` at `vault://<host:port>/<kv-mount>/<prefix>`, and
it authenticates with a `VAULT_TOKEN` read from its environment (plus
`VAULT_SKIP_VERIFY` for the self-signed in-cluster CA). The orchestrator wires this up
per session:

1. **Scope** the prefix per tenant — `mediator/user-<id>/session-<id>` under the same KV
   mount (`secret`), mirroring the VTA's seed-path scheme. Secrets land at
   `secret/data/mediator/user-<id>/session-<id>/{mediator_jwt_secret,…}`.
2. **Policy** — extend the per-user policy to grant that prefix. The mediator *probes*
   (write → read → delete a sentinel) at setup and startup, so read alone is not enough:

   ```hcl
   path "secret/data/mediator/user-<id>/session-<id>/*" {
     capabilities = ["create", "update", "read", "delete"]
   }
   path "secret/metadata/mediator/user-<id>/session-<id>/*" {
     capabilities = ["read", "list", "delete"]
   }
   ```
3. **Mint a token** attached to that policy (the API already holds an AppRole session, so
   it can create a child token). Make it **periodic/renewable or long-TTL** — the mediator
   Deployment re-reads secrets on every restart, so a short-TTL token would strand it.
   Store it in a per-session K8s Secret `mediator-vault-token-{sid}`.
4. **Inject** it as `VAULT_TOKEN` (`secretKeyRef`) into both mediator setup Jobs and the
   mediator Deployment, alongside `VAULT_SKIP_VERIFY=true`.

> This is the one structural difference from the VTA: the VTA never needs a token in its
> pod (kubernetes auth), whereas the mediator does. If a future mediator build gains
> kubernetes-auth support it can drop the token and mirror the VTA exactly.

**DIDS daemon — plaintext** (`[secrets] backend = "plaintext"`) on its PVC; unchanged.

**Admin private keys (2c, 3c).** Still captured from the setup logs and surfaced to the
user **once** via `GET /setup/:id` for offline backup. The mediator key (2c) is also
already in Vault; the dids key (3c) lives only in the dids plaintext backend.

---

## 10. Data model changes

Extend `SetupSession` (additive columns; new migration `000009_full_stack_fields`):

```go
// Three subdomains instead of one (Subdomain stays for vta_only back-compat).
VtaSubdomain      string
MediatorSubdomain string
DidsSubdomain     string

// One Cloudflare record id per host (CFRecordID stays for vta_only).
CFRecordVta      string
CFRecordMediator string
CFRecordDids     string

// Per-component images (VtaImage exists today).
MediatorImage string
DidsImage     string

// Collected outputs (VtaDid, AdminDid exist today). AdminDid already holds the
// user-supplied PNM admin DID (4a) in vta_only — full_stack reuses it as-is (no new
// column, no parsing). full_stack adds exactly two more admin DIDs:
MediatorDid      string  // 1b  (NB: today this column holds the *shared* mediator DID for vta_only)
MediatorAdminDid string  // 2b  → json: mediator_admin_did
WebvhAdminDid    string  // 3b  → json: webvh_admin_did
DidsDaemonDid    string  // 3d

// Admin private keys, returned to the user once (2c, 3c); plaintext, like the PVCs.
MediatorAdminKey string  // 2c
WebvhAdminKey    string  // 3c
DidsEnrollURL    string  // 3e dids admin-enroll URL — REQUIRED output, single-use (regenerable)
```

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
| `MEDIATOR_IMAGE` | default mediator image — **must be built with the `secrets-vault` feature** (`--features "didcomm,redis-backend,fjall-backend,secrets-vault"`) |
| `DIDS_IMAGE` | default image for the did-hosting-daemon |
| `FULL_STACK_HOST_STRATEGY` | `named` (→ `vta.{domain}`) \| `prefixed` (→ `fpp-xxxx-vta.{domain}`) |

Reuse existing: `CLUSTER_INGRESS_IP`, `CLUSTER_DOMAIN`, `CLOUDFLARE_*`, and all
`VAULT_*`. Both the VTA seed and the mediator secrets are Vault-backed; the mediator's
`vault://` host is derived from `VAULT_VTA_ADDR` (in-cluster `vault.vault.svc:8200`) and
its KV prefix is `mediator/user-<id>/session-<id>` under `VAULT_KV_MOUNT`.
`MEDIATOR_DID` and `DID_HOSTING_*` remain **only** for `vta_only`; `full_stack` ignores
them (it grows its own mediator + dids — mediator on Vault, dids plaintext).

The existing ClusterRole covers most of this mode (namespaces, SAs, pods, pods/log,
configmaps, pvcs, services, roles, rolebindings, jobs, deployments, ingresses) per
CLAUDE.md. **One addition:** `secrets` (`get`/`create`/`delete`) — full_stack creates a
per-session K8s Secret to hold the mediator's `VAULT_TOKEN`. (The VTA seed and the
mediator secrets themselves live in Vault, not K8s Secrets.)

---

## 12. API surface

Mostly the existing endpoints, generalized for three components. `mode = "full_stack"`
on `POST /api/v1/setup` selects this path.

| Method | Path | Change |
| --- | --- | --- |
| `POST` | `/setup/validate` | also assert all three hosts are creatable |
| `POST` | `/setup` | accept `mode=full_stack`, optional `mediator_image`/`dids_image`, optional `admin_did` (user's local PNM admin DID — when present, auto-runs import + `deploy_vta`); create 3 DNS records; start the §5 machine |
| `POST` | `/setup/:id/admin` | **reused from `vta_only`** — supply the user's PNM `admin_did` once the stack is up (`awaiting_admin_did`); triggers `vta import-did` + `deploy_vta` |
| `GET` | `/setup/:id` | return the three URLs + **VTA DID (1a)** + **dids admin-enroll URL (3e)** + per-step status + (once) the admin keys |
| `GET` | `/setup/:id/logs` | `?source=` gains `mediator_p1\|mediator_p2\|dids_p1\|dids_p2\|dids_invite\|import_admin_did\|mediator\|dids` |
| `DELETE` | `/setup/:id` | tear down all 3 DNS records + all component resources |
| `POST` | `/setup/:id/dids/reissue-enroll` *(new, optional)* | regenerate the single-use dids admin enrollment URL (`did-hosting-daemon invite`) |

`GET /setup/:id` (full_stack) response sketch:

```jsonc
{
  "id": "ab12cd34",
  "mode": "full_stack",
  "status": "completed",
  "urls": {
    "vta":      "https://vta.example.com",
    "mediator": "https://mediator.example.com",
    "dids":     "https://dids.example.com"
  },
  "collected": {
    "vta_did": "did:webvh:…:dids.example.com:vta",     // 1a — REQUIRED: user feeds it to `pnm setup continue --vta-did`
    "mediator_did": "did:webvh:…:dids.example.com:mediator",
    "dids_daemon_did": "did:webvh:…:dids.example.com",
    "mediator_admin_did": "did:key:z6Mk…",
    "webvh_admin_did": "did:key:z6Mk…"
  },
  "action_required": {
    "dids_admin_enroll_url": "https://dids.example.com/enroll/…",  // 3e — REQUIRED output, single-use, visit to set a passkey
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
2. Delete 3 Cloudflare records (`CFRecordVta/Mediator/Dids`).
3. Delete component resources: Deployments, Services, Ingresses, PVCs for
   vta/mediator/dids, all setup Jobs/ConfigMaps, and the `mediator-vault-token-{sid}` Secret.
4. Delete Vault material: `TeardownVaultSeed` (VTA master seed) **and** the mediator's
   KV prefix (`secret/{data,metadata}/mediator/user-<id>/session-<id>/*`); revoke the
   mediator's token.
5. Delete the `SetupSession` row.
6. If this was the user's last session, `DeleteNamespace` + `TeardownVaultUserAccess`
   (remove the per-user Vault policy/role) — both already implemented.

No external DID-host cleanup is needed (unlike `vta_only`) — the mediator + dids pods are
namespaced and die with everything else; only the Vault material (VTA seed, mediator
secrets + token, and the per-user Vault access on the last session) needs explicit
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
2. **DIDS admin-panel passkey enrollment (`3e`).** Programmatic DID-log upload (§6
   `step_upload_didlogs`) removes the *functional* need to log in, but the user still gets
   the single-use enrollment URL (always minted in `step_dids_invite`) to register a
   passkey for the dids admin UI. Surfaced under `action_required.dids_admin_enroll_url`;
   single-use and regenerable via the reissue endpoint.
3. **Reveal-once secrets (`2c`, `3c`).** The mediator + webvh admin private keys are shown
   to the user once for offline backup.

Only (1) gates the VTA from starting; (2) and (3) are post-`completed` conveniences.

---

## 15. Implementation checklist

1. Migration `000009_full_stack_fields` — additive columns (§10).
2. `model.SetupSession` — new fields + `MediatorFQDN()` / `DidsFQDN()` + host-strategy
   helper.
3. `internal/setup/templates.go` — `create_mediator` VTA template variant (reuses the
   existing Vault `[secrets]` block) + mediator (fjall message store, **Vault `vault://`
   secrets**) + webvh (p1/p3, plaintext) renderers; add `Vault.HostPort` +
   `Vault.MediatorPrefix` to the render data.
4. `internal/setup/parser.go` — regexes from §8 (incl. `3e` enroll-URL) + multibase key
   decode. (`4a` is a user input, not parsed.)
5. `internal/k8s/` — generic `CreateComponentSetupJob` (image, command, one-or-two PVC
   mounts at `/work/<c>`, workingDir, SA, recipe ConfigMap, optional `VAULT_TOKEN`
   `secretKeyRef` + `VAULT_SKIP_VERIFY` env); mediator + dids Deployment/Service/Ingress
   helpers (generalize the VTA ones to a `/work/<c>` mount); create the per-session
   `mediator-vault-token-{sid}` Secret. **ClusterRole += `secrets`.**
6. `internal/didhosting/` — per-session client constructor accepting a multibase admin
   key + an in-cluster base URL.
7. `internal/vault/` — extend the per-user policy to grant
   `secret/{data,metadata}/mediator/user-<id>/session-<id>/*`; add `MediatorPrefix(userID, sid)`,
   `MintMediatorToken` (periodic, scoped), and teardown (delete mediator secrets + revoke token).
8. `internal/setup/orchestrator.go` — `runFullStack` state machine (§5); extend
   `Resume` for the new statuses. **Reuse the `vta_only` admin-DID handshake unchanged**:
   `admin_did` is a user input, `Provision` / `POST /setup/:id/admin` triggers it, and
   `vta import-did` runs before `deploy_vta` (`awaiting_admin_did` gate when not supplied
   upfront). Reuse `EnsureUserAccess` + `TeardownVaultSeed` for the VTA seed; mint +
   inject the mediator `VAULT_TOKEN`, and revoke it + delete the mediator KV prefix on
   teardown.
9. `internal/handler/setup.go` — branch on `mode`; 3 DNS records; **reuse `POST
   /setup/:id/admin` for the PNM admin DID**; generalized teardown; reveal-once keys +
   required `vta_did` / dids-enroll-URL in `GET /setup/:id`; reissue-enroll endpoint.
10. `internal/config/config.go` + `.env.example` — §11 vars.
11. `internal/apidocs/openapi.yaml` — document new/changed routes (`User` tag).
12. Update the stale **Full Stack** section of
    [`vta-setup-design.md`](vta-setup-design.md) to point here.

---

## Appendix A — Verified Ubuntu reference flow

The bare-metal flow this design automates (the "Automated VTI Setup" guide), verified
end-to-end on Ubuntu Server with `fjall` mediator message storage. Kept as the source of
truth for command ordering and recipe contents; the K8s mapping above supersedes the
home-dir / `nohup` mechanics. Both the **VTA and the Mediator** store secrets in **Vault**
(the VTA via kubernetes auth as in `vta_only`; the mediator via token auth with a
`secrets-vault` build — see below); only the dids daemon stays plaintext.

**Verified versions:**

| VTA | Mediator | DID Hosting daemon |
| --- | --- | --- |
| 0.7.0 | 0.15.5 | 0.7.0 |
| 0.6.0 | 0.15.4 | 0.7.0 |
| 0.6.0 | 0.15.3 | 0.7.0 |

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
- **DID logs are uploaded to a *running* daemon** (reference: via the admin UI). →
  automated in §6 `step_upload_didlogs`, with the manual UI upload as the fallback.
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

The K8s mapping (§9) keeps all of this but swaps the single static `VAULT_TOKEN`/prefix
for a **per-session** token + `mediator/user-<id>/session-<id>` prefix, injected from a
K8s Secret, with `VAULT_SKIP_VERIFY=true` for the self-signed in-cluster CA.

> The full step-by-step reference guide — including the recipe edits, the
> `vta contexts reprovision` / `vta bootstrap provision-integration` transcripts, the
> PNM bind transcript, and the health checks — is the upstream source for this
> appendix. This document captures everything from it that the automated K8s flow
> depends on.
</content>
