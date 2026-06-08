package main

import (
	"context"
	"log"

	"github.com/joho/godotenv"

	"github.com/ic3software/cipherportal-api/internal/cloudflare"
	"github.com/ic3software/cipherportal-api/internal/config"
	"github.com/ic3software/cipherportal-api/internal/database"
	"github.com/ic3software/cipherportal-api/internal/didhosting"
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

	var dhClient *didhosting.Client
	if cfg.DidHosting.ControlUrl != "" && cfg.DidHosting.Did != "" && cfg.DidHosting.PrivateKey != "" {
		var dhErr error
		dhClient, dhErr = didhosting.New(cfg.DidHosting.ControlUrl, cfg.DidHosting.Did, cfg.DidHosting.PrivateKey)
		if dhErr != nil {
			log.Printf("warn: DID hosting client init failed: %v — auto-upload disabled", dhErr)
			dhClient = nil
		} else {
			log.Printf("DID hosting auto-upload enabled (%s)", cfg.DidHosting.ControlUrl)
		}
	} else {
		log.Printf("warn: DID_HOSTING_CONTROL_URL/DID/PRIVATE_KEY not set — DID auto-upload disabled")
	}

	var orch *setup.Orchestrator
	if k8sClient != nil {
		orch = setup.NewOrchestrator(db, k8sClient, dhClient)
		orch.Resume(context.Background())
	}

	// GHCR client for listing available image tags (optional).
	var ghcrClient *ghcr.Client
	if cfg.GHCR.Owner != "" && cfg.GHCR.PackageName != "" {
		ghcrClient = ghcr.New(cfg.GHCR.Token, cfg.GHCR.Owner, cfg.GHCR.PackageName)
		log.Printf("GHCR image listing enabled for ghcr.io/%s/%s", cfg.GHCR.Owner, cfg.GHCR.PackageName)
	} else {
		log.Printf("warn: GITHUB_PACKAGE_OWNER or GITHUB_PACKAGE_NAME not set — image listing disabled")
	}

	r := router.Setup(db, cfClient, k8sClient, orch, ghcrClient, dhClient, cfg.AppEnv, cfg.ClusterIngressIP, cfg.ClusterDomain, cfg.MediatorDid, cfg.DidHosting.ServerUrl, cfg.JWTSecret)

	log.Printf("server listening on :%s (env=%s)", cfg.AppPort, cfg.AppEnv)
	if err := r.Run(":" + cfg.AppPort); err != nil {
		log.Fatalf("server: %v", err)
	}
}
