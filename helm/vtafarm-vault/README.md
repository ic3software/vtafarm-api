# vtafarm-vault

HashiCorp Vault for the VTA Farm. Stores each VTA's **master seed** (the root
of all its DIDs/keys) in Vault's KV v2 engine instead of a Kubernetes Secret.

This is **shared infrastructure**: one Vault cluster serves every vtafarm user.
Multi-tenancy is enforced with per-user policies + Kubernetes-auth roles, not
one-Vault-per-user. See the isolation model below.

This setup follows HashiCorp's official guidance:

- **Run Vault on Kubernetes:** <https://developer.hashicorp.com/vault/docs/deploy/kubernetes>
- **Vault on Kubernetes deployment guide:** <https://developer.hashicorp.com/vault/tutorials/kubernetes/kubernetes-raft-deployment-guide>

Where this chart deviates from the guide it is deliberate and noted inline
(e.g. 3 replicas, resources right-sized down for an early farm,
Agent Injector disabled because VTA reads its seed natively).

---

## Contents

```text
helm/vtafarm-vault/
├── Chart.yaml              # umbrella chart; pins hashicorp/vault 0.33.0 as a dependency
├── values.yaml             # HA + Raft + TLS + auto-unseal
├── tls/cert-manager.yaml   # self-signed CA + server cert (vault-tls secret); apply before install
├── bootstrap.sh            # one-time Vault config (run once per cluster after install)
└── README.md               # this file
```

Both the **dev** and **production** clusters are real clusters and use the
**same `values.yaml`** — the deployment is identical across environments. Each
cluster runs its own in-cluster transit Vault (`helm/vtafarm-transit`) for
auto-unseal; nothing in this `values.yaml` differs per cluster.

## How it fits together

```text
namespace: vault                         ← this chart (1 shared HA Vault)
   └── vault-0/1/2  (SA: vault)

namespace: vtafarm-user-abc123           ← tenant namespace (created by the API)
   └── VTA pod (SA: vta) ──┐
namespace: vtafarm-user-def456           │ cross-namespace k8s auth
   └── VTA pod (SA: vta) ──┴──► https://vault.vault.svc:8200
```

Each VTA pod authenticates to Vault with its ServiceAccount JWT. Vault's
Kubernetes-auth role binds `bound_service_account_namespaces` to the tenant's
namespace, and the attached policy scopes it to that tenant's seed paths only.

### Isolation model (decided)

- **One policy + one k8s-auth role per user namespace** — `vta-user-<userID>`.
- **One seed path per session** — `secret/data/vta/user-<userID>/session-<sessionID>/master-seed`.
- The policy uses a glob so all of a user's sessions share the role but live at
  distinct paths:

  ```hcl
  # policy: vta-user-abc123  (created at runtime by vtafarm-api)
  path "secret/data/vta/user-abc123/*"     { capabilities = ["read","create","update","delete"] }
  path "secret/metadata/vta/user-abc123/*" { capabilities = ["read","delete"] }
  ```

> Vault object count is O(users), not O(sessions). Cross-tenant isolation is
> enforced by Vault (namespace-bound auth). A user's own sessions share a trust
> domain, so distinct paths under one policy are enough.

---

## Step-by-step

> One identical flow per cluster. Run it once on **dev**, once on **production**
> — same chart, same `values.yaml`. Each cluster gets its own init/unseal and
> its own `bootstrap.sh` run (Vault state is per-cluster, not shared).

### 0. Prerequisites

- `helm` and `kubectl` pointed at the target cluster (repeat per cluster).
- `vault` CLI installed locally (for `bootstrap.sh`).
- **cert-manager installed** in the cluster (used to issue Vault's internal TLS
  cert — option B). No public issuer needed; `tls/cert-manager.yaml` stands up
  its own self-signed CA.
- **The transit Vault must be deployed + bootstrapped first** (see
  `helm/vtafarm-transit`). It provides auto-unseal for this farm Vault, so its
  `vault-transit-token` secret must exist in the `vault` namespace before you
  install/upgrade here. **Do not run the farm without auto-unseal** — every pod
  restart would otherwise block on manual unsealing. (To switch to cloud KMS
  later, swap the `seal` stanza in `values.yaml` — see the comment there.)

