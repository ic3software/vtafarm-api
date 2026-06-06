# CipherPortal API

Go REST API backend for managing VTA setup sessions with per-user namespace isolation.

## Tech Stack

| Layer | Choice |
|---|---|
| HTTP | Gin (`github.com/gin-gonic/gin`) |
| ORM | GORM + `gorm.io/driver/postgres` |
| Migrations | golang-migrate (`file://migrations`, raw SQL) |
| Database | PostgreSQL 18 |
| K8s client | `k8s.io/client-go` v0.36 |
| Hot reload | Air (`github.com/air-verse/air`) |
| Container | Docker Compose (dev) + multi-stage Dockerfile (prod) |

## Quick Start

```bash
cp .env.example .env
make dev                  # start DB (Docker) + API with Air hot-reload; migrations run automatically
make seed                 # (optional) insert test data — run in a separate terminal
```

API: `http://localhost:8080`
Docs (local only): `http://localhost:8080/docs`

## Environment Variables

See `.env.example` for all options. Key ones:

| Variable | Default | Notes |
|---|---|---|
| `APP_PORT` | `8080` | HTTP listen port |
| `APP_ENV` | `development` | Set to `production` to disable `/docs` |
| `DB_HOST` | `localhost` | Overridden to `db` in docker-compose |
| `DB_NAME` | `cipherportal` | |
| `JWT_SECRET` | `change-me-in-production` | HS256 signing secret |
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
├── .air.toml                   # Air hot-reload config
├── migrations/
│   ├── 000001_init.up.sql
│   └── 000001_init.down.sql
├── docs/
│   ├── automated-setup.md      # VTA stack CLI reference
│   └── vta-setup-design.md     # API design for VTA setup automation
└── internal/
    ├── apidocs/
    │   ├── openapi.yaml        # OpenAPI 3.1 spec — update whenever routes change
    │   └── apidocs.go          # Embeds spec; serves GET /openapi.yaml and GET /docs
    ├── config/config.go        # Config struct loaded from env vars
    ├── database/database.go    # DB connect + migrate on startup
    ├── cloudflare/client.go    # Cloudflare API: CreateARecord, DeleteRecord
    ├── model/
    │   ├── admin.go
    │   ├── user.go
    │   └── setup_session.go    # GORM model for VTA setup sessions
    ├── handler/
    │   ├── health.go
    │   ├── auth.go             # AdminLogin, UserLogin
    │   ├── user.go             # Create user (admin only)
    │   └── setup.go            # VTA setup wizard endpoints
    ├── middleware/
    │   └── auth.go             # JWT auth + role enforcement
    ├── k8s/
    │   ├── client.go           # K8s client: in-cluster + kubeconfig support
    │   ├── setup_jobs.go       # Launch/watch K8s Jobs for setup commands
    │   └── vta_resources.go    # Create/teardown PVC, Service, Ingress, Deployment
    ├── setup/
    │   ├── orchestrator.go     # State machine, goroutine per session
    │   ├── parser.go           # Regex extractors for DID/digest values from job logs
    │   └── templates.go        # Go text/template renderers for VTA/Mediator/DIDS configs
    └── router/router.go        # Gin route registration
```

## Migration Workflow

```bash
make migrate-new NAME=create_users   # create a new migration pair
make migrate                          # apply pending migrations
make migrate-down                     # roll back one step
```

Migrations run automatically on every API startup. `ErrNoChange` is silently ignored.

## REST API Endpoints

All API routes are prefixed with `/api/v1`.
Local docs: `http://localhost:8080/docs` (disabled when `APP_ENV=production`).

### Auth

| Method | Path | Role | Description |
|---|---|---|---|
| `POST` | `/api/v1/auth/admin/login` | public | Admin login → JWT |
| `POST` | `/api/v1/auth/user/login` | public | User login → JWT |

### Admin

| Method | Path | Role | Description |
|---|---|---|---|
| `POST` | `/api/v1/users` | admin | Create a user account |

### User — VTA Setup Wizard

| Method | Path | Role | Description |
|---|---|---|---|
| `POST` | `/api/v1/setup/validate` | user | Check Cloudflare connectivity |
| `POST` | `/api/v1/setup` | user | Create session + provision DNS |
| `DELETE` | `/api/v1/setup/:id` | user | Cancel session + tear down DNS |

## API Docs Rule

**Every new route must be documented in `internal/apidocs/openapi.yaml`.**

Assign the correct tag so it appears in the right group in Scalar:
- `System` — health / system routes
- `Auth — Admin` / `Auth — User` — login routes
- `Admin` — routes requiring admin JWT
- `User` — routes requiring user JWT

## Kubernetes Design

### Per-User Namespace Isolation

Every user gets their own namespace: `cp-user-{userID}`.

`EnsureUserEnvironment` creates on first use (idempotent):
1. **Namespace** labelled `managed-by=cipherportal`
2. **ServiceAccount** `pod-operator`
3. **Role** `pod-manager` — grants `pods`, `pods/log`, `pods/exec`
4. **RoleBinding** — binds the SA to the Role

### API Server Permissions (Production)

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
