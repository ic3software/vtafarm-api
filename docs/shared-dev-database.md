# The shared development database

One PostgreSQL for the whole team, running in the dev cluster, reached by every
developer through `kubectl port-forward`. It replaces the per-developer
`docker-compose` database.

## Why

The dev **cluster** was already shared; the database was not. So `setup_sessions`
rows lived on one laptop while the namespaces, PVCs and Jobs they describe lived
in `rke2-vtafarm-dev` where everyone could see them. Keeping the two in agreement
meant passing dumps around by hand, and every restore silently reintroduced
whatever the sender's laptop happened to hold.

Moving the database next to the cluster it describes removes the sync step
rather than automating it. The cost is real and is the subject of most of this
document: the database is now shared mutable state, and several of this API's
behaviours quietly assumed it was not.

## What is deployed

`k8s/dev-postgres/` — four manifests in the `default` namespace of the dev
cluster (`rke2-vtafarm-dev`):

| Object | Notes |
| --- | --- |
| Secret `vtafarm-dev-postgres` | password, committed on purpose — see below |
| PVC `vtafarm-dev-postgres` | Longhorn, 5Gi, `ReadWriteOnce` |
| Deployment `vtafarm-dev-postgres` | `postgres:18.4-alpine`, single replica, `Recreate` |
| Service `vtafarm-dev-postgres` | ClusterIP, 5432 |

Deployed with `make deploy-db`, which pins `--context rke2-vtafarm-dev` so it cannot
land in `docker-desktop` by accident.

Some deliberate choices:

- **The same image as production, pinned to the patch.** Both
  `k8s/dev-postgres/deployment.yaml` and `helm/vtafarm-api/values.yaml` say
  `postgres:18.4-alpine`, and `make check-pg-image` (part of `make test`, so CI
  runs it) fails the build if they ever differ. Production used to track the
  floating `18-alpine`, which meant a pod restart could move it a patch without
  anyone choosing to — and "works against dev" would quietly stop meaning
  "works against production". Bumping the version is a two-file edit, on
  purpose.
- **`default`, not a dedicated namespace.** It must never live under
  `fpp-user-*`: those namespaces are created and *deleted* by this API as
  sessions come and go, and the database would go with them.
- **Named `vtafarm-dev-postgres`, not `vtafarm-api-postgresql`.** The Helm chart
  (`helm/vtafarm-api/templates/postgresql/`) uses the latter, so if anyone ever
  installs the full chart into this cluster's `default` namespace, the two sets
  of objects don't collide.
- **The password is in git.** It is `postgres`, the same throwaway value
  `.env.example` has always carried, and nothing reaches port 5432 without a
  kubeconfig for the cluster. Committing it is what makes setup a single
  command. The corollary is a rule, not a hope: **nothing that matters may live
  in this database.** No production data, no real user records, no secret worth
  having. Master seeds are in Vault and stay there.
- **Not exposed.** No NodePort, no LoadBalancer, no Ingress. `port-forward` is
  the only path in, so access is authorised by the cluster's RBAC.
- **No backup job.** Decided deliberately: this is scratch data. Longhorn's
  reclaim policy is `Delete`, so removing the PVC destroys the team's data with
  no way back. `make deploy-db` only ever applies, never deletes, so a redeploy
  is safe; a `kubectl delete pvc` is not.

## Daily use

Three terminals, all left running:

```bash
make forward-db          # localhost:5432 → svc/vtafarm-dev-postgres
make forward-vault       # localhost:8200 → vault/svc/vault (needed for setup work)
make dev                 # air, against those tunnels
```

`make dev` refuses to start when nothing is listening on 5432 — without the
check the symptom is a bare `connection refused` from GORM, which reads like a
broken database rather than a missing tunnel.

Both `forward-*` targets loop on purpose. `kubectl port-forward` dies on a
dropped connection or whenever the pod restarts, and never returns on its own;
the loop reconnects every 2s. Ctrl-C stops it.

## Things that changed because the database is shared

### Startup no longer resumes interrupted work

