# VTA Setup API — Design

Automates VTA stack installation driven by a frontend form. Supports two modes:

- **VTA Only** (`vta_only`) — deploys just the VTA service; user provides an existing external DID hosting URL.
- **Full Stack** (`full_stack`) — deploys VTA + DIDComm Mediator + WebVH DID Hosting Daemon + VTC; all four are hosted in-cluster.

> **The VTC is always part of Full Stack.** An earlier iteration split this into
> `full_stack` (three components) and `full_stack_with_vtc` (four). That split is retired:
> the three-component pipeline is gone and `full_stack` now always provisions all four.
> The `full_stack_with_vtc` mode identifier no longer exists.

In both modes the backend:

1. Validates form inputs
2. Creates Cloudflare DNS subdomains
3. Provisions K8s resources (PVC, Service, Ingress per component)
4. Runs CLI setup commands as K8s Jobs, parsing stdout to collect DIDs/digests
5. Starts persistent Deployments after setup completes
6. Returns the live service URLs to the user

Reference for the underlying CLI commands: [Automated Setup](automated-setup.md)
VTA source & architecture: `verifiable-trust-infrastructure/docs/`

---

## Deployment Modes

### Mode A — VTA Only

User provides their own DID hosting endpoint. VTA Farm deploys only the VTA service.

```text
User provides (form):
  vta_name    → unique name per user (default "personal-vta")
  vta_image   → full image URL chosen from GET /setup/images
  admin_did   → optional; the user's local `pnm setup` admin DID
  portable, pre_rotation_count → optional advanced VTA-DID knobs

Backend derives (not user input):
  subdomain        → "vta-{vta_name}" ("dev-vta-{vta_name}" in dev), under CLUSTER_DOMAIN
  vta public URL   → https://{subdomain}.{CLUSTER_DOMAIN}
  did_hosting_url  → {platform stack's dids URL}/{vta_name}-vta            (shared host)
  mediator         → the platform stack's mediator DID

VTA TOML uses:
  [secrets]
  backend = "vault"            ← master seed in HashiCorp Vault (kubernetes auth), not plaintext

  [messaging]
  kind = "existing"            ← points at the shared mediator
  did  = "{mediator_did}"

  [vta_did]
  kind = "create_webvh"
  url  = "{did_hosting_url}"
```

**State machine (VTA Only)** — step names below match the code; the shared steps reuse the
**same names** in Full Stack (Mode B). The implemented DB `status` value for each step is in
parentheses:

```text
pending
  → dns_provision          POST /setup: create the Cloudflare A-record + persist session   (status: dns_provisioned)
  → step_vta_setup         EnsureUserEnvironment + EnsureUserAccess + render TOML, then the
                           `vta setup` Job; parse VTA DID (1a) + upload DID log to the host  (status: vta_setup_running → vta_setup_complete)
  → awaiting_admin_did     gate: wait for the user's PNM admin DID
                           (auto-skipped when admin_did was supplied at POST /setup)         (status: stays at vta_setup_complete)
  → step_import_admin_did  create the hosting ACL + the `vta import-did` Job                 (status: provisioning)
  → deploy_vta             create the VTA Deployment + Service + Ingress                      (status: provisioning → running)
  → completed                                                                                 (status: running)
        ↓ (any step)
     failed
```

From the code (`internal/setup/orchestrator.go`, `internal/handler/setup.go`):

- **`admin_did` is a user input** — supplied at `POST /setup` (`runSetup` then auto-triggers
  the import right after `step_vta_setup`) **or** later at `POST /setup/:id/admin` (which
  triggers `Provision` out of `vta_setup_complete`). The API only ever runs `vta import-did`;
  it never runs `pnm setup` / `pnm setup continue` — those are the user's local commands.
- **`env_provision` + `k8s_provision` are folded into `step_vta_setup`** (they run inside the
  setup goroutine before the Job, while `status` is still `dns_provisioned`). Full Stack
  breaks them out as their own steps because it provisions three components.
- There is **no separate `deploy_vta` status**: `runProvision` creates the Deployment / Service
  / Ingress while still `provisioning`, then flips straight to the terminal `running`.

