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

## Project Structure

```
.
├── main.go                     # Entry point: config → DB → migrate → K8s → router
├── cmd/migrate/main.go         # Migration CLI (up / down / drop)
├── seed/main.go                # Test data seeder
├── migrations/
│   ├── 000001_init.up.sql
│   └── 000001_init.down.sql
└── internal/
    ├── config/config.go        # Config struct loaded from env vars
    ├── database/database.go    # DB connect + auto-migrate on startup
    ├── model/pod_deployment.go # GORM model for pod records
    ├── handler/
    │   ├── health.go
    │   └── pod.go              # CRUD handlers for pod deployments
    ├── k8s/client.go           # K8s client: in-cluster + kubeconfig support
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

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Liveness check |
| `POST` | `/api/v1/pods` | Create a pod from YAML in user namespace |
| `GET` | `/api/v1/pods?user_id=` | List pods in user namespace |
| `GET` | `/api/v1/pods/:name?user_id=` | Get pod status |
| `DELETE` | `/api/v1/pods/:name?user_id=` | Delete a pod |

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
  resources: ["namespaces", "serviceaccounts", "pods", "pods/log", "pods/exec"]
  verbs: ["get", "list", "create", "delete", "watch"]
- apiGroups: ["rbac.authorization.k8s.io"]
  resources: ["roles", "rolebindings"]
  verbs: ["get", "list", "create", "delete"]
```

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
