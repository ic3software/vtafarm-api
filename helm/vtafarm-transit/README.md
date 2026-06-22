# vtafarm-transit

A standalone single-node **transit Vault** whose only job is to hold the
`autounseal` key that the **farm Vault** (`helm/vtafarm-vault`) uses to
auto-unseal. Runs in its own `vault-transit` namespace in the **same RKE2
cluster** — the zero-cost, all-self-hosted auto-unseal option.

Stores **no tenant data**. It is itself sealed with Shamir keys and unsealed
**manually** (it is the root of the unseal chain).

## ⚠️ What this does and does NOT protect

- ✅ **Avoids per-restart manual unsealing** of the farm Vault's 3 pods.
- ✅ **Protects data-at-rest** — a stolen farm-Vault backup/PVC alone can't be
  decrypted without this transit key.
- ❌ **Does NOT protect against a full live cluster compromise.** The unseal
  token lives in the cluster (`vault-transit-token`), so an attacker with
  cluster-admin can use it — same as cloud KMS would be. In-cluster transit is
  the *least* isolated option; it's chosen here purely for zero cost / no cloud.
- ⚠️ On a **full-cluster cold start** (everything reboots at once) you must
  **manually unseal this one transit Vault first**; the farm pods then
  auto-unseal from it.

To migrate to cloud KMS later, swap the farm Vault's `seal` stanza for
`seal "gcpckms"`/`"awskms"` and delete this chart — no other change.

## Contents

```text
helm/vtafarm-transit/
├── Chart.yaml             # umbrella chart; pins hashicorp/vault 0.33.0
├── values.yaml            # single-node, TLS, tiny resources
├── tls/cert-manager.yaml  # self-signed CA + server cert (vault-transit-tls)
├── networkpolicy.yaml     # only the `vault` namespace may reach :8200
├── bootstrap-transit.sh   # enable transit + key + policy + mint farm token
└── README.md              # this file
```

## Prerequisites

- `helm` and `kubectl` pointed at the cluster.
- **`vault` CLI installed locally** (used by `bootstrap-transit.sh`). macOS:
  `brew tap hashicorp/tap && brew install hashicorp/tap/vault`. Otherwise see
  <https://developer.hashicorp.com/vault/install>.
- **cert-manager installed** in the cluster (issues the transit TLS cert).

## Deployment order (important)

The transit Vault must be **up, unsealed, and bootstrapped** before the farm
Vault can unseal. Do this chart first, then `helm/vtafarm-vault`.

### 1. Pull the dependency + issue TLS

```bash
helm dependency update helm/vtafarm-transit

kubectl create namespace vault-transit
kubectl apply -n vault-transit -f helm/vtafarm-transit/tls/cert-manager.yaml
kubectl wait -n vault-transit --for=condition=Ready certificate/vault-transit-tls --timeout=120s
```

### 2. Install + lock down

```bash
helm install vault-transit helm/vtafarm-transit -n vault-transit -f helm/vtafarm-transit/values.yaml
kubectl apply -n vault-transit -f helm/vtafarm-transit/networkpolicy.yaml
```

### 3. Initialize + unseal (Shamir — manual)

```bash
kubectl exec -n vault-transit vault-transit-0 -- vault operator init -format=json > transit-init.json
```

⚠️ `transit-init.json` holds the unseal keys + root token for the transit
Vault. **Store offline, split among people, delete the local copy.** Losing
these means you cannot unseal transit (and thus cannot auto-unseal the farm).

Unseal with 3 of the 5 keys:

```bash
kubectl exec -n vault-transit vault-transit-0 -- vault operator unseal <key-1>
kubectl exec -n vault-transit vault-transit-0 -- vault operator unseal <key-2>
kubectl exec -n vault-transit vault-transit-0 -- vault operator unseal <key-3>
```

### 4. Bootstrap (enable transit + mint the farm token)

```bash
kubectl port-forward -n vault-transit svc/vault-transit 8210:8200 >/dev/null 2>&1 &

kubectl get secret -n vault-transit vault-transit-tls -o jsonpath='{.data.ca\.crt}' | base64 -d > /tmp/transit-ca.crt

export VAULT_ADDR=https://127.0.0.1:8210
export VAULT_CACERT=/tmp/transit-ca.crt
export VAULT_TOKEN=<root-token-from-transit-init.json>

chmod +x helm/vtafarm-transit/bootstrap-transit.sh
helm/vtafarm-transit/bootstrap-transit.sh
```

This enables the transit engine, creates the `autounseal` key + policy, mints an
orphan+periodic token, and writes it as the **`vault-transit-token`** secret in
the **`vault`** namespace. The farm Vault reads it via `VAULT_TOKEN` to
auto-unseal.

> Now continue with `helm/vtafarm-vault` — its `seal "transit"` stanza already
> points at `https://vault-transit.vault-transit.svc:8200` and consumes the
> `vault-transit-token` secret.

## Operations

- **Token renewal:** the farm Vault auto-renews the periodic token
  (`disable_renewal=false`). No manual rotation needed under normal operation.
  To rotate manually, re-run `bootstrap-transit.sh` and restart the farm pods.
- **Upgrades / restarts:** any restart of this Vault leaves it **sealed** →
  manually unseal it again (step 3). It rarely restarts; pin it to a stable node
  if you can. The farm Vault keeps running while transit is briefly down — it
  only needs transit during its own unseal events.
- **Backups:** `kubectl exec ... vault operator raft snapshot save` is N/A for
  file storage; back up the PVC. There's little state (one key) — what matters
  is the offline unseal keys + root token.
- **UI:** `kubectl port-forward -n vault-transit svc/vault-transit-ui 8210:8200`
  → `https://127.0.0.1:8210/ui/`.

## Cleanup (after bootstrap)

The port-forward from step 4 runs in the background (`&`) and is no longer
needed once the token secret exists — the farm Vault reaches transit in-cluster
via `vault-transit.vault-transit.svc`, not through your local port-forward.

```bash
# same terminal: list background jobs, then kill by number
jobs
kill %1                                   # use the port-forward's job number

# confirm nothing is listening on 8210
lsof -i :8210

# the local CA copy was only for the bootstrap CLI — safe to remove
rm -f /tmp/transit-ca.crt
```