### Mode B — Full Stack

> **Full state machine, recipes, and API surface live in
> [`full-stack-setup-design.md`](full-stack-setup-design.md).** Don't copy the state
> machine from this section — a previous copy kept here went stale (it still showed an
> abandoned `step_upload_didlogs` design, later replaced by the offline
> `step_dids_load_did` approach; see `full-stack-setup-design.md` §4/§6/Appendix A for
> why). This section is just a pointer + the shape of the mode.

VTA Farm deploys and wires all four components. The VTC is not optional — there is no
VTC-less Full Stack.

```text
User provides:
  vta_name → creates  vta-{vta_name}.{domain}       (VTA)
                      mediator-{vta_name}.{domain}  (DIDComm Mediator)
                      dids-{vta_name}.{domain}      (WebVH DID Hosting Daemon)
  vtc_name → creates  vtc-{vtc_name}.{domain}       (Verifiable Trust Community)
```

**State machine (Full Stack)** — see
[`full-stack-setup-design.md` §5](full-stack-setup-design.md) for the authoritative,
up-to-date step list. The shared steps (`dns_provision`, `step_vta_setup`,
`awaiting_admin_did`, `step_import_admin_did`, `deploy_vta`, and the terminal `running`
status) are the **same steps, same names** as VTA Only above; Full Stack additionally
splits `env_provision` / `k8s_provision` out of `step_vta_setup` (four components to
provision instead of one), inserts the mediator/dids steps between `step_vta_setup`
and the `awaiting_admin_did` gate, and wraps `deploy_vta` in the VTC steps
(`step_vtc_setup_key` / `step_vtc_acl_grant` before it, `step_vtc_setup` / `deploy_vtc`
after).

---

## Frontend Form Fields

| Field | Mode | Required | Default | Notes |
| --- | --- | --- | --- | --- |
| `mode` | both | yes | — | `vta_only` \| `full_stack` |
| `vta_name` | both | no | `personal-vta` | unique per user; drives the VTA subdomain and the DID-hosting path |
| `vta_image` | both | yes | — | full image URL chosen from `GET /setup/images` |
| `admin_did` | both | no | — | the user's local `pnm setup` admin DID; when present, Phase 2 (import + deploy) auto-runs |
| `portable` | both | no | `true` | advanced — VTA-DID portability |
| `pre_rotation_count` | both | no | `1` | advanced — number of pre-rotation keys |
| `mediator_image` | `full_stack` | yes | — | from `GET /setup/images?component=mediator` |
| `dids_image` | `full_stack` | yes | — | from `GET /setup/images?component=dids` |
| `vtc_image` | `full_stack` | yes | — | from `GET /setup/images?component=vtc` |
| `vtc_name` | `full_stack` | no | `personal-vtc` | **unique across all sessions** (409 if taken); drives the VTC subdomain, its `did:webvh` path, and the VTA context id |

> There is **no `domain`, `did_hosting_url`, `*_port`, or `log_level` form field** in
> either mode: the domain comes from `CLUSTER_DOMAIN`, the `vta_only` DID-hosting URL is
> derived (below), and ports / log-level are fixed in the rendered TOML (`log_level` is
> hardcoded to `"info"` in both modes). See
> [`full-stack-setup-design.md`](full-stack-setup-design.md) §11/§12 for the Full Stack
> request fields.

**Backend-derived values** (not form inputs):

| Value | How it's derived | `vta_only` example |
| --- | --- | --- |
| VTA subdomain | `vta-{vta_name}` via `setup.VtaHost` (`dev-vta-{vta_name}` in dev) | `vta-personal-vta` |
| VTA public URL | `https://{subdomain}.{CLUSTER_DOMAIN}` | `https://vta-personal-vta.example.com` |
| DID hosting URL | the platform stack's dids URL + `/{vta_name}-vta` via `setup.VtaDidPath` | `https://dids.example.com/personal-vta-vta` |
| Mediator DID | the platform stack's `mediator_did`, read from its row | `did:webvh:…:mediator` |

