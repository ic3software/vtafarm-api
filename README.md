# CipherPortal API

Go REST API backend for managing Kubernetes pod deployments
with per-user namespace isolation.

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

The API runs directly on your machine (with Air hot reload) while only
the database runs in Docker. This gives the API direct access to your
local `~/.kube/config` without any networking workarounds.

### Prerequisites

- Go 1.26+
- Docker & Docker Compose
- `kubectl` configured with access to a cluster (for K8s features)

### Setup

1. Copy the example env file:

   ```bash
   cp .env.example .env
   ```

2. Start PostgreSQL:

   ```bash
   make up
   ```

3. Run migrations:

   ```bash
   make migrate
   ```

4. Seed test data:

   ```bash
   make seed
   ```

5. Start the API:

   ```bash
   make dev
   ```

   The API is now available at `http://localhost:8080`.

### Environment Variables

Copy `.env.example` and adjust as needed:

| Variable | Default | Notes |
| --- | --- | --- |
| `APP_PORT` | `8080` | HTTP listen port |
| `DB_HOST` | `localhost` | Points to the Docker-managed PostgreSQL |
| `DB_NAME` | `cipherportal` | |
| `KUBECONFIG` | _(empty)_ | Auto-detects `~/.kube/config` when empty |
| `K8S_NAMESPACE_PREFIX` | `cp-user` | Per-user namespace: `cp-user-{userID}` |

### Migration Workflow

```bash
make migrate-new NAME=add_users_table   # create a new migration pair
make migrate                             # apply pending migrations
make migrate-down                        # roll back one step
```

---

## Testing

### Unit / Integration Tests

```bash
go test ./...
```

### Manual API Testing

Health check:

```bash
curl http://localhost:8080/health
```

Create a pod (use the provided test fixture):

```bash
curl -X POST http://localhost:8080/api/v1/pods \
  -H "Content-Type: application/json" \
  -d @testdata/create-pod.json
```

List pods:

```bash
curl "http://localhost:8080/api/v1/pods?user_id=1"
```

Get a single pod:

```bash
curl "http://localhost:8080/api/v1/pods/nginx-test?user_id=1"
```

Delete a pod:

```bash
curl -X DELETE "http://localhost:8080/api/v1/pods/nginx-test?user_id=1"
```

---

## Production Deployment

The production stack is deployed to a Kubernetes cluster via Helm. The
chart (`helm/cipherportal-api`) ships both the API and a bundled
PostgreSQL instance as a single release.

### 1. Build and Push the Docker Image

Set your Docker Hub username and build:

```bash
export DOCKER_USERNAME=your-dockerhub-username

# builds + tags with git SHA and "latest", then pushes
make image-push
# or pin a specific tag:
make image-push TAG=v1.2.3
```

### 2. Connect to the Target Cluster

Ensure `kubectl` is pointing at the correct cluster:

```bash
kubectl config get-contexts             # list available contexts
kubectl config use-context <context>    # switch to the target cluster
kubectl cluster-info                    # verify connectivity
```

### 3. Deploy with Helm

`PG_PASSWORD` is the only required secret. Pass it directly on the
command line — it is stored as a Kubernetes Secret by the chart and
never written to disk.

**Minimal deploy (bundled PostgreSQL):**

```bash
make deploy \
  DOCKER_USERNAME=your-dockerhub-username \
  TAG=$(git rev-parse --short HEAD) \
  PG_PASSWORD=your-strong-password
```

**With a custom namespace:**

```bash
make deploy \
  DOCKER_USERNAME=your-dockerhub-username \
  TAG=$(git rev-parse --short HEAD) \
  PG_PASSWORD=your-strong-password \
  NAMESPACE=cipherportal
```

**With Ingress enabled**, first update `helm/cipherportal-api/values.yaml`:

```yaml
ingress:
  enabled: true
  className: nginx
  host: api.example.com
  tls: true   # requires a cert-manager TLS secret named cipherportal-tls
```

Then run `make deploy`.

**Uninstall:**

```bash
helm uninstall cipherportal
```

### 4. Run Migrations in the Cluster

After the first deploy, exec into the API pod to apply migrations:

```bash
kubectl exec -it deployment/cipherportal \
  -n <namespace> -- go run ./cmd/migrate up
```

### 5. CI/CD (GitHub Actions)

`scripts/deploy.sh` automates the deploy step in CI. It expects the
following repository secrets:

| Secret | Description |
| --- | --- |
| `SSH_PRIVATE_KEY` | Private key with SSH access to the server |
| `SERVER_IP` | IP address of the Kubernetes server node |
| `KUBECONFIG_PATH` | Remote path to the kubeconfig on the server |
| `DOCKER_USERNAME` | Docker Hub username |
| `PG_PASSWORD` | PostgreSQL password (first deploy or rotation) |

The script copies the kubeconfig from the server over SSH, rewrites the
API server URL from `127.0.0.1` to the public IP, installs `kubectl`
and `helm`, then runs `make deploy`.

### Production Kubernetes RBAC

The API server pod needs a `ClusterRole` with these permissions to
manage per-user namespaces:

```yaml
rules:
- apiGroups: [""]
  resources: ["namespaces", "serviceaccounts", "pods", "pods/log", "pods/exec"]
  verbs: ["get", "list", "create", "delete", "watch"]
- apiGroups: ["rbac.authorization.k8s.io"]
  resources: ["roles", "rolebindings"]
  verbs: ["get", "list", "create", "delete"]
```

This is provisioned automatically by the Helm chart via `clusterrole.yaml`
and `clusterrolebinding.yaml`.
