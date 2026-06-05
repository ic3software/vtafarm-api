package main

import (
	"log"

	"github.com/joho/godotenv"

	"github.com/ic3software/cipherportal-api/internal/cloudflare"
	"github.com/ic3software/cipherportal-api/internal/config"
	"github.com/ic3software/cipherportal-api/internal/database"
	"github.com/ic3software/cipherportal-api/internal/router"
)

func main() {
	_ = godotenv.Load()

	cfg := config.Load()

	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("database connect: %v", err)
	}

	if err := database.Migrate(cfg); err != nil {
		log.Fatalf("database migrate: %v", err)
	}

	// Cloudflare client is optional; setup endpoints return 503 if not configured
	var cfClient *cloudflare.Client
	if cfg.Cloudflare.APIToken != "" && cfg.Cloudflare.ZoneID != "" {
		cfClient = cloudflare.New(cfg.Cloudflare.APIToken, cfg.Cloudflare.ZoneID)
	} else {
		log.Printf("warn: CLOUDFLARE_API_TOKEN or CLOUDFLARE_ZONE_ID not set — setup endpoints disabled")
	}

	r := router.Setup(db, cfClient, cfg.AppEnv, cfg.ClusterIngressIP, cfg.JWTSecret)

	log.Printf("server listening on :%s (env=%s)", cfg.AppPort, cfg.AppEnv)
	if err := r.Run(":" + cfg.AppPort); err != nil {
		log.Fatalf("server: %v", err)
	}
}
