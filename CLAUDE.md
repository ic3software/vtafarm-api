# CipherPortal API

Go REST API backend for managing Kubernetes pod deployments with per-user namespace isolation.

## Tech Stack

| Layer | Choice |
|---|---|
| HTTP | Gin (`github.com/gin-gonic/gin`) |
| ORM | GORM + `gorm.io/driver/postgres` |
| Migrations | golang-migrate (`file://migrations`, raw SQL) |
| Database | PostgreSQL 16 |
| K8s client | `k8s.io/client-go` v0.36 |
| Hot reload | Air (`github.com/air-verse/air`) |
| Container | Docker Compose (dev) + multi-stage Dockerfile (prod) |

## Quick Start

```bash
cp .env.example .env
make up                       # start DB + API (Air hot-reload inside container)
make migrate                  # run migrations
make seed                     # insert test data
```

## Environment Variables

See `.env.example` for all options. Key ones:

| Variable | Default | Notes |
|---|---|---|
| `APP_PORT` | `8080` | HTTP listen port |
| `DB_HOST` | `localhost` | Overridden to `db` in docker-compose |
| `DB_NAME` | `cipherportal` | |
| `KUBECONFIG` | `~/.kube/config` | Leave empty; auto-detected |
| `K8S_NAMESPACE_PREFIX` | `cp-user` | Per-user namespace prefix |
| `CLOUDFLARE_API_TOKEN` | — | Cloudflare API token (`Zone:DNS:Edit` permission) |
| `CLOUDFLARE_ZONE_ID` | — | Cloudflare Zone ID for the user's root domain |
| `CLUSTER_INGRESS_IP` | — | External IP of the cluster's Ingress-NGINX LoadBalancer |

## Project Structure

```
.
├── main.go                     # Entry point: config → DB → migrate → K8s → router
├── cmd/migrate/main.go         # Migration CLI (up / down / drop)
├── seed/main.go                # Test data seeder
├── migrations/
│   ├── 000001_init.up.sql
│   ├── 000001_init.down.sql
│   ├── 000002_add_setup_sessions.up.sql   # VTA setup sessions table
│   └── 000002_add_setup_sessions.down.sql
├── docs/
│   ├── automated-setup.md      # VTA stack CLI reference (source of truth for commands)
│   └── vta-setup-design.md     # API design for VTA setup automation
└── internal/
    ├── config/config.go        # Config struct loaded from env vars (incl. Cloudflare + ingress IP)
    ├── database/database.go    # DB connect + auto-migrate on startup
    ├── cloudflare/client.go    # Cloudflare API: CreateARecord, DeleteRecord
    ├── model/
    │   ├── pod_deployment.go   # GORM model for pod records
    │   └── setup_session.go    # GORM model for VTA setup sessions
    ├── handler/
    │   ├── health.go
    │   ├── pod.go              # CRUD handlers for pod deployments
    │   └── setup.go            # VTA setup wizard endpoints (6 routes)
    ├── k8s/
    │   ├── client.go           # K8s client: in-cluster + kubeconfig support
    │   ├── setup_jobs.go       # Launch/watch K8s Jobs for setup commands
    │   └── vta_resources.go    # Create/teardown PVC, Service, Ingress, Deployment per component
    ├── setup/
    │   ├── orchestrator.go     # State machine (both modes), goroutine per session
    │   ├── parser.go           # Regex extractors for DID/digest values from job logs
    │   └── templates.go        # Go text/template renderers for VTA/Mediator/DIDS TOML configs
    └── router/router.go        # Gin route registration
```

## Migration Workflow

Migrations live in `migrations/` as numbered SQL files:

```
000001_init.up.sql / 000001_init.down.sql
000002_<name>.up.sql / 000002_<name>.down.sql
```

```bash
# Create a new migration pair
make migrate-new NAME=create_users

# Apply all pending migrations
make migrate

# Roll back one step
make migrate-down
```

Migrations run automatically on every API startup. `ErrNoChange` is silently ignored.

## REST API Endpoints

All API routes are prefixed with `/api/v1`.

### Pod Management

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Liveness check |
| `POST` | `/api/v1/pods` | Create a pod from YAML in user namespace |
| `GET` | `/api/v1/pods?user_id=` | List pods in user namespace |
| `GET` | `/api/v1/pods/:name?user_id=` | Get pod status |
| `DELETE` | `/api/v1/pods/:name?user_id=` | Delete a pod |

### VTA Setup Wizard

Automates VTA stack installation driven by a frontend form. Two modes; see [`docs/vta-setup-design.md`](docs/vta-setup-design.md) for full design.

