package main

import (
	"context"
	"log"

	"github.com/joho/godotenv"

	"github.com/ic3software/cipherportal-api/internal/cloudflare"
	"github.com/ic3software/cipherportal-api/internal/config"
	"github.com/ic3software/cipherportal-api/internal/database"
	"github.com/ic3software/cipherportal-api/internal/ghcr"
	"github.com/ic3software/cipherportal-api/internal/k8s"
	"github.com/ic3software/cipherportal-api/internal/router"
	"github.com/ic3software/cipherportal-api/internal/setup"
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

	// Cloudflare client is optional; setup endpoints return 503 if not configured.
	var cfClient *cloudflare.Client
	if cfg.Cloudflare.APIToken != "" && cfg.Cloudflare.ZoneID != "" {
		cfClient = cloudflare.New(cfg.Cloudflare.APIToken, cfg.Cloudflare.ZoneID)
	} else {
		log.Printf("warn: CLOUDFLARE_API_TOKEN or CLOUDFLARE_ZONE_ID not set — setup endpoints disabled")
	}

	// K8s client is optional; setup execution requires it.
	var k8sClient *k8s.Client
	if k8sCli, k8sErr := k8s.NewClient(cfg); k8sErr != nil {
		log.Printf("warn: K8s client unavailable: %v — vta setup execution disabled", k8sErr)
	} else {
		k8sClient = k8sCli
		log.Printf("K8s client initialised")
	}

	// Orchestrator requires both K8s and a VTA image.
	var orch *setup.Orchestrator
	if k8sClient != nil && cfg.K8s.VTAImage != "" {
		orch = setup.NewOrchestrator(db, k8sClient, cfg.K8s.VTAImage)
		orch.Resume(context.Background())
	} else {
		log.Printf("warn: VTA orchestrator disabled (set VTA_IMAGE and ensure K8s is reachable)")
	}

	// GHCR client for listing available image tags (optional).
	var ghcrClient *ghcr.Client
	if cfg.GHCR.Owner != "" && cfg.GHCR.PackageName != "" {
		ghcrClient = ghcr.New(cfg.GHCR.Token, cfg.GHCR.Owner, cfg.GHCR.PackageName)
		log.Printf("GHCR image listing enabled for ghcr.io/%s/%s", cfg.GHCR.Owner, cfg.GHCR.PackageName)
	} else {
		log.Printf("warn: GITHUB_PACKAGE_OWNER or GITHUB_PACKAGE_NAME not set — image listing disabled")
	}

	r := router.Setup(db, cfClient, k8sClient, orch, ghcrClient, cfg.K8s.VTAImage, cfg.AppEnv, cfg.ClusterIngressIP, cfg.JWTSecret)

	log.Printf("server listening on :%s (env=%s)", cfg.AppPort, cfg.AppEnv)
	if err := r.Run(":" + cfg.AppPort); err != nil {
		log.Fatalf("server: %v", err)
	}
}
