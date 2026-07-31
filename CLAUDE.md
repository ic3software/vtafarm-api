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
| Container | Multi-stage Dockerfile + Helm (prod) |

## Quick Start

The database is **shared**: one PostgreSQL in the dev cluster that every
developer tunnels to. There is no local database. Read
`docs/shared-dev-database.md` once — it changes how migrations, accounts and
`ORCHESTRATOR_RESUME` behave.

```bash
cp .env.example .env
make forward-db           # tunnel the shared dev database (own terminal, keep open)
make forward-vault        # tunnel Vault — required for setup work (own terminal)
make dev                  # API with Air hot-reload; migrations run automatically
make enroll               # first admin + 24h enrollment token — ONCE for the whole team
```

API: `http://localhost:8080`
Docs (local only): `http://localhost:8080/docs`

## Environment Variables

See `.env.example` for all options. Key ones:

| Variable | Default | Notes |
| --- | --- | --- |
| `APP_PORT` | `8080` | HTTP listen port |
| `APP_ENV` | `development` | Set to `production` to disable `/docs` |
| `DB_HOST` | `localhost` | The `make forward-db` tunnel to the shared dev database |
| `DB_NAME` | `vtafarm` | |
| `JWT_SECRET` | `change-me-in-production` | HS256 signing secret — identical across the team, since they share one set of accounts |
| `ORCHESTRATOR_RESUME` | `true` | Re-attach interrupted sessions/upgrades at startup. Crash recovery, so production keeps the default; local `.env` sets `false` so that N developers' APIs don't all resume the same rows |
| `KUBECONFIG` | `~/.kube/config` | Leave empty; auto-detected |
| `K8S_NAMESPACE_PREFIX` | `vtafarm-user` | Per-user namespace prefix |
| `DID_HOSTING_DID` / `DID_HOSTING_PRIVATE_KEY` | — | vtafarm-api's **own** keypair (`make gen-keypair`) for the DID-hosting control API, enrolled in a daemon's ACL with `role=admin`. Not anything a daemon issued, so one keypair serves every daemon it is enrolled in. There are deliberately no DID-hosting **URLs** here — see "Shared infrastructure comes from the platform stack" below |
| `CLOUDFLARE_API_TOKEN` | — | Cloudflare API token (`Zone:DNS:Edit` permission) |
| `CLOUDFLARE_ZONE_ID` | — | Cloudflare Zone ID for the user's root domain |
| `CLUSTER_INGRESS_IP` | — | External IP of the cluster's Ingress-NGINX LoadBalancer |
| `ACME_CLUSTER_ISSUER` | `letsencrypt-http01` | The same issuer in every environment — there is no staging variant. A staging certificate passes `tls_provision` and then crash-loops the mediator, because the components resolve each other's `did:webvh` over HTTPS and reject an untrusted chain. Every environment therefore shares Let's Encrypt's unraisable allowances (5 certs per identical name set per week), so keep iteration on domains we own |

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
├── k8s/dev-postgres/           # The shared dev database (Secret / PVC / Deployment / Service)
├── docs/
│   ├── vta-setup-design.md          # API design for VTA setup automation (Mode A + shared shape)
│   ├── full-stack-setup-design.md   # Authoritative design for the full_stack mode (all 4 components)
│   ├── custom-domain-design.md      # Custom + platform domains, the dev- prefix (§17 = what has shipped)
│   ├── shared-dev-database.md       # One PostgreSQL for the team + what it changes
│   └── vault-transit-upgrade.md     # Vault / transit upgrade + restore runbook
└── internal/
    ├── apidocs/
    │   ├── openapi.yaml        # OpenAPI 3.1 spec — update whenever routes change
    │   └── apidocs.go          # Embeds spec; serves GET /openapi.yaml and GET /docs
    ├── config/config.go        # Config struct loaded from env vars
    ├── database/database.go    # DB connect + migrate on startup
    ├── cloudflare/client.go    # Cloudflare API: CreateARecord, DeleteRecord
    ├── dnscheck/checker.go     # TXT + CNAME resolution, straight to public resolvers
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
    │   ├── domain.go           # Attach / verify / release a user-owned domain
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
| `DELETE` | `/api/v1/admin/setup-sessions/:id` | admin | Delete any user's session — same teardown as `DELETE /setup/:id`, but not scoped to the caller. Irreversible; the UI requires typing the session's `vta_name` to confirm. Deleting the **platform stack** additionally requires `{"confirm": "<label>"}` in the body — enforced by the API, not the UI |
| `GET` | `/api/v1/admin/setup-sessions/:id/logs` | admin | Stream any session's setup logs (`full_stack` only). Exists for the platform stack: it's owned by a passkey-less system account, so nobody can hold the cookie the user-facing route requires |
| `POST` | `/api/v1/admin/setup-sessions/:id/admin` | admin | Resume a session parked at `awaiting_admin_did`. Same reason as the logs route — and the platform stack always parks there, since the admin DID is minted from a VTA DID the pipeline hasn't produced yet |
| `POST` | `/api/v1/admin/setup-sessions/:id/dids/reissue-enroll` | admin | Admin twin of the user route |
| `POST` | `/api/v1/admin/setup-sessions/:id/dids/enroll-ack` | admin | Admin twin of the user route |
| `POST` | `/api/v1/admin/setup-sessions/:id/vtc/reissue-install` | admin | Admin twin. The one that matters most: without an install URL + claim code nobody can claim the platform community |
| `POST` | `/api/v1/admin/setup-sessions/:id/vtc/install-ack` | admin | Admin twin of the user route |
| `POST` | `/api/v1/admin/platform-stack` | admin | Create the platform stack: domain row + 4 proxied DNS records + `full_stack` session, in one action. The only route that mints a `domains` row for our own zone |
| `GET` | `/api/v1/admin/platform-stack` | admin | Its state, plus the `config_values` to copy into the environment once it's running |
| `GET` | `/api/v1/admin/setup/domain-info` | admin | Admin-cookie twin of `GET /setup/domain-info` — the platform stack page names its hostnames before they exist |

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
| `POST` | `/api/v1/setup` | user | Create session + provision DNS (`mode=vta_only` or `full_stack`; `full_stack` requires `beta_access`). Optional `domain_id` runs it on a verified custom domain — then `label` replaces `vta_name`/`vtc_name`, and no DNS is created |
| `GET` | `/api/v1/domains` | user | The caller's domains (at most one) |
| `POST` | `/api/v1/domains` | user | Attach a domain → row + fresh TXT token + the 5 records to create |
| `GET` | `/api/v1/domains/:id` | user | Detail, resolved live; promotes to verified when everything passes |
| `POST` | `/api/v1/domains/:id/verify` | user | Run the check. A failure is **200**, not 4xx — it's a retryable state, not a client error |
| `DELETE` | `/api/v1/domains/:id` | user | 409 while a session runs on it |
| `DELETE` | `/api/v1/setup/:id` | user | Cancel session + tear down DNS |
| `POST` | `/api/v1/setup/:id/upgrade` | user | Self-service image upgrade/downgrade of the user's **own** session (looked up by `vta_name AND user_id` — never another user's) |
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

