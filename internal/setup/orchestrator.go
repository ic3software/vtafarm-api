package setup

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/k8s"
	"github.com/ic3software/cipherportal-api/internal/model"
)

// Orchestrator drives a setup session from dns_provisioned through to complete/failed.
// One goroutine per session; cancellation stops the goroutine without touching the DB
// (the Delete handler owns DB + K8s cleanup in that case).
type Orchestrator struct {
	db       *gorm.DB
	k8s      *k8s.Client
	vtaImage string
	mu       sync.Mutex
	cancels  map[uint]context.CancelFunc
}

func NewOrchestrator(db *gorm.DB, k8sClient *k8s.Client, vtaImage string) *Orchestrator {
	return &Orchestrator{
		db:       db,
		k8s:      k8sClient,
		vtaImage: vtaImage,
		cancels:  make(map[uint]context.CancelFunc),
	}
}

// Start launches a goroutine to drive session sessionID. Safe to call from an HTTP handler.
func (o *Orchestrator) Start(sessionID uint) {
	ctx, cancel := context.WithCancel(context.Background())

	o.mu.Lock()
	o.cancels[sessionID] = cancel
	o.mu.Unlock()

	go func() {
		defer func() {
			o.mu.Lock()
			delete(o.cancels, sessionID)
			o.mu.Unlock()
			cancel()
		}()
		o.run(ctx, sessionID)
	}()
}

// Cancel stops the goroutine for sessionID. Called by the Delete handler before it tears
// down DNS and K8s resources so the goroutine doesn't race with that cleanup.
func (o *Orchestrator) Cancel(sessionID uint) {
	o.mu.Lock()
	if cancel, ok := o.cancels[sessionID]; ok {
		cancel()
		delete(o.cancels, sessionID)
	}
	o.mu.Unlock()
}

// Resume re-attaches goroutines for any session stuck in vta_setup_running at startup.
// Called once from main() after the K8s client is ready.
func (o *Orchestrator) Resume(ctx context.Context) {
	var sessions []model.SetupSession
	if err := o.db.Where("status = ?", "vta_setup_running").Find(&sessions).Error; err != nil {
		log.Printf("[orchestrator] resume: query failed: %v", err)
		return
	}
	for _, s := range sessions {
		log.Printf("[orchestrator] resuming session %d", s.ID)
		o.Start(s.ID)
	}
}

// run is the state machine body. Advances dns_provisioned → vta_setup_running → complete/failed.
func (o *Orchestrator) run(ctx context.Context, sessionID uint) {
	var session model.SetupSession
	if err := o.db.First(&session, sessionID).Error; err != nil {
		log.Printf("[orchestrator] session %d: load failed: %v", sessionID, err)
		return
	}

	ns := o.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))

	// Ensure the user's K8s namespace exists.
	if err := o.k8s.EnsureUserEnvironment(ctx, fmt.Sprintf("%d", session.UserID)); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "failed to ensure k8s namespace: "+err.Error())
		return
	}

	// Render the VTA TOML config.
	toml, err := RenderVtaSetupTOML(&session)
	if err != nil {
		o.markFailed(sessionID, "failed to render TOML: "+err.Error())
		return
	}

	// Resolve the image: prefer the per-session choice, fall back to server default.
	vtaImage := session.VtaImage
	if vtaImage == "" {
		vtaImage = o.vtaImage
	}

	// Create ConfigMap + Job (idempotent on AlreadyExists).
	jobName := k8s.SetupJobName(sessionID)
	if err := o.k8s.CreateSetupResources(ctx, ns, sessionID, toml, vtaImage); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "failed to create k8s resources: "+err.Error())
		return
	}

	// Advance status so the frontend can show progress.
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"status":     "vta_setup_running",
		"updated_at": time.Now(),
	})

	log.Printf("[orchestrator] session %d: job %s started in namespace %s", sessionID, jobName, ns)

	// Wait for the job to reach a terminal state.
	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		if ctx.Err() != nil {
			return // session was cancelled/deleted
		}
		o.markFailed(sessionID, "job watch error: "+err.Error())
		return
	}
	if !succeeded {
		o.markFailed(sessionID, failMsg)
		return
	}

	// Parse the VTA DID from the job output.
	logs, err := o.k8s.JobLogs(ctx, ns, jobName)
	if err != nil {
		o.markFailed(sessionID, "failed to read job logs: "+err.Error())
		return
	}

	vtaDID, err := ParseVtaDID(logs)
	if err != nil {
		o.markFailed(sessionID, "job succeeded but VTA DID not found in output: "+err.Error())
		return
	}

	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"status":     "complete",
		"vta_did":    vtaDID,
		"updated_at": time.Now(),
	})
	log.Printf("[orchestrator] session %d: complete, VTA DID=%s", sessionID, vtaDID)
}

func (o *Orchestrator) markFailed(sessionID uint, msg string) {
	log.Printf("[orchestrator] session %d: failed: %s", sessionID, msg)
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"status":     "failed",
		"error_msg":  msg,
		"updated_at": time.Now(),
	})
}