> Install with release name **`vault`** (the namespace is also `vault`). The
> chart pins resource names via `fullnameOverride: vault`, so the service is
> `vault.vault.svc` and the Raft peers are `vault-0.vault-internal` — matching
> the cert SANs and `retry_join` config.

### 1. Pull the dependency chart

```bash
helm dependency update helm/vtafarm-vault
```

This downloads `hashicorp/vault` (0.33.0) into `charts/` and writes `Chart.lock`
(commit the lock; `charts/` is gitignored).

### 2. Issue the internal TLS cert (cert-manager)

The Vault pods mount the `vault-tls` secret on startup, so it must exist
**before** install. Apply the CA chain and wait for the leaf cert:

```bash
kubectl apply -n vault -f helm/vtafarm-vault/tls/cert-manager.yaml
kubectl wait -n vault --for=condition=Ready certificate/vault-tls --timeout=120s
```

This creates a long-lived self-signed CA (`vault-ca`, 10y) and the auto-renewing
server cert (`vault-tls`, 90d). The CA is stable so a later move to full client
verification (option C) reuses it — no re-issue.

### 3. Install Vault

Make sure the transit Vault is already up and the `vault-transit-token` secret
exists in the `vault` namespace (see `helm/vtafarm-transit`). Then:

```bash
helm install vault helm/vtafarm-vault -n vault -f helm/vtafarm-vault/values.yaml
```

Pods start **sealed and not ready** until step 4.

### 4. Initialize + unseal

Initialize on the first pod (with auto-unseal this returns recovery keys):

```bash
kubectl exec -n vault vault-0 -- vault operator init -format=json > vault-init.json
```

⚠️ `vault-init.json` holds your recovery keys and initial root token.
**Store it offline / in a secret manager and delete the local copy.** It is
gitignored, but never commit it anywhere. Each cluster has its own keys.

Peers **auto-join** via the `retry_join` config — no manual
`vault operator raft join` needed. With auto-unseal they also unseal themselves
once the leader is initialized. (Without auto-unseal — not recommended — unseal
each pod manually: `vault operator unseal <key>` ×3 per pod.)

Confirm all three pods are `Running` and `READY 1/1`, and the Raft cluster has
three peers:

```bash
kubectl get pods -n vault
kubectl exec -n vault vault-0 -- vault status
```

`raft list-peers` needs the root token (single node just shows `vault-0` — optional):

```bash
TOKEN=        # paste the root token from vault-init.json
kubectl exec -n vault vault-0 -- env VAULT_TOKEN="$TOKEN" vault operator raft list-peers
```

### 5. Run the one-time bootstrap

Port-forward and export credentials, then run the script (once per cluster).
Vault now serves **https**, so point `VAULT_CACERT` at the CA from the secret:

```bash
kubectl port-forward -n vault svc/vault 8200:8200 >/dev/null 2>&1 &

kubectl get secret -n vault vault-tls -o jsonpath='{.data.ca\.crt}' | base64 -d > /tmp/vault-ca.crt

export VAULT_ADDR=https://127.0.0.1:8200
export VAULT_CACERT=/tmp/vault-ca.crt
chmod +x helm/vtafarm-vault/bootstrap.sh

export VAULT_TOKEN=        # paste the root token from vault-init.json
helm/vtafarm-vault/bootstrap.sh
```

This enables KV v2 + Kubernetes auth + AppRole, writes the `vtafarm-api-admin`
policy, and prints the API's `VAULT_ROLE_ID` / `VAULT_SECRET_ID`.

### 6. Store the API's Vault credentials

From the bootstrap output:

```bash
kubectl create secret generic vtafarm-api-vault \
  -n default \
  --from-literal=role-id='<VAULT_ROLE_ID>' \
  --from-literal=secret-id='<VAULT_SECRET_ID>'
```

