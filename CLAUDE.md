# VTA Farm API

Go REST API backend for managing VTA setup sessions with per-user namespace isolation.

## Tech Stack

| Layer | Choice |
| --- | --- |
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
make enroll               # create first admin + print 24h enrollment token (run in a separate terminal)
```

API: `http://localhost:8080`
Docs (local only): `http://localhost:8080/docs`

## Environment Variables

See `.env.example` for all options. Key ones:

| Variable | Default | Notes |
| --- | --- | --- |
| `APP_PORT` | `8080` | HTTP listen port |
| `APP_ENV` | `development` | Set to `production` to disable `/docs` |
| `DB_HOST` | `localhost` | Overridden to `db` in docker-compose |
| `DB_NAME` | `vtafarm` | |
| `JWT_SECRET` | `change-me-in-production` | HS256 signing secret |
| `KUBECONFIG` | `~/.kube/config` | Leave empty; auto-detected |
| `K8S_NAMESPACE_PREFIX` | `vtafarm-user` | Per-user namespace prefix |
| `CLOUDFLARE_API_TOKEN` | — | Cloudflare API token (`Zone:DNS:Edit` permission) |
| `CLOUDFLARE_ZONE_ID` | — | Cloudflare Zone ID for the user's root domain |
| `CLUSTER_INGRESS_IP` | — | External IP of the cluster's Ingress-NGINX LoadBalancer |
| `RESEND_API_KEY` | — | Resend API key — email sending disabled when unset |
| `RESEND_FROM` | — | Sender address, e.g. `VTA Farm <noreply@example.com>` (domain must be verified in Resend) |
| `PUBLIC_BASE_URL` | first `WEBAUTHN_RP_ORIGINS` entry | Frontend origin for links in emails |

## Project Structure

```text
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

## Admin Enrollment

All accounts (admin and user) use **passkey-only** authentication. There are no passwords.

To create the first admin:

```bash
make enroll
# prints: ID, UniqueID, Token, and the enrollment URL
```

The token is valid for 24 hours. Pass it to the frontend enrollment page, or call the API directly:

```bash
# 1. Consume the token — returns vtafarm_admin cookie + JWT
POST /api/v1/admin/enroll/<token>