`Orchestrator.Resume` picks up every session in `vta_setup_running` or
`provisioning` at startup, and `upgrade.Runner.Resume` does the same for image
upgrades. That is crash recovery, and it is correct when one API owns the
database. Against a shared one, every developer who starts their API resumes
**everyone's** in-flight sessions: several orchestrators creating Jobs for the
same session and writing the same status column.

So `ORCHESTRATOR_RESUME` gates both (they are one hazard; gating either alone
would achieve nothing):

- **Defaults to `true`.** Production must never lose crash recovery because
  someone forgot a Helm value, so the flag is opt-out and the chart needs no
  change.
- **`.env.example` sets it to `false`.** Local APIs are observers by default.
- **Turn it on in exactly one API** when you actually need to drive a `full_stack`
  pipeline, and coordinate that with the team.

Creating a session through the API still runs the orchestrator in that same
process — the flag only governs what happens at *startup*. Two people running
`POST /setup` at once is fine; they are different sessions.

### Migrations are a shared resource

Migrations run automatically on every API start, against everyone's database.

- Starting on an **older** branch is harmless: golang-migrate finds no file past
  the recorded version and returns `ErrNoChange`. Your schema simply has columns
  your code doesn't know about.
- **Destructive migrations are not harmless.** A `DROP COLUMN` merged by one
  person breaks everyone still on a branch whose code selects it.
- **`make migrate-down` hits everybody.** Don't run it against the shared
  database to test a rollback; do that against a throwaway local container.
- **A failed migration blocks the whole team.** golang-migrate marks the schema
  `dirty` and every subsequent start fails until someone repairs the version
  manually.

Working rules that follow:

1. Iterate on a new migration locally (a disposable `docker run postgres:18.4-alpine`)
   until it applies cleanly. The shared database sees it once it's settled.
2. Prefer additive migrations. Split a rename into add → backfill → drop across
   separate merges, so nobody's branch is broken between them.
3. Say something in the team channel before anything destructive lands.

### Accounts are shared, passkeys are not

`make enroll` creates the first admin. **Run it once for the team**, not once per
person. Everyone else gets their own account from an authenticated admin:

```
POST /api/v1/admin/admins     → enrollment token → register your own passkey
```

Passkeys are bound to the device that created them, so each person registers
their own even though the account rows are shared. `WEBAUTHN_RP_ID=localhost` is
the same for everyone, so a credential registered against one developer's
`localhost` works with their own API only — which is the intent.

`JWT_SECRET` **must be identical across the team**. One database means one set of
accounts, but a token signed by one API is rejected by another that signs with a
different secret, and the failure looks like a broken login rather than a config
mismatch.

### The database and the cluster are a pair

Rows in `setup_sessions` describe objects in `rke2-vtafarm-dev`. Anyone connected to
the shared database must also be pointed at that cluster and configured the same
way — `KUBECONFIG` context, `K8S_NAMESPACE_PREFIX=fpp-user`, `CLUSTER_DOMAIN`,
the Cloudflare token, Vault.

Run against `docker-desktop` by mistake and you leave rows behind that describe
namespaces nobody has: a session the team can see, can't use, and can only clear
by hand.

## Resetting it

There is no `make db-reset`, on purpose — the old `make reset` destroyed only
your own data, and a same-named target here would destroy everyone's.

To wipe and start over, deliberately:

```bash
kubectl --context rke2-vtafarm-dev delete deployment vtafarm-dev-postgres
kubectl --context rke2-vtafarm-dev delete pvc vtafarm-dev-postgres
make deploy-db
```

The next API start recreates the schema from the migrations, and the team needs
a fresh `make enroll`.

## Working offline

Nothing stops you running your own PostgreSQL — point `DB_HOST`/`DB_PORT` at it
in `.env` and skip `make forward-db`. It's the right move for testing a
destructive migration or a schema experiment. Just remember the cluster is still
shared: a local database plus the dev cluster is exactly the split this setup
exists to remove, so it's a temporary mode, not a way of working.