These are what vtafarm-api uses to authenticate and provision per-user
policies/roles at runtime. **Never commit them.**

### Done

Steps 1–6 complete the Vault infrastructure: the farm Vault is running,
auto-unsealed via transit, and ready for vtafarm-api to use. ✅

### Verify later — after you create a VTA

There's nothing to verify here yet. End-to-end verification happens **once you
create a VTA through the vtafarm interface** — that triggers `vta setup`, which
writes the VTA's master seed into Vault. (This requires the app side wired to
Vault first; the VTA `[secrets]` block must set `vault_skip_verify = true` for
now, since the client can't yet be given the CA.)

After creating a VTA, confirm the seed landed and the pod authenticated:

```bash
vault kv get secret/vta/user-<userID>/session-<sessionID>/master-seed
```

The VTA pod logs should also show: `authenticated to Vault`.

## Cleanup (after bootstrap)

The port-forward from step 5 runs in the background (`&`) and is no longer
needed once setup is done — vtafarm-api reaches Vault via its own address
(`VAULT_ADDR`), not your local port-forward.

```bash
# same terminal: list background jobs, then kill by number
jobs
kill %1                                   # use the port-forward's job number

# confirm nothing is listening on 8200
lsof -i :8200

# the local CA copy was only for the bootstrap CLI — safe to remove
rm -f /tmp/vault-ca.crt
```

---

## Accessing the Vault UI

`ui = true` serves the web UI on the same HTTPS listener (port 8200, path
`/ui/`). The chart's `vault-ui` Service is **ClusterIP** — intentionally *not*
exposed outside the cluster. Reach it with a port-forward:

```bash
kubectl port-forward -n vault svc/vault-ui 8200:8200
# then open:  https://127.0.0.1:8200/ui/
```

- The cert is signed by the self-signed `vault-ca`, so the browser shows a
  "not trusted" warning — click through, or import `vault-ca`'s `ca.crt` into
  your OS/browser trust store to silence it:

  ```bash
  kubectl get secret -n vault vault-tls -o jsonpath='{.data.ca\.crt}' | base64 -d > vault-ca.crt
  ```

- **Log in with a token** — the root token from `vault-init.json`, or any token
  you mint later. (Avoid using the root token for day-to-day work.)

> Don't expose the UI via LoadBalancer/NodePort. If a team needs it, put it
> behind an internal ingress with your own auth — never publish a secrets
> store's UI to the internet.

---

## What the API does at runtime (reference, not a step)

`bootstrap.sh` only grants vtafarm-api the *ability* to provision tenants.
At runtime, on session/user create, the API (`internal/vault`) will:

1. `PUT sys/policies/acl/vta-user-<id>` — the per-user seed policy (glob above).
2. `POST auth/kubernetes/role/vta-user-<id>` — bound to
   `bound_service_account_names=vta`, `bound_service_account_namespaces=vtafarm-user-<id>`,
   `policies=vta-user-<id>`, `ttl=1h`.

`vta setup` (running in the tenant pod) then **creates** the seed at its
session path. On teardown the API deletes the seed and, for full user removal,
the policy + role.

---

## Operations notes

- **Lifecycle is decoupled from the app.** Do **not** wire this chart into the
  vtafarm-api CD pipeline. Deploy/upgrade it deliberately and rarely. A routine
  API deploy must never touch Vault's stateful resources.
- **Upgrades:** bump `dependencies[].version` in `Chart.yaml`, run
  `helm dependency update`, review the upstream changelog, then
  `helm upgrade vault helm/vtafarm-vault -n vault -f values.yaml`.
  Raft + auto-unseal makes the rolling restart non-disruptive.
- **Backups:** snapshot Raft regularly —
  `vault operator raft snapshot save snap.snap`. Keep recovery keys offline.
- **Secret-id rotation:** `bootstrap.sh` issues a non-expiring secret_id by
  default. To rotate, re-run it (generates a new one) and update the
  `vtafarm-api-vault` Secret; use `SKIP_SECRET_ID=1` to update policy/role
  without rotating.
