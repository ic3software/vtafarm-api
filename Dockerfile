# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.26.3-alpine3.23 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/bin/api    ./main.go && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/bin/enroll ./cmd/enroll

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
FROM alpine:3.23

WORKDIR /app

COPY --from=builder /app/bin/api    .
COPY --from=builder /app/bin/enroll .
COPY --from=builder /app/migrations ./migrations

EXPOSE 8080

CMD ["./api"]