# 2. Register a passkey (use the returned JWT as Bearer token)
POST /api/v1/admin/passkeys/register/begin
POST /api/v1/admin/passkeys/register/complete?name=MyKey
```

To create additional admins, an authenticated admin calls `POST /api/v1/admin/admins`, which returns a new enrollment token.

### Auth

| Method | Path | Role | Description |
| --- | --- | --- | --- |
| `POST` | `/api/v1/auth/passkey/begin` | public | Begin passkey login (admin or user) |
| `POST` | `/api/v1/auth/passkey/complete` | public | Complete passkey login → JWT + cookie |
| `GET` | `/api/v1/admin/enroll/:token` | public | Validate admin enrollment token |
| `POST` | `/api/v1/admin/enroll/:token` | public | Consume token → auto-login as admin |

### Admin

| Method | Path | Role | Description |
| --- | --- | --- | --- |
| `POST` | `/api/v1/admin/admins` | admin | Create admin + return enrollment token |
| `GET` | `/api/v1/admin/users` | admin | List user accounts (includes `beta_access`) |
| `PUT` | `/api/v1/admin/users/:id/beta-access` | admin | Grant/revoke a user's beta access — the only way it's ever changed |
| `POST` | `/api/v1/admin/invitations` | admin | Create a user invitation link |
| `GET` | `/api/v1/admin/invitations` | admin | List invitation links |
| `POST` | `/api/v1/admin/test-email` | admin | Send a test email via Resend to verify the mail configuration |
| `GET` | `/api/v1/admin/signup-requests` | admin | List account signup requests from the public home page (paginated, state filter) |
| `POST` | `/api/v1/admin/signup-requests/approve` | admin | Approve requests by id — issues invitation links and emails them via Resend |

Every route an authenticated admin calls lives under `/api/v1/admin/...` — that
prefix is the signal that the route requires the admin role, regardless of
what resource it operates on.

User accounts themselves aren't created by admin directly — an admin creates an
invitation link (`POST /api/v1/admin/invitations`) and the user self-registers
with `POST /api/v1/invitations/:token/register` (public, token-based — not
under `/admin/`, since the caller isn't authenticated as an admin).

Visitors can also request an account themselves: `POST /api/v1/signup-requests`
(public) records their email; an admin approves it
(`POST /api/v1/admin/signup-requests/:id/approve`), which issues an invitation
link and emails it to them via Resend.

### User — VTA Setup Wizard

| Method | Path | Role | Description |
| --- | --- | --- | --- |
| `GET` | `/api/v1/user/me` | user | Get own profile, incl. `beta_access` (read-only) |
| `POST` | `/api/v1/setup/validate` | user | Check Cloudflare connectivity |
| `POST` | `/api/v1/setup` | user | Create session + provision DNS (`mode=full_stack_with_vtc` requires `beta_access`; `full_stack` is retired for new sessions — existing ones remain supported) |
| `DELETE` | `/api/v1/setup/:id` | user | Cancel session + tear down DNS |

## Beta Access

`users.beta_access` (bool, default `false`) gates access to features still in
beta — currently the `full_stack` and `full_stack_with_vtc` modes on
`POST /setup`. It's a plain on/off switch, not a tier: only an admin can flip
it (`PUT /api/v1/admin/users/:id/beta-access`), never the user themselves.
`GET /api/v1/user/me` lets the frontend check the caller's own value fresh
(not from the JWT, which doesn't carry it) so it can decide whether to offer
the beta modes at all.

## API Docs Rule

**Every new route must be documented in `internal/apidocs/openapi.yaml`.**

Assign the correct tag so it appears in the right group in Scalar:

- `System` — health / system routes
- `Auth — Admin` / `Auth — User` — login routes
- `Admin` — routes requiring admin JWT
- `User` — routes requiring user JWT

## Kubernetes Design

### Per-User Namespace Isolation

Every user gets their own namespace: `vtafarm-user-{userID}`.

`EnsureUserEnvironment` creates on first use (idempotent):

1. **Namespace** labelled `managed-by=vtafarm`
2. **ServiceAccount** `pod-operator`
3. **ServiceAccount** `vta` — identity the VTA jobs/Deployment run as; the
   farm Vault's per-user kubernetes-auth role is bound to this SA
4. **Role** `pod-manager` — grants `pods`, `pods/log`, `pods/exec`
5. **RoleBinding** — binds the SA to the Role

### Secret Storage (HashiCorp Vault)

Each VTA's master seed is stored in **HashiCorp Vault** (KV v2), not a
Kubernetes Secret. See `helm/vtafarm-vault` (farm Vault) and
`helm/vtafarm-transit` (in-cluster auto-unseal).

- `internal/vault` provisions, per user, a Vault **policy** + **kubernetes-auth
  role** (`vta-user-<userID>`) scoped to `secret/{data,metadata}/vta/user-<id>/*`.
- The VTA pod authenticates to Vault with its `vta` ServiceAccount JWT and
  reads/writes its seed at `vta/user-<id>/session-<id>/master-seed`.
- vtafarm-api authenticates to Vault via **AppRole** (`VAULT_ROLE_ID` /
  `VAULT_SECRET_ID` from the `vtafarm-api-vault` Secret). It manages policies/
  roles and deletes seeds on teardown — it never reads seeds.
- The VTA `[secrets]` config block is rendered in `internal/setup/templates.go`
  with `vault_skip_verify = true` (self-signed in-cluster CA).

### API Server Permissions (Production)

Mirrors `helm/vtafarm-api/templates/vtafarm-api/clusterrole.yaml` exactly — keep both in sync.

```yaml
rules:
- apiGroups: [""]
  resources: ["namespaces"]
  verbs: ["get", "list", "create", "delete"]
- apiGroups: [""]
  resources: ["serviceaccounts"]
  verbs: ["get", "list", "create", "delete"]
- apiGroups: [""]
  resources: ["pods", "pods/log"]
  verbs: ["get", "list", "watch", "create", "delete"]
- apiGroups: [""]
  resources: ["pods/exec"]
  verbs: ["create"]
- apiGroups: ["rbac.authorization.k8s.io"]
  resources: ["roles", "rolebindings"]
  verbs: ["get", "list", "create", "delete"]
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "create", "delete"]
- apiGroups: [""]
  resources: ["persistentvolumeclaims"]
  verbs: ["get", "list", "create", "delete", "watch"]
- apiGroups: [""]
  resources: ["services"]
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
