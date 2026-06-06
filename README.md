# CipherPortal API

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

3. (Optional) Seed test data — run in a separate terminal while the API is running:

   ```bash
   make seed
   ```

### Environment Variables

Copy `.env.example` and adjust as needed:

| Variable | Default | Notes |
| --- | --- | --- |
| `APP_PORT` | `8080` | HTTP listen port |
| `APP_ENV` | `development` | Set to `production` to disable `/docs` |
| `DB_HOST` | `localhost` | Points to the Docker-managed PostgreSQL |
| `DB_NAME` | `cipherportal` | |
| `JWT_SECRET` | _(required)_ | HS256 signing secret — see below |
| `CLUSTER_INGRESS_IP` | _(required)_ | External IP of the cluster's Ingress-NGINX LoadBalancer |
| `CLOUDFLARE_API_TOKEN` | _(optional)_ | Required for VTA setup wizard |
| `CLOUDFLARE_ZONE_ID` | _(optional)_ | Required for VTA setup wizard |
| `KUBECONFIG` | _(empty)_ | Auto-detects `~/.kube/config` when empty |
| `K8S_NAMESPACE_PREFIX` | `cp-user` | Per-user namespace: `cp-user-{userID}` |

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

The production stack is deployed to a Kubernetes cluster via Helm.

### 1. Create the PostgreSQL Secret (one-time)

```bash
kubectl create secret generic cipherportal-postgresql \
  --from-literal=postgres-password='your-strong-password' \
  --namespace=default
```

### 2. Deploy with Helm

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
  NAMESPACE=cipherportal
```

**Uninstall:**

```bash
helm uninstall cipherportal
```

### 3. Run Migrations in the Cluster

```bash
kubectl exec -it deployment/cipherportal \
  -n <namespace> -- go run ./cmd/migrate up
```

### 4. CI/CD (GitHub Actions)

`scripts/deploy.sh` automates the deploy step in CI. It expects the
following repository secrets:

| Secret | Description |
| --- | --- |
| `SSH_PRIVATE_KEY` | Private key with SSH access to the server |
| `SERVER_IP` | IP address of the Kubernetes server node |
| `KUBECONFIG_PATH` | Remote path to the kubeconfig on the server |
| `DOCKER_USERNAME` | Docker Hub username |
| `DOCKERHUB_TOKEN` | Docker Hub access token |

### Production Kubernetes RBAC

The API server pod needs a `ClusterRole` with these permissions:

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
