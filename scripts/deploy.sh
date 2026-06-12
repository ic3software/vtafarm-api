#!/bin/bash
set -e

# Required env vars (set by GitHub Actions):
#   SSH_PRIVATE_KEY, SERVER_IP, KUBECONFIG_PATH, DOCKER_USERNAME
# TAG defaults to the short git SHA via the Makefile (git rev-parse --short HEAD).
# Pre-requisite: Secret "vtafarm-api-postgresql" must exist in the cluster namespace.
#   See k8s/postgresql-secret.yaml — apply once manually before first deploy.

# ── SSH setup ─────────────────────────────────────────────────────────────────
echo "Setting up SSH..."
mkdir -p ~/.ssh
echo "$SSH_PRIVATE_KEY" > ssh_key
chmod 600 ssh_key
eval $(ssh-agent -s)
ssh-add ssh_key
ssh-keyscan -H "$SERVER_IP" >> ~/.ssh/known_hosts

# ── Copy kubeconfig from server ───────────────────────────────────────────────
echo "Copying kubeconfig..."
scp "root@${SERVER_IP}:${KUBECONFIG_PATH}" ./kubeconfig
export KUBECONFIG=./kubeconfig
sed -i "s|https://127.0.0.1:6443|https://${SERVER_IP}:6443|" ./kubeconfig

# ── Install kubectl ───────────────────────────────────────────────────────────
echo "Installing kubectl..."
curl -LO "https://storage.googleapis.com/kubernetes-release/release/\
$(curl -s https://storage.googleapis.com/kubernetes-release/release/stable.txt)\
/bin/linux/amd64/kubectl"
chmod +x ./kubectl
sudo mv ./kubectl /usr/local/bin/kubectl

# ── Install Helm ──────────────────────────────────────────────────────────────
echo "Installing Helm..."
curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

# ── Deploy (API + PostgreSQL as one release) ──────────────────────────────────
echo "Deploying vtafarm..."
make deploy \
  DOCKER_USERNAME="$DOCKER_USERNAME" \
  INGRESS_HOST="$INGRESS_HOST"

# ── Cleanup ───────────────────────────────────────────────────────────────────
eval $(ssh-agent -k)
rm -f ssh_key kubeconfig
echo "Done."