Design: `docs/custom-domain-design.md` (§17 tracks what has shipped).

### Custom domains

A user attaches a zone they own on its own page (`/api/v1/domains`), proves
control, and only then creates a session against it. Keeping verification
**out of** session creation is what removes a whole state from the session state
machine: a session is only ever created against DNS that is already live, so it
starts provisioning immediately instead of parking half-built while someone
edits DNS.

- **Two records, both required.** Four `CNAME → {env_prefix}lb.{CLUSTER_DOMAIN}`
  prove traffic routes to us; one TXT at `_vtafarm-challenge.<domain>` (or the
  apex) proves the person asking controls the zone. The CNAMEs alone don't:
  someone who deletes their session but leaves their records behind would
  otherwise hand the next attacher a passing check on a name they never
  controlled. The token is minted fresh per attach, never rotates while pending,
  and **may be deleted once verified** — it's checked at verification time only.
- **We never touch the user's DNS.** Not on create, not on teardown — which is
  why the UI must tell them to remove the four CNAMEs themselves.
- **One certificate covers all four names**, issued by cert-manager over
  HTTP-01 (`ACME_CLUSTER_ISSUER`). The four Ingresses share its Secret and carry
  **no** `cert-manager.io/cluster-issuer` annotation — with the annotation
  *and* our own Certificate, ingress-shim would create a second one for the same
  Secret and the two would re-issue in a loop.
