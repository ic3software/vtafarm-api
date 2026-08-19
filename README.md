# VTA Farm API

Go REST API backend for managing VTA setup sessions with per-user namespace isolation.

## Tech Stack

| Layer | Choice |
| --- | --- |
| HTTP | Gin |
| ORM | GORM + PostgreSQL driver |
| Migrations | golang-migrate (raw SQL) |
| Database | PostgreSQL 18 |
| K8s client | client-go v0.36 |
| Hot reload | Air |
| Container | Helm (prod) |

---

## Local Development

The API runs directly on your machine, against the **shared PostgreSQL in the dev
cluster** — there is no local database. Running the API locally gives it direct
access to your `~/.kube/config` without any networking workarounds.

The database being shared has consequences worth reading once:
[`docs/shared-dev-database.md`](docs/shared-dev-database.md). It already exists —
`make deploy-db` (re)deploys it and is not something you need for daily work.

### Prerequisites

- Go 1.26+
- [Air](https://github.com/air-verse/air) — `go install github.com/air-verse/air@latest`
- `kubectl` with access to the dev cluster (context `k8s-fpp-dev`)

### Setup

1. Copy the example env file:

   ```bash
   cp .env.example .env
   ```

   `DB_*` and `JWT_SECRET` must match the rest of the team — one database means
   one set of accounts, and a token signed with a different secret is rejected.

2. Open the two tunnels into the dev cluster. Each needs its own terminal and
   stays open while you develop — both reconnect on their own, since
   `kubectl port-forward` drops on pod restarts.

   ```bash
   make forward-db          # localhost:5432 → svc/vtafarm-dev-postgres
   make forward-vault       # localhost:8200 → vault/svc/vault
   ```

   Vault is required for VTA setup — the API provisions per-user Vault
   policies/roles.

3. Start the API (migrations run automatically on startup):

   ```bash
   make dev
   ```

   The API is now available at `http://localhost:8080`.
   API docs: `http://localhost:8080/docs`

4. (Optional) Generate a DID hosting keypair (required only if DID hosting is enabled):

   ```bash
   make gen-keypair
   ```

   Copy the two output lines (`DID_HOSTING_PRIVATE_KEY` and `DID_HOSTING_DID`) into your `.env`,
   then register the DID in the did-hosting service Access Control with **role=Service**.

5. Create the first admin enrollment token — run in a separate terminal while the API is running:

   ```bash
   make enroll
   ```

   This prints a 24-hour single-use token. Pass it to your frontend enrollment page, or call the API directly:

   ```bash
   # Consume the token — creates the admin account and sets vtafarm_admin cookie
   POST /api/v1/admin/enroll/<token>

   # Then register a passkey (use the returned JWT as Authorization: Bearer)
   POST /api/v1/admin/passkeys/register/begin
   POST /api/v1/admin/passkeys/register/complete?name=MyKey
   ```

   To create additional admins, an authenticated admin calls `POST /api/v1/admin/admins`, which returns a new enrollment token.

### Environment Variables

Copy `.env.example` and adjust as needed:

| Variable | Default | Notes |
| --- | --- | --- |
| `APP_PORT` | `8080` | HTTP listen port |
| `APP_ENV` | `development` | Set to `production` to disable `/docs` |
| `DB_HOST` | `localhost` | The `make forward-db` tunnel to the shared dev database |
| `DB_NAME` | `vtafarm` | |
| `JWT_SECRET` | _(required)_ | HS256 signing secret — must match the team, see below |
| `ORCHESTRATOR_RESUME` | `true` | Re-attach interrupted sessions at startup. Set `false` locally — see [`docs/shared-dev-database.md`](docs/shared-dev-database.md) |
| `CLUSTER_INGRESS_IP` | _(required)_ | External IP of the cluster's Traefik LoadBalancer |
| `CLOUDFLARE_API_TOKEN` | _(optional)_ | Required for VTA setup wizard |
| `CLOUDFLARE_ZONE_ID` | _(optional)_ | Required for VTA setup wizard |
| `KUBECONFIG` | _(empty)_ | Auto-detects `~/.kube/config` when empty |
| `K8S_NAMESPACE_PREFIX` | `vtafarm-user` | Per-user namespace: `vtafarm-user-{userID}` |

#### Generating JWT_SECRET

```bash
openssl rand -base64 32
```

### Migration Workflow

```bash
make migrate-new NAME=add_users_table   # create a new migration pair
make migrate                             # apply pending migrations
make migrate-down                        # roll back one step
```

---

## Releasing

Publishing a version to GHCR: [`docs/release.md`](docs/release.md).

## Production Deployment

The production stack is deployed to a Kubernetes cluster (RKE2) via Helm.

### 1. TLS — Wildcard Certificate via cert-manager (one-time cluster setup)

All VTA sessions share a single `*.firstperson.dev` wildcard certificate managed by
cert-manager. Traefik serves it as its default certificate, so no per-Ingress TLS
configuration is needed.

#### Step 1 — Create the Cloudflare API token Secret

The token needs **Zone → Zone → Read** and **Zone → DNS → Edit** permissions on `firstperson.dev`.

```bash
kubectl create secret generic cloudflare-api-token \
  --namespace=cert-manager \
  --from-literal=api-token='<CLOUDFLARE_API_TOKEN>'
```

#### Step 2 — Apply ClusterIssuer

```bash
kubectl apply -f k8s/tls/clusterissuer.yaml
```

Verify it registered with Let's Encrypt:

```bash
kubectl get clusterissuer letsencrypt-prod
```

#### Step 3 — Apply Certificate

```bash
kubectl apply -f k8s/tls/certificate.yaml
```

cert-manager will complete the DNS-01 challenge (adds a `_acme-challenge.firstperson.dev`
TXT record to Cloudflare, then removes it) and store the issued certificate.
Check status with:

```bash
kubectl get certificate -n kube-system firstperson-dev-wildcard
```

The Certificate issues into `kube-system` — Traefik's own namespace — because a
TLSStore can only reference a Secret alongside it. Adjust both files if Traefik
runs elsewhere in your cluster.

#### Step 4 — Configure Traefik

Two things: the controller's entrypoints, and the default certificate.

**Entrypoints — through Rancher, not kubectl.** All fpp clusters are
Rancher-managed, and Rancher owns the `HelmChartConfig` object
(`objectset.rio.cattle.io/owner-name: managed-chart-config`). A `kubectl apply`
holds until the next sync or upgrade and is then reverted — this is exactly
what kept wiping ingress-nginx's `default-ssl-certificate`. Put the values in
the cluster spec so they survive:

Rancher UI → Cluster Management → the cluster → Edit YAML → `rkeConfig.chartValues`:

```yaml
rkeConfig:
  chartValues:
    rke2-calico: {}                 # other charts' entries — leave them alone
    rke2-traefik:
      ingressClass:
        isDefaultClass: true
      ports:
        web:
          http:
            redirections:
              entryPoint:
                to: websecure
                scheme: https
                permanent: true
        websecure:
          http:
            tls:
              enabled: true
```

Add these keys, do not replace the map — the siblings are other charts' values.

The chart ships no values schema, so a key that is misspelled or one level off
is accepted, produces no argument, and looks exactly like a working config.
Verify against the rendered args, never against what you typed.

On a cluster Rancher does not manage, the same values are in
`k8s/tls/rke2-traefik-config.yaml` — `kubectl apply` that instead.

**The default certificate** is a plain CRD object, not chart config, so Rancher
never touches it:

```bash
kubectl apply -f k8s/tls/tlsstore-default.yaml
```

After this every Ingress gets HTTPS and an HTTP→HTTPS redirect with no
annotation, `tls:` block or cert-manager annotation of its own.

Verify before going further — this is the step whose failure shows up several
minutes later as a mediator crash loop rather than as a TLS error.

**Test against the origin, not the hostname.** Managed and platform records are
*proxied* through Cloudflare, so plain `curl https://<hostname>` reports
Cloudflare's edge certificate (issuer: Google Trust Services) and Cloudflare's
status code — it tells you nothing about the cluster. Pin the node IP:

```bash
IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')

# 1. The certificate the cluster itself serves — Let's Encrypt, not TRAEFIK DEFAULT CERT
openssl s_client -connect "$IP":443 -servername <a dids hostname> </dev/null 2>/dev/null \
  | openssl x509 -noout -issuer

# 2. Routing. Pick a path the component actually serves: / on dids, /health on a
#    VTA. A 404 on / from a VTA is correct and means nothing is wrong.
curl -skI --resolve <a dids hostname>:443:"$IP" https://<a dids hostname> | head -1

# 3. The redirect really applied (see the note in rke2-traefik-config.yaml —
#    a mistyped values path fails silently)
kubectl -n kube-system get ds rke2-traefik \
  -o jsonpath='{.spec.template.spec.containers[0].args}' | tr ',' '\n' | grep redirections
```

Note the ADDRESS column of `kubectl get ingress` stays **empty** under Traefik in
this layout, and that is not a fault — see the `publishedService` note in
`k8s/tls/rke2-traefik-config.yaml`. Read the CLASS column instead.

#### Step 5 — Migrating an existing cluster off ingress-nginx

Only when the cluster already ran sessions. vtafarm-api never updates an Ingress
after creating it, so pre-existing ones keep `ingressClassName: nginx` and
Traefik ignores them:

```bash
KUBE_CONTEXT=k8s-fpp-dev ./scripts/migrate-ingress-to-traefik.sh           # dry run
KUBE_CONTEXT=k8s-fpp-dev ./scripts/migrate-ingress-to-traefik.sh --apply
```

### 2. HashiCorp Vault (one-time, before the API)

Each VTA's master seed is stored in HashiCorp Vault, which `vtafarm-k8s` stack
04 deploys — not this repo. Its runbook is `docs/vault.md` there, and it has to
be installed and bootstrapped **before** the API: the API needs the
`vtafarm-api-vault` Secret that `scripts/vault-bootstrap.sh farm` produces.

### 3. Create the API Secrets (one-time)

Copy the example secret manifest and fill in real values:

```bash
cp k8s/secret.yaml.example k8s/secret.yaml
```

Edit `k8s/secret.yaml`, then generate the values you need:

```bash
# JWT_SECRET
openssl rand -base64 32

# DID_HOSTING_PRIVATE_KEY + DID_HOSTING_DID
make gen-keypair
```

Apply to the cluster:

```bash
kubectl apply -f k8s/secret.yaml
```

> **Note:** `k8s/secret.yaml` is listed in `.gitignore` — never commit it.

### 4. Create the PostgreSQL Secret (one-time)

```bash
kubectl create secret generic vtafarm-api-postgresql \
  --from-literal=postgres-password='your-strong-password' \
  --namespace=default
```

### 5. Deploy with Helm

```bash
make deploy \
  DOCKER_USERNAME=your-dockerhub-username \
  TAG=$(git rev-parse --short HEAD)
```

With a custom namespace:

```bash
make deploy \
  DOCKER_USERNAME=your-dockerhub-username \
  TAG=$(git rev-parse --short HEAD) \
  NAMESPACE=vtafarm
```

**Uninstall:**

```bash
helm uninstall vtafarm
```

### 6. Run Migrations in the Cluster

```bash
kubectl exec -it deployment/vtafarm \
  -n <namespace> -- go run ./cmd/migrate up
```

### Production Kubernetes RBAC

When deploying via Helm (`make deploy`), the ClusterRole below is created
automatically. For **test clusters or manual setups**, apply it by hand.

The master seed is stored in HashiCorp Vault (deployed by `vtafarm-k8s`), not a
Kubernetes Secret, so vtafarm-api needs no secrets permissions and there is no
`vtafarm-vta-secret-manager` ClusterRole.

The API server pod needs a `ClusterRole` with these permissions (managed by the Helm chart as `{{ .Values.name }}`):

```yaml
rules:
- apiGroups: [""]
  resources: ["namespaces", "serviceaccounts"]
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
- apiGroups: ["cert-manager.io"]
  resources: ["certificates"]
  verbs: ["get", "list", "watch", "create", "delete"]
- apiGroups: ["traefik.io"]
  resources: ["middlewares"]
  verbs: ["get", "list", "create", "delete"]
```
