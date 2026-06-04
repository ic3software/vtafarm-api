# VTA Setup API — Design

Automates VTA stack installation driven by a frontend form. Supports two modes:

- **VTA Only** — deploys just the VTA service; user provides an existing external DID hosting URL.
- **Full Stack** — deploys VTA + DIDComm Mediator + WebVH DID Hosting Daemon; all three are hosted in-cluster.

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

User provides their own DID hosting endpoint. CipherPortal deploys only the VTA service.

```
User provides:
  domain            → creates  vta.{domain}   (DNS + Ingress + TLS)
  did_hosting_url   → e.g. https://dids.example.com/vta  (external WebVH host)

VTA TOML uses:
  [vta_did]
  kind = "create_webvh"
  url  = "{did_hosting_url}"

  [messaging]
  kind = "skip"               ← no in-cluster mediator
```

**State machine (VTA Only):**
```
pending → dns_provision → k8s_provision → step_vta → deploy → completed
                                                            ↓ (any step)
                                                         failed
```

### Mode B — Full Stack

CipherPortal deploys and wires all three components.

```
User provides:
  domain  → creates  vta.{domain}       (VTA)
                     mediator.{domain}  (DIDComm Mediator)
                     dids.{domain}      (WebVH DID Hosting Daemon)
```

**State machine (Full Stack):**
```
pending
  → dns_provision          create 3 Cloudflare DNS records
  → k8s_provision          create PVCs, Services, Ingresses for all 3 components
  → step_vta               vta setup --from vta-setup.toml
  → step_mediator_p1       mediator-setup --from mediator-recipe.toml (phase 1)
  → step_mediator_vta      vta contexts reprovision … --out bundle.armor
  → step_mediator_p2       mediator-setup --from … --bundle … --digest … (phase 2)
  → step_dids_p1           did-hosting-daemon setup --from webvh-recipe.toml (phase 1)
  → step_dids_vta_admin    ← MANUAL GATE: operator runs vta bootstrap provision-integration
  → step_dids_p2           did-hosting-daemon setup (offline-complete)
  → step_pnm               pnm setup + vta import-did + pnm setup continue
  → deploy                 create Deployments for all 3 components
  → completed
         ↓ (any step)
      failed
```

`step_dids_vta_admin` is the only manual gate. The operator provides the SHA-256 digest via `POST /setup/:id/advance`.

---

## Frontend Form Fields

| Field | Mode | Required | Default | Validation |
|---|---|---|---|---|
| `mode` | both | yes | — | `vta_only` \| `full_stack` |
| `domain` | both | yes | — | valid FQDN, no `https://` prefix, no trailing slash |
| `did_hosting_url` | vta_only | yes | — | valid HTTPS URL ending with DID path segment |
| `vta_name` | both | yes | `personal-vta` | `[a-z0-9-]`, 1–64 chars |
| `vta_port` | both | no | `8100` | 1024–65535 |
| `mediator_port` | full_stack | no | `7037` | 1024–65535, unique |
| `dids_port` | full_stack | no | `8534` | 1024–65535, unique |
| `log_level` | both | no | `info` | `info` \| `debug` \| `warn` \| `error` |

**Derived subdomain URLs** (backend computes):

| Subdomain | Pattern | Used for |
|---|---|---|
| `vta.{domain}` | `https://vta.{domain}` | VTA public URL + VTA REST endpoint |
| `mediator.{domain}` | `https://mediator.{domain}/mediator/v1` | Mediator DIDComm URL *(full_stack only)* |
| `dids.{domain}` | `https://dids.{domain}` | WebVH DID hosting base URL *(full_stack only)* |

---

## Cloudflare DNS Integration

The backend calls the Cloudflare API to create DNS records **before** running any setup Jobs. This is required because setup TOML configs reference the final HTTPS URLs, which must resolve for DID document publishing.

### Config (env vars)

| Variable | Notes |
|---|---|
| `CLOUDFLARE_API_TOKEN` | Cloudflare API token with `Zone:DNS:Edit` permission |
| `CLOUDFLARE_ZONE_ID` | Zone ID for the user's root domain (from Cloudflare dashboard) |
| `CLUSTER_INGRESS_IP` | External IP of the cluster's Nginx/Ingress-NGINX LoadBalancer |

### DNS Records Created

For **VTA Only** mode:
```
A   vta.{domain}  →  {CLUSTER_INGRESS_IP}   proxied=true
```

For **Full Stack** mode:
```
A   vta.{domain}       →  {CLUSTER_INGRESS_IP}   proxied=true
A   mediator.{domain}  →  {CLUSTER_INGRESS_IP}   proxied=true
A   dids.{domain}      →  {CLUSTER_INGRESS_IP}   proxied=true
```

### Cloudflare Client — `internal/cloudflare/client.go`