(Full Stack instead derives four named hosts and grows its own mediator, dids daemon, and
VTC — see [`full-stack-setup-design.md` §3](full-stack-setup-design.md#3-urls--dns).)

---

## Cloudflare DNS Integration

The backend calls the Cloudflare API to create DNS records **before** running any setup Jobs. This is required because setup TOML configs reference the final HTTPS URLs, which must resolve for DID document publishing.

### Config (env vars)

| Variable | Notes |
| --- | --- |
| `CLOUDFLARE_API_TOKEN` | Cloudflare API token with `Zone:DNS:Edit` permission |
| `CLOUDFLARE_ZONE_ID` | Zone ID for the user's root domain (from Cloudflare dashboard) |
| `CLUSTER_INGRESS_IP` | External IP of the cluster's Nginx/Ingress-NGINX LoadBalancer |
| `CLUSTER_DOMAIN` | Root domain the generated subdomain is appended to (e.g. `example.com`) |

### DNS Records Created

For **VTA Only** mode (one record — the derived subdomain):

```text
A   vta-{vta_name}.{CLUSTER_DOMAIN}  →  {CLUSTER_INGRESS_IP}   proxied=true
```

For **Full Stack** mode (four records):

```text
A   vta-{vta_name}.{domain}       →  {CLUSTER_INGRESS_IP}   proxied=true
A   mediator-{vta_name}.{domain}  →  {CLUSTER_INGRESS_IP}   proxied=true
A   dids-{vta_name}.{domain}      →  {CLUSTER_INGRESS_IP}   proxied=true
A   vtc-{vtc_name}.{domain}       →  {CLUSTER_INGRESS_IP}   proxied=true
```

### Cloudflare Client — `internal/cloudflare/client.go`

```go
type Client struct {
    apiToken string
    zoneID   string
    http     *http.Client
}

// CreateARecord creates a proxied A record and returns its Cloudflare record ID.
func (c *Client) CreateARecord(ctx context.Context, name, ip string) (string, error)
// DeleteRecord removes a record by its Cloudflare record ID (idempotent).
func (c *Client) DeleteRecord(ctx context.Context, recordID string) error
// VerifyZone checks the API token can read the configured zone.
func (c *Client) VerifyZone(ctx context.Context) error
```

The returned record ID is stored on the session (`CFRecordID`) and used for teardown.

---

## Kubernetes Resource Provisioning

All resources are created in the user's isolated namespace (`vtafarm-user-{userID}`). TLS is
served by the cluster-wide **wildcard default-ssl-certificate** on nginx-ingress — there is no
cert-manager and no per-host `tls:` block.

### Resources (VTA Only)

| Resource | `vta_only` name | Purpose |
| --- | --- | --- |
| `PersistentVolumeClaim` | `vta-data-{sessionID}` | 200Mi RWO; persists `config.toml` + `data/vta/` into the Deployment |
| `ConfigMap` (setup input) | `vta-setup-{sessionID}` | holds the rendered `vta-setup.toml` |
| `Job` (setup) | `vta-setup-{sessionID}` | runs `vta setup --from …` |
| `Job` (provision) | `vta-provision-{sessionID}` | runs `vta import-did` (+ `did-mgmt servers add`) |
| `Service` | `vta-{sessionID}` | ClusterIP on 8100 |
| `Ingress` | `vta-{sessionID}` | nginx ingress (ssl-redirect annotation; wildcard TLS) |
| `Deployment` (server) | `vta-{sessionID}` | long-running VTA, starts after the provision Job |

### Ingress template

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: vta-{sessionID}
  namespace: vtafarm-user-{userID}
  annotations:
    nginx.ingress.kubernetes.io/ssl-redirect: "true"
spec:
  ingressClassName: nginx
  rules:
    - host: {subdomain}.{CLUSTER_DOMAIN}
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: vta-{sessionID}
                port:
                  number: 8100
  # no tls: block — wildcard default-ssl-certificate handles it
```

### ClusterRole additions required

The API server's ClusterRole must include:

```yaml
- apiGroups: [""]
  resources: ["persistentvolumeclaims"]
  verbs: ["get", "list", "create", "delete", "watch"]
- apiGroups: ["apps"]
  resources: ["deployments"]
  verbs: ["get", "list", "create", "update", "delete", "watch"]
- apiGroups: ["networking.k8s.io"]
  resources: ["ingresses"]
  verbs: ["get", "list", "create", "update", "delete", "watch"]
- apiGroups: ["batch"]
  resources: ["jobs"]
  verbs: ["get", "list", "create", "delete", "watch"]
```

No `secrets` verb is needed in either mode. An earlier `full_stack` design created a
per-session K8s Secret holding the mediator's minted `VAULT_TOKEN`; every component now
authenticates to Vault with kubernetes auth (its own pod's ServiceAccount JWT), so there
is no such Secret and the grant was removed. The authoritative rule set is in
`CLAUDE.md` § Kubernetes Design.

---

## Kubernetes Execution Model

### Setup phase (one-off Jobs)

Each CLI setup command runs as a K8s Job:

1. Backend renders TOML template → stores as `ConfigMap`
2. Job mounts the ConfigMap and the component's PVC
3. Backend watches Job via informer until `Succeeded` or `Failed`
4. Backend reads Pod log from the Job, extracts DIDs/digests via regex
5. Updates `SetupSession` in DB, advances state machine

**Job pod spec sketch:**

```yaml
volumes:
  - name: config
    configMap:
      name: vta-setup-{sessionID}      # key: vta-setup.toml
  - name: data
    persistentVolumeClaim:
      claimName: vta-data-{sessionID}
containers:
  - name: vta-setup
    image: {vta_image}
    workingDir: /app/vta
    # cat the DID log after the marker so the orchestrator grabs it from the Job logs
    command: ["sh", "-c", "vta setup --from /config/vta-setup.toml && echo '---DID_LOG_START---' && cat /app/vta/data/vta/did-logs/VTA-did.jsonl"]
    volumeMounts:
      - name: config
        mountPath: /config
      - name: data
        mountPath: /app/vta
# Job: backoffLimit 0, ttlSecondsAfterFinished 3600, activeDeadlineSeconds 600,
#      restartPolicy Never, serviceAccountName "vta"
```

### Server phase (persistent Deployments)

After all setup Jobs succeed, the backend creates Deployments for the running servers:

```yaml
containers:
  - name: vta
    image: {vta_image}                 # image default entrypoint — no --config override
    env:
      - { name: NO_COLOR, value: "1" }  # keep streamed logs plain-text
      - { name: CLICOLOR, value: "0" }
    volumeMounts:
      - name: data
        mountPath: /app/vta            # config.toml + data/vta/ written by the setup Job
    ports:
      - containerPort: 8100
# runs as serviceAccountName "vta" (authenticates to Vault via kubernetes auth)
```

The Deployment mounts the same PVC as the setup Job at `/app/vta`, so `config.toml` and
`data/vta/` are already present when the VTA boots.

---

## TOML Templates

### vta-setup.toml (the live `vta_only` template — `internal/setup/templates.go`)

```toml
config_path = "config.toml"
data_dir    = "data/vta"
vta_name    = "{{ .VtaName }}"
public_url  = "{{ .PublicURL }}"           # https://{subdomain}.{CLUSTER_DOMAIN}

[services]
rest    = true
didcomm = true

[server]
host = "0.0.0.0"
port = 8100

[log]
level  = "info"
format = "text"

[secrets]                                  # master seed in Vault; pod auth = kubernetes (vta SA)
backend     = "vault"
addr        = "{{ .Vault.Addr }}"
secret_path = "{{ .Vault.SecretPath }}"    # vta/user-<id>/session-<id>/master-seed
kv_mount    = "{{ .Vault.KVMount }}"
secret_key  = "seed"
auth_method = "kubernetes"
k8s_role    = "{{ .Vault.K8sRole }}"       # vta-user-<id>
skip_verify = {{ .Vault.SkipVerify }}

[messaging]                                # vta_only: the shared external mediator
kind = "existing"
did  = "{{ .MediatorDid }}"

[vta_did]
kind               = "create_webvh"
url                = "{{ .VtaDidUrl }}"
portable           = {{ .Portable }}
pre_rotation_count = {{ .PreRotationCount }}
```

> Paths are **relative** (resolved via the Job's `workingDir = /app/vta`), ports/log-level are
> fixed, and the seed lives in **Vault** (not plaintext). Full Stack swaps `[messaging]` for
> `kind = "create_mediator"` and points `[vta_did].url` at the in-cluster dids host — see
> [`full-stack-setup-design.md`](full-stack-setup-design.md).

### Full Stack recipes — see the Full Stack design

`mediator-recipe.toml`, `webvh-recipe.toml` (phases 1 and 3), and `vtc-setup.toml` are
**not** reproduced here. Earlier copies of the first two went stale in exactly the way
this document's Mode B note warns about (they still showed plaintext `[secrets]` backends
and absolute `/data/...` paths, both since replaced by Vault kubernetes auth and
`workingDir`-relative paths). The authoritative renderings live in
[`full-stack-setup-design.md` §7](full-stack-setup-design.md#7-recipe-templates), next to
the state machine and Job specs that consume them.

---

## Output Parsing (Regex)

| Mode | Step | Extracted value | Regex / source |
| --- | --- | --- | --- |
| **vta_only** | `step_vta_setup` | VTA DID (1a) | `(?i)vta did:\s*(did:\S+)` |
| **vta_only** | `step_vta_setup` | VTA DID log | everything after the `---DID_LOG_START---` marker (uploaded to the DID host) |
| full_stack | `step_vta_setup` | Mediator DID (1b) | `(?i)mediator:\s*(did:\S+)` |
| full_stack | `step_mediator_reprov` | SHA-256 digest (2a) | `SHA-256 digest:\s+(\S+)` |
| full_stack | `step_mediator_reprov` | Admin DID (2b) | `Admin DID:\s+(did:\S+)` |
| full_stack | `step_dids_provision` | SHA-256 digest (3a) | `SHA-256 digest:\s+(\S+)` |
| full_stack | `step_dids_p2` | Admin DID (3b) | `Generated admin did:key:\s+(did:\S+)` |
| full_stack | `step_dids_p2` | Daemon DID (3d) | `server_did` from `config.toml` |
| full_stack | `step_vtc_setup_key` | ephemeral setup DID (5a) | `Setup DID \(ephemeral\):\s+(did:\S+)` |
| full_stack | `step_vtc_setup` | VTC DID + install credentials (5b–5e) | `key=value` completion block — one parser pass |
| both | `step_import_admin_did` | PNM Admin DID (4a) | n/a — **user input** (local `pnm setup`), not parsed |

The full list (including the reveal-once admin private keys and the reissue parsers) is in
[`full-stack-setup-design.md` §8](full-stack-setup-design.md#8-output-parsing-regex).

In `vta_only` the **Mediator DID is not parsed** — it is the platform stack's, read from its row
written straight into the config. Only the VTA DID (1a) and its DID log come from the Job
output (`internal/setup/parser.go`).

---

## Data Model: SetupSession

The implemented (`vta_only`) model — `internal/model/setup_session.go`:

```go
type SetupSession struct {
    ID       uint   // internal PK
    UniqueId string // public 8-char id (json "id")
    UserID   uint
    Status   string // state-machine value (see Mode A)
    Mode     string // "vta_only" | "full_stack"
    ErrorMsg string

    // Inputs
    Domain           string // = CLUSTER_DOMAIN
    Subdomain        string // derived "vta-{vta_name}"
    VtaName          string
    VtaImage         string
    Portable         bool   // default true
    PreRotationCount int    // default 1

    // Derived / shared
    MediatorDid string // the platform stack's, snapshotted at create (vta_only)
    VtaDidUrl   string // {DidHostingServerURL}/{vta_name}-vta
    DidHostingServerURL  string // where these DIDs resolve      (json "-")
    DidHostingControlURL string // where the daemon is managed   (json "-")
    CFRecordID  string // single Cloudflare record id (json "-")

    // Outputs
    VtaDid   string // 1a — parsed from `vta setup`
    AdminDid string // 4a — user-supplied PNM admin DID (input, not parsed)

    CreatedAt, UpdatedAt time.Time
}
```

> Full Stack's additive columns (mediator/dids/vtc subdomains + Cloudflare records,
> `mediator_admin_did`, `did_hosting_admin_did`, `vtc_name`, `vtc_did`, the reveal-once
> install credentials, per-component images, etc.) are implemented — see
> [`full-stack-setup-design.md` §10](full-stack-setup-design.md#10-data-model-changes).

---

## API Endpoints

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/api/v1/setup/validate` | Check Cloudflare connectivity + `CLUSTER_INGRESS_IP` (no body) |
| `GET` | `/api/v1/setup/images` | List selectable VTA image tags (from GHCR) |
| `GET` | `/api/v1/setup` | List the caller's sessions (newest first) |
| `POST` | `/api/v1/setup` | Create session + DNS record, start async setup |
| `GET` | `/api/v1/setup/:id` | Poll session state |
| `POST` | `/api/v1/setup/:id/admin` | Phase 2: supply `admin_did` → `vta import-did` + deploy |
| `GET` | `/api/v1/setup/:id/logs` | SSE stream of step output (`?source=setup\|provision\|vta`) |
| `DELETE` | `/api/v1/setup/:id` | Tear down DNS + K8s + Vault seed, delete session |

`:id` is the session's `vta_name`, **not** the numeric PK. There is no opaque id:
the name addresses the session and is what a delete confirmation asks for, which
is why it is globally unique rather than unique only among managed sessions.

### GET /api/v1/setup/:id — response shape

```json
{
  "id": "ab12cd34",
  "status": "running",
  "mode": "vta_only",
  "url": "https://vta-personal-vta.example.com",
  "vta_image": "ghcr.io/ic3software/vta:0.9.0",
  "vta_did": "did:webvh:...:dids.example.com:ab12cd34:personal-vta",
  "created_at": "2026-06-03T10:00:00Z",
  "updated_at": "2026-06-03T10:05:00Z"
}
```

`error_msg` is included only when `status = "failed"`. The live URL is reachable once
`status = "running"`. The list endpoint `GET /setup` additionally returns `vta_name`,
`mediator_did`, and `vta_did_url` per session.

---

## DID hosting credentials

Two different things are easy to confuse, and mixing them up leads to the wrong
design:

| | Whose identity | Where it lives |
| --- | --- | --- |
| `did_hosting_admin_did` (3b) + `webvh_admin_key` (3c) | the **daemon's** own bootstrap admin, handed to a human for offline backup | the session row |
| `did_hosting_did` (3d) | the **daemon's** own DID | the session row (and the control client fetches it from `/api/server-info` anyway) |
| `DID_HOSTING_DID` + `DID_HOSTING_PRIVATE_KEY` | **vtafarm-api's own**, from `make gen-keypair`, enrolled in a daemon's ACL with `role=admin` | configuration only |

Because the last one is ours and not something a daemon issued, **one keypair
serves every daemon it is enrolled in**. That is why the URLs moved onto the
session row while the keypair stayed in configuration: which daemon to talk to
varies per session, the identity we talk with does not.

It also means rebuilding the platform stack does **not** invalidate the keypair
— it produces a fresh daemon with an empty ACL, so the same keypair has to be
enrolled again. **The pipeline does that itself**: `step_dids_grant_farm` runs
`did-hosting-daemon add-acl --did <DID_HOSTING_DID> --role admin --label vtafarm`
as an offline Job on the dids PVC, in the same window as `step_dids_invite` and
`step_dids_load_did` — after the store exists and before any daemon pod holds
it.

Offline is not a convenience: the control API authenticates callers *from* the
ACL, so enrolling over HTTP would require already being enrolled. Writing the
store directly is the only way in.

The step runs for **every** `full_stack` session, not just the platform stack:
the farm operates these deployments and needs to manage the `did.jsonl`
documents they serve. `GET /admin/platform-stack` reports the platform stack's
own result under `farm_acl`; `granted: false` means no keypair was configured
when it was built, which is the one case still needing a human.

### Open: a user-supplied DID host

Once a user can point a session at a DID-hosting service of their own, uploading
its DID log requires vtafarm-api to be an admin in **their** daemon's ACL. Two
shapes, neither chosen:

1. **Publish our client DID and have the user enroll it.** No new secrets, and
   the same keypair everywhere. But it hands one identity admin rights across
   every user's daemon, so a compromise is not contained, and it puts a manual
   enrollment step in a user-facing flow.
2. **Mint a keypair per session (or per user) and enroll that.** Contained by
   construction and revocable per session. Costs a private key per session,
   which belongs in Vault next to the master seed rather than in a column — the
   same rule the rest of the design already follows.

The schema does not prejudge it: `did_hosting_control_url` is already per
session, so only the credential lookup changes. Decide when the feature is
actually built.

---

## Validation Rules

`POST /api/v1/setup/validate` is a server-side pre-flight — it takes **no form fields**:

1. Cloudflare is configured (`503` if not) and `VerifyZone` succeeds (`502` on failure)
2. `CLUSTER_INGRESS_IP` is non-empty (`422` if not)

`POST /api/v1/setup` then validates the body:

1. `mode`: required, `vta_only` | `full_stack` (`full_stack` additionally requires the
   caller's `beta_access`)
2. `vta_image`: required; `full_stack` also requires `mediator_image`, `dids_image`, and
   `vtc_image`
3. `vta_name`: must be unique for this user (`409` on conflict; defaults to `personal-vta`)
4. `vtc_name` (`full_stack`): must be unique across **all** sessions (`409` on conflict;
   defaults to `personal-vtc`)
5. names must be DNS-safe per `setup.ValidateName` — lowercase letters/digits joined by
   single hyphens, ≤48 chars
6. cluster config: `CLUSTER_INGRESS_IP` **and** `CLUSTER_DOMAIN` must be set (`422` if not)

---

## New File Layout

```text
internal/
├── config/config.go          + CloudflareAPIToken, CloudflareZoneID, ClusterIngressIP
├── cloudflare/
│   └── client.go             CreateARecord, DeleteRecord, ListRecords
├── handler/
│   └── setup.go              Validate, Images, Create, List, Get, Delete, Logs, ProvisionAdmin
├── model/
│   └── setup_session.go      SetupSession GORM model
├── k8s/
│   ├── setup_jobs.go         LaunchSetupJob, WatchJob, ReadJobLogs
│   └── vta_resources.go      CreatePVC, CreateService, CreateIngress, CreateDeployment, TeardownAll
├── setup/
│   ├── orchestrator.go       goroutine per session, drives state machine
│   ├── parser.go             regex extractors for each step's stdout
│   └── templates.go          Go text/template renderers for VTA/Mediator/DIDS TOMLs
└── router/router.go          + setup routes
migrations/
└── 000002_add_setup_sessions.up.sql
```

---

## Implementation Steps (Ordered)

1. **Config** — add `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ZONE_ID`, `CLUSTER_INGRESS_IP` to `internal/config/config.go`
2. **Migration** — `000002_add_setup_sessions.up.sql` + down
3. **Model** — `internal/model/setup_session.go`
4. **Cloudflare client** — `internal/cloudflare/client.go` (CreateARecord, DeleteRecord)
5. **TOML templates** — `internal/setup/templates.go` (VTA + Mediator + DIDS, both modes)
6. **Output parser** — `internal/setup/parser.go` (regex per step)
7. **K8s resources** — `internal/k8s/vta_resources.go` (PVC, Service, Ingress, Deployment, teardown)
8. **K8s job launcher** — `internal/k8s/setup_jobs.go` (ConfigMap + Job + watch + log read)
9. **Orchestrator** — `internal/setup/orchestrator.go` (goroutine per session, full state machine for both modes)
10. **Handler** — `internal/handler/setup.go` (6 endpoints)
11. **Router** — wire new routes in `internal/router/router.go`
12. **Helm values** — add `cloudflare.*` and `clusterIngressIP` to `helm/vtafarm-api/values.yaml`
13. **ClusterRole** — add `persistentvolumeclaims`, `deployments`, `ingresses` to Helm ClusterRole template
