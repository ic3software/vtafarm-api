# ─── Image & deploy variables ─────────────────────────────────────────────────
NAME         ?= vtafarm-api
DOCKER_USERNAME ?=
IMAGE        ?= $(DOCKER_USERNAME)/$(NAME)
TAG          ?= $(shell git rev-parse --short HEAD)
NAMESPACE    ?= default
DEPLOY_ENV   ?= production
INGRESS_HOST ?=

# ─── Dev cluster ──────────────────────────────────────────────────────────────
# The database is shared and lives here — see docs/shared-dev-database.md.
DEV_CONTEXT  ?= k8s-fpp-dev
DEV_DB       ?= vtafarm-dev-postgres
DB_PORT      ?= 5432
VAULT_PORT   ?= 8200

.PHONY: build test check-pg-image gen-keypair tidy dev \
        migrate migrate-down migrate-new enroll enroll-prod \
        deploy-db forward-db forward-vault \
        image-build image-push \
        deploy

# ─── Local ────────────────────────────────────────────────────────────────────
build:
	go build -o bin/api ./main.go

# Same checks CI runs (.github/workflows)
test: check-pg-image
	go vet ./...
	go test ./...

# Dev and production must run the identical PostgreSQL image — a version that
# only differs locally turns "works on dev" into a guess. Enforced here rather
# than by convention, because the two files are edited months apart.
check-pg-image:
	@dev=$$(grep -o 'postgres:[0-9a-z.-]*' k8s/dev-postgres/deployment.yaml); \
	prod=$$(grep -o 'postgres:[0-9a-z.-]*' helm/vtafarm-api/values.yaml); \
	if [ "$$dev" != "$$prod" ]; then \
	  echo "PostgreSQL image mismatch:"; \
	  echo "  k8s/dev-postgres/deployment.yaml : $$dev"; \
	  echo "  helm/vtafarm-api/values.yaml     : $$prod"; \
	  exit 1; \
	fi; \
	echo "PostgreSQL image matches in dev and production: $$dev"

gen-keypair:
	go run ./cmd/gen-keypair

tidy:
	go mod tidy

# Start the API with Air hot-reload. The database is the shared one in the dev
# cluster, so `make forward-db` must already be running in another terminal —
# checked here because otherwise the failure is a bare "connection refused".
dev:
	@nc -z localhost $(DB_PORT) 2>/dev/null || { \
	  echo "Nothing listening on localhost:$(DB_PORT)."; \
	  echo "Start the tunnel to the shared dev database first, in another terminal:"; \
	  echo ""; \
	  echo "    make forward-db"; \
	  echo ""; \
	  exit 1; \
	}
	air

# ─── Migrations (run locally against DB_HOST=localhost) ───────────────────────
# Usage: make migrate-new NAME=create_users
migrate-new:
	@test -n "$(NAME)" || (echo "Usage: make migrate-new NAME=<name>"; exit 1)
	@n=$$(ls migrations/*.up.sql 2>/dev/null | wc -l | tr -d ' '); \
	seq=$$(printf "%06d" $$((n + 1))); \
	touch migrations/$${seq}_$(NAME).up.sql migrations/$${seq}_$(NAME).down.sql; \
	echo "Created migrations/$${seq}_$(NAME).{up,down}.sql"

migrate:
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate down

enroll:
	go run ./cmd/enroll

enroll-prod:
	kubectl exec -n $(NAMESPACE) deploy/$(NAME) -- ./enroll

# ─── Dev cluster ──────────────────────────────────────────────────────────────
# Deploy / update the shared database. Applies only — the PVC is never deleted
# here, so team data survives every redeploy. The context is explicit so this
# can't land in docker-desktop by accident.
deploy-db:
	kubectl --context $(DEV_CONTEXT) apply -f k8s/dev-postgres/

# Tunnels. Keep each running in its own terminal while developing. The loops are
# not cosmetic: kubectl port-forward dies on a dropped connection or a pod
# restart and never comes back on its own.
forward-db:
	@echo "Forwarding $(DEV_CONTEXT) svc/$(DEV_DB) → localhost:$(DB_PORT)  (Ctrl-C to stop)"
	@trap 'exit 0' INT; while true; do \
	  kubectl --context $(DEV_CONTEXT) port-forward svc/$(DEV_DB) $(DB_PORT):5432 || true; \
	  echo "port-forward dropped — reconnecting in 2s"; \
	  sleep 2; \
	done

forward-vault:
	@echo "Forwarding $(DEV_CONTEXT) vault/svc/vault → localhost:$(VAULT_PORT)  (Ctrl-C to stop)"
	@trap 'exit 0' INT; while true; do \
	  kubectl --context $(DEV_CONTEXT) port-forward -n vault svc/vault $(VAULT_PORT):8200 || true; \
	  echo "port-forward dropped — reconnecting in 2s"; \
	  sleep 2; \
	done

# ─── Docker Hub ───────────────────────────────────────────────────────────────
image-build:
	docker build -t $(IMAGE):$(TAG) -t $(IMAGE):latest .

image-push: image-build
	docker push $(IMAGE):$(TAG)
	docker push $(IMAGE):latest

# ─── Kubernetes (Helm) ────────────────────────────────────────────────────────
# Deploys both API and PostgreSQL as one release.
# helm uninstall vtafarm-api → removes everything.
# Pre-requisite: kubectl apply -f k8s/postgresql-secret.yaml (one-time, before first deploy)
# Usage: make deploy [DOCKER_USERNAME=xxx] [TAG=abc1234]
deploy:
	helm upgrade $(NAME) ./helm/vtafarm-api \
	  --set image.repository=$(IMAGE) \
	  --set image.tag=$(TAG) \
	  --set app.env=$(DEPLOY_ENV) \
	  --set ingress.host=$(INGRESS_HOST) \
	  --install --atomic --timeout=10m \
	  --namespace=$(NAMESPACE)