- **Teardown deletes the Certificate but keeps the Secret.** Recreating a
  session on the same domain requests the identical four names, which is what
  Let's Encrypt's unraisable "5 per identical set per 7 days" limit counts;
  leaving the Secret lets a rebuild reuse the certificate for free. The
  namespace collects it when the user's last session goes.
- **The CNAME target is derived, never configured** (`setup.CNAMETarget`). It's a
  name rather than the ingress IP because the user's records are effectively
  permanent, so the cluster IP has to be able to change without anyone editing
  DNS again.
- **`CLUSTER_DOMAIN` and every subdomain of it are refused for every caller,
  admins included.** The only route that mints a row for our own zone is
  `POST /admin/platform-stack`. That's enforced at the route, not by role,
  because the two paths produce different objects.
- **Always on** — no feature flag. It still needs its one-off cluster
  prerequisites (the grey-cloud `lb` / `dev-lb` records, the ACME
  ClusterIssuers) before a verification can pass; until they exist the check
  fails with a reason, which tells an operator more than a hidden route would.

### A session is addressed by its name

`vta_name` is the session's public identifier: the `:id` in `/setup/<name>` and
`/admin/setup-sessions/<name>`, and the word the UI makes you type to confirm a
delete. There is no opaque id — `setup_sessions.unique_id` is gone.

That is why `setup_sessions_vta_name_unique` is **global**, not partial on
`domain_type = 'managed'` as it was while the name was only a hostname. The admin
routes resolve a session by name with no `user_id` to scope the query, so two
rows sharing a name would make them ambiguous. The cost, stated plainly: a label
on a fixed-label domain now lives in a global namespace, so two users cannot both
call their session `main`.

`setup_sessions_did_path_unique` is implied by that global index and kept anyway
— it states the narrower, permanent rule (DID paths collide per daemon), which
must survive if sessions ever stop being addressed by name.

Users and admins keep their own `unique_id`; only sessions lost theirs.

### Shared infrastructure comes from the platform stack, not configuration

What a `vta_only` session is wired to — the mediator DID and the DID-hosting
resolution + control URLs — is read from the platform stack's own session row
when the session is created, and snapshotted onto it
(`setup_sessions.mediator_did` / `did_hosting_server_url` /
`did_hosting_control_url`).

These were `MEDIATOR_DID` / `DID_HOSTING_SERVER_URL` / `DID_HOSTING_CONTROL_URL`
in the environment, pasted in by an admin from the platform stack page once the
pipeline had minted them. Removing that copy step removes the whole class of
"the stack is running but this server is still pointed at the last one".

Two consequences worth holding onto:

- **`didhosting.Client` cannot be a startup singleton**, because no URL is known
  at boot — on a first boot the platform stack does not exist. `didhosting.Factory`
  builds one per control URL and caches it; the keypair it authenticates with
  stays global because it is vtafarm-api's own identity.
- **Snapshotted, not looked up.** A `did:webvh` bakes its host into the
  identifier at mint time, so the URLs are facts about the session, not current
  settings. Teardown deletes through the control URL the row carries — a
  rebuilt platform stack is a different daemon, and deleting from it would strip
  somebody else's DID while leaving this one behind.

Two URL columns rather than one because a standalone DID-hosting service splits
resolution from its management API. The daemon build deployed today answers both
on one host, so they are equal for every session that exists so far.

**Every `full_stack` session enrolls this API in its own daemon's ACL.**
`step_dids_grant_farm` runs `did-hosting-daemon add-acl --did <DID_HOSTING_DID>
--role admin --label vtafarm` as an offline Job on the dids PVC. Offline is
forced: the control API authenticates callers *from* the ACL, so doing it over
HTTP would require already being in it.

