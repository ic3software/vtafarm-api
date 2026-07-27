package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/ic3software/vtafarm-api/internal/cloudflare"
	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/database"
	"github.com/ic3software/vtafarm-api/internal/didhosting"
	"github.com/ic3software/vtafarm-api/internal/ghcr"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/router"
	"github.com/ic3software/vtafarm-api/internal/setup"
	"github.com/ic3software/vtafarm-api/internal/upgrade"
	"github.com/ic3software/vtafarm-api/internal/vault"
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

	// Vault client provisions per-user secret isolation; setup requires it.
	var vaultClient *vault.Client
	if cfg.Vault.Addr != "" {
		vc, vErr := vault.New(vault.Config{
			Addr:         cfg.Vault.Addr,
			RoleID:       cfg.Vault.RoleID,
			SecretID:     cfg.Vault.SecretID,
			KVMount:      cfg.Vault.KVMount,
			K8sAuthMount: cfg.Vault.K8sAuthMount,
			AppRoleMount: cfg.Vault.AppRoleMount,
			SkipVerify:   cfg.Vault.SkipVerify,
		})
		if vErr != nil {
			log.Printf("warn: Vault client init failed: %v — vta setup disabled", vErr)
		} else {
			vaultClient = vc
			log.Printf("Vault client initialised (%s)", cfg.Vault.Addr)
		}
	} else {
		log.Printf("warn: VAULT_ADDR not set — vta setup disabled")
	}

	var orch *setup.Orchestrator
	if k8sClient != nil {
		orch = setup.NewOrchestrator(db, k8sClient, vaultClient, cfg.Vault.VTAAddr, dhClient,
			cfg.ClusterIngressIP, cfg.ACMEClusterIssuer)
		orch.Resume(context.Background())
	}

	// Upgrade runner processes admin image-upgrade batches in the background;
	// like the orchestrator it re-attaches interrupted work on startup.
	var upgradeRunner *upgrade.Runner
	if k8sClient != nil {
		upgradeRunner = upgrade.NewRunner(db, k8sClient)
		upgradeRunner.Resume()
	}

	// GHCR clients for listing available image tags (optional, one per component).
	var ghcrClient, mediatorGhcrClient, didsGhcrClient, vtcGhcrClient *ghcr.Client
	if cfg.GHCR.Owner != "" && cfg.GHCR.PackageName != "" {
		ghcrClient = ghcr.New(cfg.GHCR.Token, cfg.GHCR.Owner, cfg.GHCR.PackageName)
		log.Printf("GHCR image listing enabled for ghcr.io/%s/%s", cfg.GHCR.Owner, cfg.GHCR.PackageName)
	} else {
		log.Printf("warn: GITHUB_PACKAGE_OWNER or GITHUB_PACKAGE_NAME not set — vta image listing disabled")
	}
	if cfg.GHCR.Owner != "" && cfg.GHCR.MediatorPackageName != "" {
		mediatorGhcrClient = ghcr.New(cfg.GHCR.Token, cfg.GHCR.Owner, cfg.GHCR.MediatorPackageName)
		log.Printf("GHCR image listing enabled for ghcr.io/%s/%s", cfg.GHCR.Owner, cfg.GHCR.MediatorPackageName)
	} else {
		log.Printf("warn: GITHUB_PACKAGE_OWNER or GITHUB_MEDIATOR_PACKAGE_NAME not set — mediator image listing disabled")
	}
	if cfg.GHCR.Owner != "" && cfg.GHCR.DIDHostingDaemonPackageName != "" {
		didsGhcrClient = ghcr.New(cfg.GHCR.Token, cfg.GHCR.Owner, cfg.GHCR.DIDHostingDaemonPackageName)
		log.Printf("GHCR image listing enabled for ghcr.io/%s/%s", cfg.GHCR.Owner, cfg.GHCR.DIDHostingDaemonPackageName)
	} else {
		log.Printf("warn: GITHUB_PACKAGE_OWNER or GITHUB_DID_HOSTING_DAEMON_PACKAGE_NAME not set — dids image listing disabled")
	}
	if cfg.GHCR.Owner != "" && cfg.GHCR.VtcPackageName != "" {
		vtcGhcrClient = ghcr.New(cfg.GHCR.Token, cfg.GHCR.Owner, cfg.GHCR.VtcPackageName)
		log.Printf("GHCR image listing enabled for ghcr.io/%s/%s", cfg.GHCR.Owner, cfg.GHCR.VtcPackageName)
	} else {
		log.Printf("warn: GITHUB_PACKAGE_OWNER or GITHUB_VTC_PACKAGE_NAME not set — vtc image listing disabled")
	}

	r := router.Setup(db, cfClient, k8sClient, orch, upgradeRunner, ghcrClient, mediatorGhcrClient, didsGhcrClient, vtcGhcrClient, dhClient, cfg)

	srv := &http.Server{
		Addr:    ":" + cfg.AppPort,
		Handler: r,
	}

	go func() {
		log.Printf("server listening on :%s (env=%s)", cfg.AppPort, cfg.AppEnv)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("server forced shutdown: %v", err)
	}
}