```go
type Client struct {
    apiToken string
    zoneID   string
}

func (c *Client) CreateARecord(ctx context.Context, name, ip string) error
func (c *Client) DeleteRecord(ctx context.Context, name string) error
func (c *Client) ListRecords(ctx context.Context, name string) ([]DNSRecord, error)
```

---

## Kubernetes Resource Provisioning

All resources are created in the user's isolated namespace (`cp-user-{userID}`) before setup Jobs run. Ingress resources rely on cert-manager (already in the cluster) for automatic TLS.

### Resources per component

| Resource | Name pattern | Purpose |
|---|---|---|
| `PersistentVolumeClaim` | `{component}-data` | Persistent storage for config + data dir |
| `Service` | `{component}` | ClusterIP, exposes component port |
| `Ingress` | `{component}` | nginx ingress with cert-manager TLS annotation |
| `Job` (setup) | `{component}-setup-{sessionID}` | Runs one-off setup command |
| `Deployment` (server) | `{component}` | Long-running server, starts after setup Jobs complete |

### Ingress template (per component)

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {component}
  namespace: cp-user-{userID}
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
spec:
  ingressClassName: nginx
  tls:
    - hosts: [{subdomain}.{domain}]
      secretName: {component}-tls
  rules:
    - host: {subdomain}.{domain}
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: {component}
                port:
                  number: {port}
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
      name: {component}-setup-config-{sessionID}
  - name: data
    persistentVolumeClaim:
      claimName: {component}-data
containers:
  - name: setup
    image: {component_image}
    command: ["{binary}", "setup", "--from", "/config/recipe.toml"]
    volumeMounts:
      - name: config
        mountPath: /config
      - name: data
        mountPath: /data
```

### Server phase (persistent Deployments)

After all setup Jobs succeed, the backend creates Deployments for the running servers:

```yaml
containers:
  - name: {component}
    image: {component_image}
    command: ["{binary}", "--config", "/data/config.toml"]
    volumeMounts:
      - name: data
        mountPath: /data
    ports:
      - containerPort: {port}
```

The Deployment mounts the same PVC as the setup Job, so `config.toml` and `data/` are already present.

---

## TOML Templates

### vta-setup.toml (both modes)

```toml
config_path = "/data/config.toml"
data_dir    = "/data/vta"
vta_name    = "{{.VTAName}}"
public_url  = "https://vta.{{.Domain}}"

[services]
rest    = true
didcomm = {{if eq .Mode "full_stack"}}true{{else}}false{{end}}

[server]
host = "0.0.0.0"
port = {{.VTAPort}}

[log]
level  = "{{.LogLevel}}"
format = "text"

[secrets]
backend = "plaintext"

{{if eq .Mode "full_stack"}}
[messaging]
kind      = "create_mediator"
context   = "mediator"
url       = "https://mediator.{{.Domain}}/mediator/v1"
webvh_url = "https://dids.{{.Domain}}/mediator"
{{else}}
[messaging]
kind = "skip"
{{end}}

[vta_did]
kind               = "create_webvh"
{{if eq .Mode "full_stack"}}
url                = "https://dids.{{.Domain}}/vta"
{{else}}
url                = "{{.DIDHostingURL}}"
{{end}}
portable           = true
pre_rotation_count = 1
```

### mediator-recipe.toml (full_stack only)

```toml
[deployment]
type      = "server"
protocols = ["didcomm"]
use_vta   = true
vta_mode  = "sealed-export"

[vta]
context = "mediator"

[secrets]
storage = "file:///data/conf/secrets.json"

[security]
ssl          = "none"
admin        = "generate"
jwt_mode     = "generate"
network_mode = "open"

[database]
url = "redis://127.0.0.1/"

[storage]
backend  = "fjall"
data_dir = "/data/mediator"

[output]
config_path    = "/data/conf/mediator.toml"
listen_address = "0.0.0.0:{{.MediatorPort}}"
```

### webvh-recipe.toml phase 1 (full_stack only)

```toml
[deployment]
service  = "daemon"
vta_mode = "offline-prepare"

[output]
config_path = "/data/config.toml"

[server]
host       = "0.0.0.0"
port       = {{.DIDsPort}}
log_level  = "{{.LogLevel}}"
log_format = "text"
data_dir   = "/data/daemon"

[identity]
public_url   = "https://dids.{{.Domain}}"
mediator_did = "{{.MediatorDID}}"

[vta]
request_path = "/data/bootstrap-request.json"

[daemon]
enable_control  = true
enable_server   = true
enable_witness  = true
enable_watcher  = false

[secrets]
backend           = "plaintext"
confirm_plaintext = true

[admin]
mode = "generate"

