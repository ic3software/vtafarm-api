package main

import (
	"log"

	"github.com/joho/godotenv"

	"github.com/ic3software/cipherportal-api/internal/config"
	"github.com/ic3software/cipherportal-api/internal/database"
	"github.com/ic3software/cipherportal-api/internal/k8s"
	"github.com/ic3software/cipherportal-api/internal/router"
)

func main() {
	// Load .env file; safe to ignore in environments that use real env vars
	_ = godotenv.Load()

	cfg := config.Load()

	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("database connect: %v", err)
	}

	if err := database.Migrate(cfg); err != nil {
		log.Fatalf("database migrate: %v", err)
	}

	// K8s client is optional; API degrades gracefully if not configured
	k8sClient, err := k8s.NewClient(cfg)
	if err != nil {
		log.Printf("warn: k8s client unavailable (%v) — pod endpoints disabled", err)
	}

	r := router.Setup(db, k8sClient)

	log.Printf("server listening on :%s (env=%s)", cfg.AppPort, cfg.AppEnv)
	if err := r.Run(":" + cfg.AppPort); err != nil {
		log.Fatalf("server: %v", err)
	}
}