| Method | Path | Description |
|---|---|---|
| `POST` | `/api/v1/setup/validate` | Validate form fields + check Cloudflare connectivity |
| `POST` | `/api/v1/setup` | Submit form, create session, begin async setup |
| `GET` | `/api/v1/setup/:id` | Poll state, collected DIDs/digests, live service URLs |
| `POST` | `/api/v1/setup/:id/advance` | Unblock manual gate (provide `dids_digest` for Step 3) |
| `GET` | `/api/v1/setup/:id/logs` | SSE stream of raw step output |
| `DELETE` | `/api/v1/setup/:id` | Cancel + tear down DNS records and K8s resources |

**Setup modes:**

| Mode | Description |
|---|---|
| `vta_only` | Deploys VTA only; user provides external DID hosting URL |
| `full_stack` | Deploys VTA + Mediator + WebVH DID Hosting Daemon in-cluster |

**Form fields accepted by `POST /api/v1/setup`:**

| Field | Mode | Required | Default |
|---|---|---|---|
| `mode` | both | yes | — |
| `domain` | both | yes | — |
| `did_hosting_url` | vta_only | yes | — |
| `vta_name` | both | yes | `personal-vta` |
| `vta_port` | both | no | `8100` |
| `mediator_port` | full_stack | no | `7037` |
| `dids_port` | full_stack | no | `8534` |
| `log_level` | both | no | `info` |

**What setup provisions automatically:**

1. Cloudflare DNS A records (`vta.{domain}` → cluster ingress IP; +2 more for full_stack)
2. K8s PVCs, Services, Ingresses (with cert-manager TLS) per component
3. K8s Jobs for one-off setup commands, parsing DIDs/digests from stdout
4. K8s Deployments for the running services (after all Jobs succeed)

### Create Pod Example

```bash
curl -X POST http://localhost:8080/api/v1/pods \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": "user-001",
    "yaml": "apiVersion: v1\nkind: Pod\nmetadata:\n  name: my-nginx\nspec:\n  containers:\n  - name: nginx\n    image: nginx:alpine"
  }'
```

> **Note:** `user_id` is currently passed in the request body. This is a placeholder —
> production auth middleware will extract it from a JWT/session token.

## Kubernetes Design

### Per-User Namespace Isolation

Every user gets their own namespace: `cp-user-{userID}`.

`EnsureUserEnvironment` creates on first use (idempotent):
1. **Namespace** labelled `managed-by=cipherportal`
2. **ServiceAccount** `pod-operator` — the identity under which user's pods run
3. **Role** `pod-manager` — grants `pods`, `pods/log`, `pods/exec` within the namespace
4. **RoleBinding** — binds the SA to the Role

### API Server Permissions

For development, the API uses your local `~/.kube/config` (full admin access).

For production (in-cluster), the API server pod needs a **ClusterRole** with:
```yaml
rules:
- apiGroups: [""]
  resources: ["namespaces", "serviceaccounts", "pods", "pods/log", "pods/exec",
              "configmaps", "persistentvolumeclaims", "services"]
  verbs: ["get", "list", "create", "update", "delete", "watch"]
- apiGroups: ["rbac.authorization.k8s.io"]
  resources: ["roles", "rolebindings"]
  verbs: ["get", "list", "create", "delete"]
- apiGroups: ["batch"]
  resources: ["jobs"]
  verbs: ["get", "list", "create", "delete", "watch"]
- apiGroups: ["apps"]
  resources: ["deployments"]
  verbs: ["get", "list", "create", "update", "delete", "watch"]
- apiGroups: ["networking.k8s.io"]
  resources: ["ingresses"]
  verbs: ["get", "list", "create", "update", "delete", "watch"]
```

The VTA setup wizard provisions PVCs, Services, Ingresses, Jobs, and Deployments inside the user's namespace — all covered above.

### Pod Terminal Access (Future)

Connecting to a pod terminal requires proxying a WebSocket connection to the K8s
API `pods/exec` subresource. This will be implemented as a WebSocket endpoint
(`GET /api/v1/pods/:name/exec?user_id=`) using `k8s.io/client-go/tools/remotecommand`.
The per-user Role already grants `pods/exec` — no RBAC changes needed.

## Adding a New Migration

```bash
make migrate-new NAME=add_users_table
# → creates migrations/000002_add_users_table.{up,down}.sql
# Edit the files, then:
make migrate-up
```

## Docker

```bash
# Dev (hot reload via Air)
make docker-up
make docker-down
make docker-reset          # wipe volumes and restart

# Production image (multi-stage, minimal alpine)
docker build -t cipherportal-api .
```
