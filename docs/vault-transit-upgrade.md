# Vault / Transit Upgrade

Upgrade order: **first transit, then vault**.
Transit comes back **sealed** after a restart and must be **unsealed manually**;
the farm Vault then **auto-unseals** from it (no manual step).

---

## Before you start

- **Register the HashiCorp helm repo** (one-time per machine) — `helm dependency
  build` resolves the subchart through it, or it fails with `no repository
  definition`:

  ```bash
  helm repo add hashicorp https://helm.releases.hashicorp.com
  helm repo update
  ```

- **Have the transit unseal keys ready** — 3 of 5, from `transit-init.json`. You
  WILL need them in Step 2.
- **Take a farm snapshot — required, do not skip.** This is your restore point if
  the upgrade goes wrong:

  ```bash
  kubectl exec -n vault vault-0 -- sh -c 'VAULT_TOKEN=<token> vault operator raft snapshot save /tmp/pre.snap'
  kubectl cp vault/vault-0:/tmp/pre.snap ./pre.snap
  ```

---

## Step 1 — Upgrade transit (restarts the transit pod)

```bash
helm dependency build helm/vtafarm-transit
helm upgrade vault-transit helm/vtafarm-transit -n vault-transit \
  -f helm/vtafarm-transit/values.yaml

# The StatefulSet uses updateStrategy: OnDelete, so `helm upgrade` only stages the
# change — it does NOT restart the pod. Apply it by deleting the pod; the
# StatefulSet recreates it on the new spec:
kubectl delete pod vault-transit-0 -n vault-transit
kubectl get   pod vault-transit-0 -n vault-transit -w   # wait until Running
```

The pod restarts and comes back **sealed**:

```bash
kubectl exec -n vault-transit vault-transit-0 -- vault status   # Sealed=true
```

---

## Step 2 — Manually unseal transit

Run `vault operator unseal` three times, once per key:

```bash
kubectl exec -n vault-transit vault-transit-0 -- vault operator unseal <key-1>
kubectl exec -n vault-transit vault-transit-0 -- vault operator unseal <key-2>
kubectl exec -n vault-transit vault-transit-0 -- vault operator unseal <key-3>
kubectl exec -n vault-transit vault-transit-0 -- vault status   # Sealed=false
```

Do not continue until transit shows `Sealed=false` — the farm can only auto-unseal
while transit is up and unsealed.

---

## Step 3 — Upgrade farm / vault (restarts the vault pod)

```bash
helm dependency build helm/vtafarm-vault
helm upgrade vault helm/vtafarm-vault -n vault \
  -f helm/vtafarm-vault/values.yaml

# OnDelete again — delete the pod to apply (replicas: 1):
kubectl delete pod vault-0 -n vault
kubectl get   pod vault-0 -n vault -w    # wait until Ready 1/1 (auto-unseals)
```

The pod restarts and **auto-unseals from transit — no manual unseal needed**.

> At `replicas: 3`: delete standby pods one at a time (verify each is Ready +
> unsealed before the next), and delete the active/leader pod last.

---

## Step 4 — Verify

```bash
kubectl get pods -n vault                                  # vault-0 Ready 1/1
kubectl exec -n vault vault-0 -- vault status              # Sealed=false
```

---

## Node drain / full-cluster RKE2 upgrade

Simple path — accepts a brief farm outage while pods churn. Just upgrade the
cluster, then unseal transit once at the end; `vault-0` auto-unseals from it.

### Step 1 — Trigger the upgrade in Rancher

First take a farm snapshot and have the 3 transit unseal keys ready (see
"Before you start"). Then start the cluster upgrade in Rancher (concurrency 1 is
fine).

### Step 2 — If it stalls on vault-0, unblock it

If your upgrade strategy drains nodes, the drain of `vault-0`'s node **hangs** — its
**PodDisruptionBudget** (`maxUnavailable: 0`) blocks eviction. Delete the pod to let
the drain proceed (delete bypasses the PDB):

```bash
kubectl delete pod vault-0 -n vault
```

### Step 3 — Unseal transit, then verify

After the upgrade, transit is sealed; unseal it once and farm auto-unseals:

```bash
kubectl exec -n vault-transit vault-transit-0 -- vault operator unseal <key-1>
kubectl exec -n vault-transit vault-transit-0 -- vault operator unseal <key-2>
kubectl exec -n vault-transit vault-transit-0 -- vault operator unseal <key-3>

kubectl get nodes                                              # all Ready, new VERSION
kubectl exec -n vault-transit vault-transit-0 -- vault status  # Sealed=false
kubectl exec -n vault       vault-0          -- vault status   # Sealed=false, Ready
```

### Optional — zero-downtime (avoid the farm outage)

Only if you can't accept the brief farm outage above. The idea: keep transit alive
and unsealed the whole time, so farm never loses its unseal source.

Start the Rancher upgrade (concurrency 1) and wait for the **first** node to finish —
that Rancher-upgraded node is your **anchor**. (It must be Rancher-upgraded: Rancher
tracks "done" by a node label, not the version, so a hand-upgraded node gets
re-drained.) Pin transit to the anchor and unseal it once:

```bash
kubectl -n vault-transit patch statefulset vault-transit --type=merge \
  -p '{"spec":{"template":{"spec":{"nodeSelector":{"kubernetes.io/hostname":"<anchor>"}}}}}'
kubectl delete pod vault-transit-0 -n vault-transit
kubectl exec -n vault-transit vault-transit-0 -- vault operator unseal <key-1>
kubectl exec -n vault-transit vault-transit-0 -- vault operator unseal <key-2>
kubectl exec -n vault-transit vault-transit-0 -- vault operator unseal <key-3>
```

Let Rancher finish the rest; `kubectl delete pod vault-0 -n vault` when its node
drains (as in Step 2) — it re-lands and auto-unseals from the pinned transit. When
the whole upgrade is done, remove the pin:

```bash
kubectl -n vault-transit patch statefulset vault-transit --type=json \
  -p '[{"op":"remove","path":"/spec/template/spec/nodeSelector"}]'
```

---

## Restore from snapshot

Roll the farm's **data** back to the `pre.snap`. The
snapshot holds Vault's logical data (KV seeds, policies, auth) — **not** the binary
or cluster state. It **overwrites everything**, so anything written after the
snapshot is lost. Restore into a **running, unsealed** farm Vault with a root token:

```bash
kubectl cp ./pre.snap vault/vault-0:/tmp/pre.snap
kubectl exec -n vault vault-0 -- sh -c \
  'VAULT_TOKEN=<root-token> vault operator raft snapshot restore /tmp/pre.snap'
```
