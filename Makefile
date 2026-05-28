# ─── Image & deploy variables ─────────────────────────────────────────────────
NAME         ?= cipherportal
DOCKER_USERNAME ?=
IMAGE        ?= $(DOCKER_USERNAME)/cipherportal-api
TAG          ?= $(shell git rev-parse --short HEAD)
NAMESPACE    ?= default
DEPLOY_ENV   ?= production
INGRESS_HOST ?=

.PHONY: build tidy dev \
        migrate migrate-down migrate-new seed \
        up down reset \
        image-build image-push \
        deploy

# ─── Local ────────────────────────────────────────────────────────────────────
build:
	go build -o bin/api ./main.go

tidy:
	go mod tidy

# Start the API locally
dev:
	go run ./main.go

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

seed:
	go run ./seed

# ─── Docker Compose (DB only) ─────────────────────────────────────────────────
up:
	docker compose up -d

down:
	docker compose down

reset:
	docker compose down -v
	docker compose up -d

# ─── Docker Hub ───────────────────────────────────────────────────────────────
image-build:
	docker build -t $(IMAGE):$(TAG) -t $(IMAGE):latest .

image-push: image-build
	docker push $(IMAGE):$(TAG)
	docker push $(IMAGE):latest

# ─── Kubernetes (Helm) ────────────────────────────────────────────────────────
# Deploys both API and PostgreSQL as one release.
# helm uninstall cipherportal → removes everything.
# Pre-requisite: kubectl apply -f k8s/postgresql-secret.yaml (one-time, before first deploy)
# Usage: make deploy [DOCKER_USERNAME=xxx] [TAG=abc1234]
deploy:
	helm upgrade $(NAME) ./helm/cipherportal-api \
	  --set image.repository=$(IMAGE) \
	  --set image.tag=$(TAG) \
	  --set app.env=$(DEPLOY_ENV) \
	  --set ingress.enabled=true \
	  --set ingress.host=$(INGRESS_HOST) \
	  --install --atomic --timeout=10m \
	  --namespace=$(NAMESPACE)