[reprovision]
force = false
```

---

## Output Parsing (Regex)

| Step | Extracted value | Regex |
|---|---|---|
| `step_vta` | VTA DID (1a) | `VTA DID:\s+(did:\S+)` |
| `step_vta` | Mediator DID (1b) | `Mediator:\s+(did:\S+)` |
| `step_mediator_vta` | SHA-256 digest (2a) | `SHA-256 digest:\s+(\S+)` |
| `step_mediator_vta` | Admin DID (2b) | `Admin DID:\s+(did:\S+)` |
| `step_dids_vta_admin` | SHA-256 digest (3a) | `SHA-256 digest:\s+(\S+)` |
| `step_dids_p2` | Admin DID (3b) | `Generated admin did:key:\s+(did:\S+)` |
| `step_dids_p2` | Daemon DID (3d) | `grep '^server_did' /data/config.toml` |
| `step_pnm` | PNM Admin DID (4a) | `Admin DID:\s+(did:\S+)` |

---

## Data Model: SetupSession

```go
type SetupSession struct {
    ID        uint   `gorm:"primaryKey"`
    UserID    uint   `gorm:"not null;index"`
    Status    string // state machine value
    ErrorMsg  string

    // Form inputs
    Mode         string // "vta_only" | "full_stack"
    Domain       string
    DIDHostingURL string // vta_only mode only
    VTAName      string
    VTAPort      int
    MediatorPort int    // full_stack only
    DIDsPort     int    // full_stack only
    LogLevel     string

    // DNS record IDs (for cleanup on failure)
    CFRecordVTA      string
    CFRecordMediator string
    CFRecordDIDs     string

    // Collected outputs
    VTADID           string // 1a
    MediatorDID      string // 1b — full_stack only
    MediatorDigest   string // 2a — full_stack only
    MediatorAdminDID string // 2b — full_stack only
    DIDsDigest       string // 3a — full_stack only
    DIDsAdminDID     string // 3b — full_stack only
    DIDsDID          string // 3d — full_stack only
    PNMAdminDID      string // 4a — full_stack only

    CreatedAt time.Time
    UpdatedAt time.Time
}
```

---

## API Endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/api/v1/setup/validate` | Validate form fields, check Cloudflare connectivity |
| `POST` | `/api/v1/setup` | Submit form, create session, start async setup |
| `GET` | `/api/v1/setup/:id` | Poll session state, collected values, service URLs |
| `POST` | `/api/v1/setup/:id/advance` | Unblock `step_dids_vta_admin` gate (provide `dids_digest`) |
| `GET` | `/api/v1/setup/:id/logs` | SSE stream of raw step output |
| `DELETE` | `/api/v1/setup/:id` | Cancel setup + tear down DNS records and K8s resources |

### GET /api/v1/setup/:id — response shape

```json
{
  "id": 42,
  "mode": "full_stack",
  "status": "step_mediator_p2",
  "error_message": "",
  "config": {
    "domain": "example.com",
    "vta_name": "personal-vta",
    "vta_port": 8100,
    "mediator_port": 7037,
    "dids_port": 8534
  },
  "urls": {
    "vta": "https://vta.example.com",
    "mediator": "https://mediator.example.com",
    "dids": "https://dids.example.com"
  },
  "collected": {
    "vta_did": "did:webvh:...:dids.example.com:vta",
    "mediator_did": "did:webvh:...:dids.example.com:mediator",
    "mediator_digest": "abc123...",
    "mediator_admin_did": "did:key:z6Mk...",
    "dids_digest": "",
    "dids_admin_did": "",
    "dids_did": "",
    "pnm_admin_did": ""
  },
  "updated_at": "2026-06-03T10:05:00Z"
}
```

When `status = "completed"`, the `urls` block contains the live endpoints the user can connect to.

---

## Validation Rules

`POST /api/v1/setup/validate` runs these checks before creating a session:

1. `mode`: must be `vta_only` or `full_stack`
2. `domain`: matches FQDN regex; must not start with `https://`; Cloudflare zone lookup confirms zone ownership
3. `did_hosting_url` *(vta_only)*: must start with `https://`; must not be empty
4. `vta_name`: matches `^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$`
5. `vta_port`, `mediator_port`, `dids_port`: each in `[1024, 65535]`; all three must be distinct
6. `log_level`: one of `info`, `debug`, `warn`, `error`
7. Cloudflare: `CLUSTER_INGRESS_IP` env var must be non-empty (server-side check, not a form field)

---

## New File Layout

```
internal/
├── config/config.go          + CloudflareAPIToken, CloudflareZoneID, ClusterIngressIP
├── cloudflare/
│   └── client.go             CreateARecord, DeleteRecord, ListRecords
├── handler/
│   └── setup.go              ValidateSetup, CreateSetup, GetSetup, AdvanceSetup, SetupLogs, DeleteSetup
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
12. **Helm values** — add `cloudflare.*` and `clusterIngressIP` to `helm/cipherportal-api/values.yaml`
13. **ClusterRole** — add `persistentvolumeclaims`, `deployments`, `ingresses` to Helm ClusterRole template
