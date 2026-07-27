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
│   ├── vta-setup-design.md          # API design for VTA setup automation (Mode A + shared shape)
│   ├── full-stack-setup-design.md   # Authoritative design for the full_stack mode (all 4 components)
│   ├── custom-domain-design.md      # Custom + platform domains, the dev- prefix (§17 = what has shipped)
│   └── vault-transit-upgrade.md     # Vault / transit upgrade + restore runbook
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
    │   ├── domain.go           # custom / platform domains a session can run under
    │   └── setup_session.go    # GORM model for VTA setup sessions
    ├── handler/
    │   ├── health.go
    │   ├── auth.go             # AdminLogin, UserLogin
    │   ├── user.go             # Create user (admin only)
    │   ├── admin_platform_stack.go  # The farm's own stack + its system account
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
| `GET` | `/api/v1/admin/dashboard` | admin | Cluster capacity overview: CPU/memory/storage per node + remaining-session estimates |
| `GET` | `/api/v1/admin/users` | admin | List user accounts (includes `beta_access`) |
| `PUT` | `/api/v1/admin/users/:id/beta-access` | admin | Grant/revoke a user's beta access — the only way it's ever changed |
| `POST` | `/api/v1/admin/users/:id/recovery-link` | admin | Issue a 1h single-use login link for a user who lost their passkey |
| `POST` | `/api/v1/admin/invitations` | admin | Create a user invitation link |
| `GET` | `/api/v1/admin/invitations` | admin | List invitation links |
| `GET` | `/api/v1/admin/setup-sessions` | admin | List all users' setup sessions (paginated, 20/page) |
| `DELETE` | `/api/v1/admin/setup-sessions/:id` | admin | Delete any user's session — same teardown as `DELETE /setup/:id`, but not scoped to the caller. Irreversible; the UI requires typing the session id to confirm. Deleting the **platform stack** additionally requires `{"confirm": "<label>"}` in the body — enforced by the API, not the UI |
| `POST` | `/api/v1/admin/platform-stack` | admin | Create the platform stack: domain row + 4 proxied DNS records + `full_stack` session, in one action. The only route that mints a `domains` row for our own zone |
| `GET` | `/api/v1/admin/platform-stack` | admin | Its state, plus the `config_values` to copy into the environment once it's running |

Every route an authenticated admin calls lives under `/api/v1/admin/...` — that
prefix is the signal that the route requires the admin role, regardless of
what resource it operates on.

User accounts themselves aren't created by admin directly — an admin creates an
invitation link (`POST /api/v1/admin/invitations`) and the user self-registers
with `POST /api/v1/invitations/:token/register` (public, token-based — not
under `/admin/`, since the caller isn't authenticated as an admin).

Visitors can also sign up themselves: `POST /api/v1/signup` (public,
rate-limited per IP) takes an email and creates the account immediately — no
admin approval and no email is ever sent. The email is a self-declared label
(unverified, unique via a partial index on `users.email`, NULL for pre-email
and admin-invited accounts); the passkey the frontend registers right after is
what authenticates. Until that first passkey exists, repeating the call with
the same email resumes the same account instead of failing, so an abandoned
passkey ceremony never strands the email; once a passkey exists it answers
409.

There is deliberately no self-service (email-based) recovery. A user who lost
their passkey contacts an admin, who verifies their identity out of band and
issues a recovery link (`POST /api/v1/admin/users/:id/recovery-link` — 1 hour,
single-use, delivered out of band; issuing a new one expires the previous).
Consuming it (`POST /api/v1/recovery/:token`, public) revokes every passkey on
the account and logs the holder in to register a fresh one.

### User — VTA Setup Wizard

| Method | Path | Role | Description |
| --- | --- | --- | --- |
| `GET` | `/api/v1/user/me` | user | Get own profile, incl. `beta_access` (read-only) |
| `POST` | `/api/v1/setup/validate` | user | Check Cloudflare connectivity |
| `GET` | `/api/v1/setup/domain-info` | user | Hostname facts for this environment (`managed_domain`, `env_prefix`, `target_ip`, `target_host`) so the portal never hardcodes the production hostname shape |
| `POST` | `/api/v1/setup` | user | Create session + provision DNS (`mode=vta_only` or `full_stack`; `full_stack` requires `beta_access`) |
| `DELETE` | `/api/v1/setup/:id` | user | Cancel session + tear down DNS |
| `POST` | `/api/v1/setup/:id/upgrade` | user | Self-service image upgrade/downgrade of the user's **own** session (looked up by `unique_id AND user_id` — never another user's) |
| `GET` | `/api/v1/setup/:id/upgrade` | user | Latest self-service upgrade of that session, for progress polling |

