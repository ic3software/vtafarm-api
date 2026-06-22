#!/usr/bin/env bash
#
# One-time bootstrap for the transit Vault.
#
# Run AFTER the transit Vault is initialized + unsealed, with VAULT_ADDR /
# VAULT_TOKEN pointed at the TRANSIT Vault (via port-forward to svc/vault-transit
# in the vault-transit namespace). It:
#   1. enables the transit secrets engine
#   2. creates the `autounseal` key
#   3. writes the `autounseal` policy (encrypt/decrypt on that key only)
#   4. mints an orphan + periodic token
#   5. stores that token as the `vault-transit-token` secret in the FARM namespace
#      so the farm Vault can auto-unseal
#
# Idempotent — safe to re-run (it rotates the token and updates the secret).
set -euo pipefail

FARM_NS="${FARM_NS:-vault}"
TOKEN_SECRET="${TOKEN_SECRET:-vault-transit-token}"

: "${VAULT_ADDR:?point at the TRANSIT Vault, e.g. https://127.0.0.1:8200 + VAULT_CACERT}"
: "${VAULT_TOKEN:?transit root token from 'vault operator init'}"

echo "==> Target (transit): ${VAULT_ADDR}"
vault status >/dev/null

echo "==> Enabling transit engine"
vault secrets enable transit 2>/dev/null || echo "    (already enabled)"
vault write -f transit/keys/autounseal >/dev/null || true

echo "==> Writing 'autounseal' policy"
vault policy write autounseal - <<EOF
path "transit/encrypt/autounseal" { capabilities = ["update"] }
path "transit/decrypt/autounseal" { capabilities = ["update"] }
EOF

echo "==> Minting orphan + periodic token for the farm Vault"
TOKEN="$(vault token create -orphan -policy=autounseal -period=24h -field=token)"

echo "==> Ensuring namespace ${FARM_NS} exists"
kubectl create namespace "${FARM_NS}" --dry-run=client -o yaml | kubectl apply -f -

echo "==> Storing it as secret/${TOKEN_SECRET} in namespace ${FARM_NS}"
kubectl create secret generic "${TOKEN_SECRET}" -n "${FARM_NS}" \
  --from-literal=token="${TOKEN}" \
  --dry-run=client -o yaml | kubectl apply -f -

cat <<MSG

Done. The farm Vault's seal "transit" stanza will read this token (VAULT_TOKEN).
If the farm Vault is already deployed, restart its pods so they pick up the
secret:  kubectl -n ${FARM_NS} rollout restart statefulset/vault
MSG