It runs for customers' stacks too, not just the platform one — the farm operates
these deployments and manages the `did.jsonl` documents they serve, which is
what `role=admin` grants (it bypasses the per-DID ownership check on publish).
That does not widen the trust boundary; the pod, PVC and Vault access are ours
regardless. The platform stack additionally *depends* on the entry, since every
`vta_only` DID upload authenticates with that keypair.

`add-acl` errors on an existing entry rather than passing, so the step probes
`list-acl` first — without that, any resume would fail the session.

The credentials story — and the open question of how a **user-supplied** DID host
would authorise us — is in `docs/vta-setup-design.md` §"DID hosting credentials".

### DID paths and where they must be unique

A `did:webvh` path only has to be distinct among the DIDs served by the **same**
daemon. Which daemon that is follows from neither mode nor `domain_type` alone,
so `setup_sessions.did_hosting_server_url` records it and
`UNIQUE (did_hosting_server_url, vta_name) WHERE did_hosting_server_url <> ''`
is the whole rule:

| session | daemon | shares with |
| --- | --- | --- |
| `vta_only` (managed, and custom once allowed) | the shared one — the platform stack's | every other `vta_only` **and the platform stack** |
| `full_stack` managed / custom | its own `dids[-<name>].<zone>` | nothing |
| `platform` | its own — which **is** the shared one | every `vta_only` |

A custom domain moves a session's hostnames, not its DID host: `vta_only`
deploys no daemon, so its DID lands on ours however its hostnames are named.
That is why per-domain scoping would be the wrong test, and why
`vta_name` still has to be globally unique for that mode.

Paths are `<vta_name>-vta` / `<vta_name>-mediator` / `<vtc_name>-vtc`
(`setup.VtaDidPath` and friends) — **`vta_only` included**, even though it mints
only one. The suffix is load-bearing, not cosmetic: it is what lets one index
over `vta_name` stand in for a rule about rendered paths. Every indexed path
ends in `-vta`, so it can never equal an unindexed `-mediator` / `-vtc` path
belonging to the platform stack. Give `vta_only` a bare name and a session
called `<platform label>-mediator` walks straight past the index and collides on
the daemon. The owning user's `unique_id` used to prefix the path, which made it
unique for free and put an opaque id inside every DID; the suffix replaced it.

### The platform stack

One `full_stack` session per environment at `vta.firstperson.dev` / `vtc.` /
`mediator.` / `dids.` — the farm's flagship stack, and the mediator + DID host
that `vta_only` sessions point at. `POST /api/v1/admin/platform-stack` creates
the domain row, the DNS and the session in one action.

**`vta_only` cannot be created until it exists.** That mode is only the VTA,
wired to the mediator and DID host the platform stack provides, so an agent
created before it would never deliver a message. `GET /setup/availability`
reports `vta_only.available: false` with a `reason`
(`platform_stack_missing` / `platform_stack_not_ready` /
`shared_infra_unconfigured`), and `POST /setup` answers 503 — the UI is not the
gate. `full_stack` provisions its own and is never blocked on it.

The third reason is now narrow and transient: a stack marked running whose
mediator DID has not landed on its row. It used to mean "an admin hasn't pasted
`MEDIATOR_DID` into this server's environment yet" — that step is gone (see
below).

- **Owned by a system account**, not an admin: `setup_sessions.user_id` is a FK
  to `users` and derives the namespace, while admins are a separate table. The
  account is a `users` row with `unique_id = 'platform'`, created on first use,
  with no passkey and no email. `GET /admin/users` flags it as `system: true`.
- **The admin DID is supplied afterwards, exactly as for a user's session.**
  `POST /admin/platform-stack` takes no `admin_did`: that DID is minted locally
  by `pnm setup` from the VTA DID, which doesn't exist until `step_vta_setup`
  has run. The stack parks at `awaiting_admin_did` like any other `full_stack`.
  The one difference is who resumes it — **any admin**, through
  `POST /admin/setup-sessions/:id/admin`, because a passkey-less owner means the
  user-facing route can never be called for it.
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
- apiGroups: ["cert-manager.io"]
  resources: ["certificates"]
  verbs: ["get", "list", "watch", "create", "delete"]
```
