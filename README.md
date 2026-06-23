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
| Container | Docker Compose (dev) / Helm (prod) |

---

## Local Development

The API runs directly on your machine while only the database runs in Docker.
This gives the API direct access to your local `~/.kube/config` without any
networking workarounds.

### Prerequisites

- Go 1.26+
- Docker & Docker Compose
- [Air](https://github.com/air-verse/air) — `go install github.com/air-verse/air@latest`
- `kubectl` configured with access to a cluster (for K8s features)

### Setup

1. Copy the example env file:

   ```bash
   cp .env.example .env
   ```

2. Start the DB + API (migrations run automatically on startup):

   ```bash
   make dev
   ```

   The API is now available at `http://localhost:8080`.
   API docs: `http://localhost:8080/docs`

3. Port-forward Vault so the locally-running API can reach it — required for
   VTA setup (the API provisions per-user Vault policies/roles). Run in a
   separate terminal and keep it open:

   ```bash
   kubectl port-forward -n vault svc/vault 8200:8200
   ```

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

   To create additional admins, an authenticated admin calls `POST /api/v1/admins`, which returns a new enrollment token.

### Environment Variables

Copy `.env.example` and adjust as needed:

| Variable | Default | Notes |
| --- | --- | --- |
| `APP_PORT` | `8080` | HTTP listen port |
| `APP_ENV` | `development` | Set to `production` to disable `/docs` |
| `DB_HOST` | `localhost` | Points to the Docker-managed PostgreSQL |
| `DB_NAME` | `vtafarm` | |
| `JWT_SECRET` | _(required)_ | HS256 signing secret — see below |
| `CLUSTER_INGRESS_IP` | _(required)_ | External IP of the cluster's Ingress-NGINX LoadBalancer |
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

## Production Deployment

The production stack is deployed to a Kubernetes cluster (RKE2) via Helm.

### 1. TLS — Wildcard Certificate via cert-manager (one-time cluster setup)

All VTA sessions share a single `*.firstperson.dev` wildcard certificate managed by
cert-manager. nginx-ingress serves it as the default SSL certificate, so no
per-Ingress TLS configuration is needed.

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
kubectl get certificate -n cert-manager firstperson-dev-wildcard
```

#### Step 4 — Configure nginx-ingress to use the wildcard cert by default

RKE2 manages its built-in nginx ingress controller via the `HelmChartConfig` CRD.

```bash
kubectl apply -f k8s/tls/rke2-ingress-nginx-config.yaml
```

RKE2 will reconcile the change and restart the ingress controller automatically.
After this, every VTA Ingress gets HTTPS automatically — no `tls:` block or
cert-manager annotation required on individual Ingress resources.

### 2. HashiCorp Vault (one-time, before the API)

Each VTA's master seed is stored in HashiCorp Vault. Deploy it **before** the
API — the API needs the `vtafarm-api-vault` Secret that Vault's bootstrap
produces. Order matters: **transit Vault first, then the farm Vault.**

1. **`helm/vtafarm-transit`** — the in-cluster transit Vault that auto-unseals
   the farm Vault. Deploy, init + manually unseal, and bootstrap it. This
   creates the `vault-transit-token` Secret the farm Vault needs.
   See `helm/vtafarm-transit/README.md`.

2. **`helm/vtafarm-vault`** — the farm Vault that holds the seeds. Deploy, init,
   and run its bootstrap to enable KV v2 + Kubernetes auth + the AppRole, then
   create the `vtafarm-api-vault` Secret from the printed role-id/secret-id.
   See `helm/vtafarm-vault/README.md`.

> Each chart's README has the full step-by-step. Don't deploy the farm Vault
> before transit is up and bootstrapped, or it will stay sealed.

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

### 7. CI/CD (GitHub Actions)

`scripts/deploy.sh` automates the deploy step in CI. It expects the
following repository secrets:

| Secret | Description |
| --- | --- |
| `SSH_PRIVATE_KEY` | Private key with SSH access to the server |
| `SERVER_IP` | IP address of the Kubernetes server node |
| `KUBECONFIG_PATH` | Remote path to the kubeconfig on the server |
| `DOCKER_USERNAME` | Docker Hub username |
| `DOCKERHUB_TOKEN` | Docker Hub access token |

---

### Production Kubernetes RBAC

When deploying via Helm (`make deploy`), the ClusterRole below is created
automatically. For **test clusters or manual setups**, apply it by hand.

The master seed is stored in HashiCorp Vault (see `helm/vtafarm-vault`), not a
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
```