## Setup Modes

There are exactly two: **`vta_only`** and **`full_stack`**.

- `vta_only` — the VTA alone, pointed at a shared external mediator + DID host.
- `full_stack` — VTA + DIDComm Mediator + WebVH DID Hosting daemon + VTC, all
  four in the user's namespace, wired to each other. **The VTC is mandatory**;
  there is no VTC-less variant.

An earlier iteration had a third mode, `full_stack_with_vtc`, with `full_stack`
meaning "the same thing minus the VTC". That split is gone: the VTC-less
pipeline is retired and `full_stack` now always means all four components. The
`full_stack_with_vtc` identifier does not exist anywhere in the API, the DB, or
the frontend.

Design: `docs/full-stack-setup-design.md` (authoritative for `full_stack`),
`docs/vta-setup-design.md` (Mode A / shared shape).

## Domains

`domain_type` on a session says where its hostnames come from. It is
**orthogonal to mode** — any `full_stack` session is also exactly one of:

| `domain_type` | Hostnames | Zone | DNS | TLS |
| --- | --- | --- | --- | --- |
| `managed` (default) | `vta-<name>.firstperson.dev` | ours | we create it | cluster wildcard |
| `platform` | `vta.firstperson.dev` (fixed labels) | ours | we create it | cluster wildcard |
| `custom` | `vta.aaa.com` (fixed labels) | **theirs** | they create it, we verify | cert-manager + Let's Encrypt |

In development every label additionally carries a `dev-` prefix
(`setup.EnvPrefix`), so `dev-vta-alice.firstperson.dev` and
`dev-vta.firstperson.dev`. It's a prefix, not an infix, so every record a
locally run API created sorts together in the Cloudflare dashboard.

Rows live in `domains` (`kind` = `custom` | `platform`; `managed` sessions have
`domain_id IS NULL`). A domain backs **at most one session** — its four labels
are fixed, so a second session would want the same hostnames.

`custom` is **not implemented yet**: nothing can create such a row. Design:
`docs/custom-domain-design.md` (§17 tracks what has shipped).

### The platform stack

One `full_stack` session per environment at `vta.firstperson.dev` / `vtc.` /
`mediator.` / `dids.` — the farm's flagship stack, and the mediator + DID host
that `vta_only` sessions point at. `POST /api/v1/admin/platform-stack` creates
the domain row, the DNS and the session in one action.

- **Owned by a system account**, not an admin: `setup_sessions.user_id` is a FK
  to `users` and derives the namespace, while admins are a separate table. The
  account is a `users` row with `unique_id = 'platform'`, created on first use,
  with no passkey and no email. `GET /admin/users` flags it as `system: true`.
- **`admin_did` is required at create time.** The pipeline parks at
  `awaiting_admin_did` without it, and the only route that resumes a gated
  session (`POST /setup/:id/admin`) filters by `user_id` — so nobody could call
  it for a passkey-less account and the stack would strand permanently.
- **No verification, no ACME, no Let's Encrypt quota** — the zone is ours and
  the `*.firstperson.dev` wildcard already covers the names.
- `beta_access` doesn't apply (it's a user gate); cluster capacity does.

## Beta Access

`users.beta_access` (bool, default `false`) gates access to features still in
beta — currently the `full_stack` mode on `POST /setup`. It's a plain on/off
switch, not a tier: only an admin can flip it
(`PUT /api/v1/admin/users/:id/beta-access`), never the user themselves.
`GET /api/v1/user/me` lets the frontend check the caller's own value fresh
(not from the JWT, which doesn't carry it) so it can decide whether to offer
the beta mode at all.

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
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list"]
- apiGroups: ["metrics.k8s.io"]
  resources: ["nodes"]
  verbs: ["get", "list"]
- apiGroups: ["longhorn.io"]
  resources: ["nodes", "settings", "volumes"]
  verbs: ["get", "list"]
- apiGroups: ["storage.k8s.io"]
  resources: ["storageclasses"]
  verbs: ["get", "list"]
```
