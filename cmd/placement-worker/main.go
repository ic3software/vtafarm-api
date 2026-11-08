package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/database"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/placement"
)

func main() {
	cfg := config.Load()
	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("database connect: %v", err)
	}
	client, err := k8s.NewClient(cfg)
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
	}
	runner, err := placement.NewRunner(db, client, cfg.Placement)
	if err != nil {
		log.Fatalf("placement runner: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var leader atomic.Bool
	go healthServer(ctx, &leader, cfg.Placement.Mode)

	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}
	identity, _ := os.Hostname()
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: "vtafarm-placement-worker", Namespace: namespace},
		Client:     client.Kubernetes().CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock: lock, LeaseDuration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				leader.Store(true)
				runner.Run(leaderCtx)
			},
			OnStoppedLeading: func() { leader.Store(false) },
			OnNewLeader:      func(current string) { log.Printf("placement leader: %s", current) },
		},
	})
}

func healthServer(ctx context.Context, leader *atomic.Bool, mode string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "leader": leader.Load(), "mode": mode})
	})
	server := &http.Server{Addr: ":8081", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("placement health server: %v", err)
	}
}
