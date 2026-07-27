#!/usr/bin/env bash
#
# One-time Vault configuration for the VTA Farm.
#
# Sets up everything vtafarm-api needs to provision per-user secret isolation:
#   1. KV v2 secrets engine at  secret/
#   2. Kubernetes auth method   (VTA pods authenticate with their SA JWT)
#   3. AppRole auth method       (vtafarm-api authenticates to manage policies/roles)
#   4. vtafarm-api-admin policy   (lets the API create per-user policies + k8s roles)
#   5. vtafarm-api AppRole        (prints role_id + secret_id for the API's Secret)
#
# It does NOT create per-user policies/roles or write any seed — those are done
# at runtime by vtafarm-api (internal/vault) and by `vta setup` respectively.
#
# Prerequisites:
#   - Vault is initialized AND unsealed.
#   - `vault` CLI installed locally.
#   - VAULT_ADDR + VAULT_TOKEN (root, or equivalently privileged) exported.
#     Vault serves https, so also export VAULT_CACERT (the CA from the vault-tls
#     secret). Typically via port-forward — see README.md step 5.
#
# Idempotent: re-running overwrites the policy and AppRole in place and skips
# already-enabled mounts. By default it generates a NEW secret_id every run;
# set SKIP_SECRET_ID=1 to update policy/role without rotating the secret_id.
set -euo pipefail

KV_MOUNT="${KV_MOUNT:-secret}"
K8S_AUTH_MOUNT="${K8S_AUTH_MOUNT:-kubernetes}"
APPROLE_MOUNT="${APPROLE_MOUNT:-approle}"
API_ROLE_NAME="${API_ROLE_NAME:-vtafarm-api}"
API_POLICY_NAME="${API_POLICY_NAME:-vtafarm-api-admin}"

: "${VAULT_ADDR:?set VAULT_ADDR (e.g. https://127.0.0.1:8200) + VAULT_CACERT}"
: "${VAULT_TOKEN:?set VAULT_TOKEN (root or admin token)}"

echo "==> Target: ${VAULT_ADDR}"
vault status >/dev/null

mount_exists() { # $1 = "secrets" | "auth", $2 = mount path
  vault "$1" list -format=json 2>/dev/null | grep -q "\"$2/\""
}

# 1. KV v2 secrets engine ------------------------------------------------------
if mount_exists secrets "${KV_MOUNT}"; then
  echo "==> KV mount '${KV_MOUNT}/' already enabled — skipping"
else
  echo "==> Enabling KV v2 at '${KV_MOUNT}/'"
  vault secrets enable -path="${KV_MOUNT}" -version=2 kv
fi

# 2. Kubernetes auth method ----------------------------------------------------
if mount_exists auth "${K8S_AUTH_MOUNT}"; then
  echo "==> Kubernetes auth '${K8S_AUTH_MOUNT}/' already enabled — skipping enable"
else
  echo "==> Enabling Kubernetes auth at '${K8S_AUTH_MOUNT}/'"
  vault auth enable -path="${K8S_AUTH_MOUNT}" kubernetes
fi

echo "==> Configuring Kubernetes auth (Vault uses its own SA as token reviewer)"
# No token_reviewer_jwt / CA: Vault uses its in-pod SA + the cluster CA to call
# TokenReview. Requires server.authDelegator.enabled (system:auth-delegator).
vault write "auth/${K8S_AUTH_MOUNT}/config" \
  kubernetes_host="https://kubernetes.default.svc"

# 3. AppRole auth method -------------------------------------------------------
if mount_exists auth "${APPROLE_MOUNT}"; then
  echo "==> AppRole auth '${APPROLE_MOUNT}/' already enabled — skipping"
else
  echo "==> Enabling AppRole auth at '${APPROLE_MOUNT}/'"
  vault auth enable -path="${APPROLE_MOUNT}" approle
fi

# 4. vtafarm-api admin policy --------------------------------------------------
# Scoped to the vta-user-* namespace pattern. The API never reads seeds, so no
# "read" on secret data — only the lifecycle operations it actually performs.
echo "==> Writing policy '${API_POLICY_NAME}'"
vault policy write "${API_POLICY_NAME}" - <<EOF
# Manage one ACL policy per user namespace (vta-user-<userID>). full_stack
# extends this same policy to also cover the user's mediator, dids and vtc KV
# prefixes (secret/{data,metadata}/{mediator,dids,vtc}/user-<id>/*) — see
# internal/vault.EnsureUserAccess. Those components all authenticate the same
# way the VTA does (kubernetes auth, same SA, same role), so there's no
# separate token-minting policy needed.
#
# Whenever EnsureUserAccess grows a KV prefix, this policy needs the matching
# teardown grant below. They are two different identities — the components hold
# the per-user kubernetes-auth token, the API holds this AppRole — so adding a
# prefix to one and not the other leaves secrets nobody can delete. That is
# exactly how vtc/* was missed: teardown logs the 403 as a warning and carries
# on, so the session disappears while its key bundle stays in Vault.
path "sys/policies/acl/vta-user-*" {
  capabilities = ["create", "read", "update", "delete"]
}

# Manage one Kubernetes auth role per user namespace
path "auth/${K8S_AUTH_MOUNT}/role/vta-user-*" {
  capabilities = ["create", "read", "update", "delete"]
}

# Clean up session seeds on teardown. No "read" — the API never needs the seed.
path "${KV_MOUNT}/data/vta/*" {
  capabilities = ["delete"]
}
path "${KV_MOUNT}/metadata/vta/*" {
  capabilities = ["delete"]
}

# full_stack: clean up mediator secrets on teardown. No "read" here either.
path "${KV_MOUNT}/data/mediator/*" {
  capabilities = ["delete"]
}
path "${KV_MOUNT}/metadata/mediator/*" {
  capabilities = ["delete"]
}

# full_stack: clean up dids daemon secrets on teardown. No "read" here either.
path "${KV_MOUNT}/data/dids/*" {
  capabilities = ["delete"]
}
path "${KV_MOUNT}/metadata/dids/*" {
  capabilities = ["delete"]
}

# full_stack: clean up the VTC key bundle on teardown. No "read" here either.
path "${KV_MOUNT}/data/vtc/*" {
  capabilities = ["delete"]
}
path "${KV_MOUNT}/metadata/vtc/*" {
  capabilities = ["delete"]
}
EOF

# 5. vtafarm-api AppRole -------------------------------------------------------
echo "==> Writing AppRole '${API_ROLE_NAME}'"
vault write "auth/${APPROLE_MOUNT}/role/${API_ROLE_NAME}" \
  token_policies="${API_POLICY_NAME}" \
  token_ttl=1h \
  token_max_ttl=4h \
  secret_id_ttl=0 \
  secret_id_num_uses=0

ROLE_ID="$(vault read -field=role_id "auth/${APPROLE_MOUNT}/role/${API_ROLE_NAME}/role-id")"

if [ "${SKIP_SECRET_ID:-0}" = "1" ]; then
  echo "==> SKIP_SECRET_ID=1 — not generating a new secret_id"
  echo ""
  echo "Role ID: ${ROLE_ID}"
  exit 0
fi

SECRET_ID="$(vault write -f -field=secret_id "auth/${APPROLE_MOUNT}/role/${API_ROLE_NAME}/secret-id")"

cat <<MSG

============================================================
 vtafarm-api Vault credentials
============================================================
  VAULT_ADDR       = ${VAULT_ADDR}
  VAULT_ROLE_ID    = ${ROLE_ID}
  VAULT_SECRET_ID  = ${SECRET_ID}

 DO NOT commit these. Store them as a Kubernetes Secret in the
 vtafarm-api namespace (default shown — change -n if you run the API elsewhere):

   kubectl create secret generic vtafarm-api-vault \\
     -n default \\
     --from-literal=role-id='${ROLE_ID}' \\
     --from-literal=secret-id='${SECRET_ID}'

 Note: VAULT_ADDR above is your local port-forward. In-cluster the
 API should use  https://vault.vault.svc:8200
============================================================
MSG
