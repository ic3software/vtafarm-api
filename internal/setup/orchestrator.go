package setup

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/didhosting"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/vault"
)

// Orchestrator drives a setup session through its full lifecycle.
// Phase 1 (Start):      dns_provisioned → vta_setup_running → vta_setup_complete
// Phase 2 (Provision):  vta_setup_complete → provisioning → running
// Cancellation stops the goroutine; the Delete handler owns K8s + DB cleanup.
type Orchestrator struct {
	db         *gorm.DB
	k8s        *k8s.Client
	vault      *vault.Client      // nil when VAULT_ADDR not configured
	vaultAddr  string             // in-cluster Vault addr rendered into the VTA [secrets] block
	didHosting *didhosting.Client // nil when DID_HOSTING_CONTROL_URL not configured
	mu         sync.Mutex
	cancels    map[uint]context.CancelFunc
}

func NewOrchestrator(db *gorm.DB, k8sClient *k8s.Client, vaultClient *vault.Client, vaultAddr string, dhClient *didhosting.Client) *Orchestrator {
	return &Orchestrator{
		db:         db,
		k8s:        k8sClient,
		vault:      vaultClient,
		vaultAddr:  vaultAddr,
		didHosting: dhClient,
		cancels:    make(map[uint]context.CancelFunc),
	}
}

// Start launches Phase 1 for a session. Safe to call from an HTTP handler.
func (o *Orchestrator) Start(sessionID uint) {
	o.launch(sessionID, func(ctx context.Context) {
		o.runSetup(ctx, sessionID)
	})
}

// Provision launches Phase 2 for a session. Called after the user provides their admin DID.
func (o *Orchestrator) Provision(sessionID uint, adminDid string) {
	o.launch(sessionID, func(ctx context.Context) {
		o.runProvision(ctx, sessionID, adminDid)
	})
}

func (o *Orchestrator) launch(sessionID uint, fn func(context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())

	o.mu.Lock()
	if existing, ok := o.cancels[sessionID]; ok {
		existing()
	}
	o.cancels[sessionID] = cancel
	o.mu.Unlock()

	go func() {
		defer func() {
			o.mu.Lock()
			delete(o.cancels, sessionID)
			o.mu.Unlock()
			cancel()
		}()
		fn(ctx)
	}()
}

// TeardownVaultSeed deletes a session's master seed from Vault (best-effort).
// Called by the Delete handler. No-op when Vault isn't configured.
func (o *Orchestrator) TeardownVaultSeed(ctx context.Context, userID, sessionID uint) {
	if o.vault == nil {
		return
	}
	if err := o.vault.DeleteSeed(ctx, vault.SeedPath(userID, sessionID)); err != nil {
		log.Printf("[orchestrator] warn: delete vault seed (user %d session %d): %v", userID, sessionID, err)
	}
}

// TeardownVaultUserAccess removes a user's Vault policy + kubernetes-auth role
// (best-effort). Call only when the user has no remaining sessions. No-op when
// Vault isn't configured.
func (o *Orchestrator) TeardownVaultUserAccess(ctx context.Context, userID uint) {
	if o.vault == nil {
		return
	}
	if err := o.vault.DeleteUserAccess(ctx, userID); err != nil {
		log.Printf("[orchestrator] warn: delete vault user access (user %d): %v", userID, err)
	}
}

// Cancel stops the goroutine for sessionID (called by Delete handler).
func (o *Orchestrator) Cancel(sessionID uint) {
	o.mu.Lock()
	if cancel, ok := o.cancels[sessionID]; ok {
		cancel()
		delete(o.cancels, sessionID)
	}
	o.mu.Unlock()
}

// Resume re-attaches goroutines for sessions that were interrupted mid-run at startup.
func (o *Orchestrator) Resume(ctx context.Context) {
	var running []model.SetupSession
	if err := o.db.Where("status = ?", "vta_setup_running").Find(&running).Error; err != nil {
		log.Printf("[orchestrator] resume: query failed: %v", err)
	}
	for _, s := range running {
		log.Printf("[orchestrator] resuming setup session %d", s.ID)
		o.Start(s.ID)
	}

	var provisioning []model.SetupSession
	if err := o.db.Where("status = ? AND admin_did != ''", "provisioning").Find(&provisioning).Error; err != nil {
		log.Printf("[orchestrator] resume: query failed: %v", err)
	}
	for _, s := range provisioning {
		log.Printf("[orchestrator] resuming provision session %d", s.ID)
		o.Provision(s.ID, s.AdminDid)
	}
}

// ── Phase 1: vta setup job ────────────────────────────────────────────────────

func (o *Orchestrator) runSetup(ctx context.Context, sessionID uint) {
	var session model.SetupSession
	if err := o.db.First(&session, sessionID).Error; err != nil {
		log.Printf("[orchestrator] session %d: load failed: %v", sessionID, err)
		return
	}

	ns := o.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))

	if err := o.k8s.EnsureUserEnvironment(ctx, fmt.Sprintf("%d", session.UserID)); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "failed to ensure k8s namespace: "+err.Error())
		return
	}

	// Provision the user's Vault policy + kubernetes-auth role so the VTA pod
	// (running as the "vta" SA) can read/write only its own seed paths.
	if o.vault == nil {
		o.markFailed(sessionID, "vault not configured: set VAULT_ADDR/VAULT_ROLE_ID/VAULT_SECRET_ID")
		return
	}
	if err := o.vault.EnsureUserAccess(ctx, session.UserID, ns, k8s.VtaServiceAccount); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "failed to provision vault access: "+err.Error())
		return
	}

	toml, err := RenderVtaSetupTOML(&session, VaultSecrets{
		Addr:       o.vaultAddr,
		SecretPath: vault.SeedPath(session.UserID, sessionID),
		KVMount:    o.vault.KVMount(),
		K8sRole:    vault.UserName(session.UserID),
		SkipVerify: true,
	})
	if err != nil {
		o.markFailed(sessionID, "failed to render TOML: "+err.Error())
		return
	}

	jobName := k8s.SetupJobName(sessionID)
	if err := o.k8s.CreateSetupResources(ctx, ns, sessionID, toml, session.VtaImage); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "failed to create k8s resources: "+err.Error())
		return
	}

	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"status":     "vta_setup_running",
		"updated_at": time.Now(),
	})
	log.Printf("[orchestrator] session %d: setup job %s started in %s", sessionID, jobName, ns)

	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "job watch error: "+err.Error())
		return
	}
	if !succeeded {
		if jobLogs, logsErr := o.k8s.JobLogs(ctx, ns, jobName); logsErr == nil && jobLogs != "" {
			failMsg = failMsg + "\n\n--- Job Logs ---\n" + jobLogs
		}
		o.markFailed(sessionID, failMsg)
		return
	}

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

	// Extract did.jsonl content appended to the job logs after the marker.
	didLog := ParseVtaDidLog(logs)

	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"status":     "vta_setup_complete",
		"vta_did":    vtaDID,
		"updated_at": time.Now(),
	})
	log.Printf("[orchestrator] session %d: setup complete, VTA DID=%s", sessionID, vtaDID)

	log.Printf("[orchestrator] session %d: did-hosting=%v didLog_len=%d vtaDidUrl=%q",
		sessionID, o.didHosting != nil, len(didLog), session.VtaDidUrl)
	if o.didHosting != nil && didLog != "" && session.VtaDidUrl != "" {
		// Extract path from the full URL e.g. https://dids.fpp2.ic3.dev/abc123/pvta → abc123/pvta
		path := session.VtaDidUrl
		if u, err := url.Parse(path); err == nil {
			path = strings.TrimPrefix(u.Path, "/")
		}
		log.Printf("[orchestrator] session %d: uploading DID log to hosting service (path=%s)", sessionID, path)
		if err := o.didHosting.RegisterDid(ctx, path, didLog); err != nil {
			log.Printf("[orchestrator] session %d: DID upload FAILED: %v", sessionID, err)
		} else {
			log.Printf("[orchestrator] session %d: DID log uploaded to hosting service", sessionID)
		}
	} else if o.didHosting != nil {
		log.Printf("[orchestrator] session %d: skipping DID upload — didLog_empty=%v vtaDidUrl_empty=%v",
			sessionID, didLog == "", session.VtaDidUrl == "")
	}

	// Auto-trigger Phase 2 if admin_did was provided at session creation time.
	if session.AdminDid != "" {
		log.Printf("[orchestrator] session %d: admin_did present, auto-starting provisioning", sessionID)
		o.Provision(sessionID, session.AdminDid)
	}
}

// ── Phase 2: import-did + Deployment ─────────────────────────────────────────

func (o *Orchestrator) runProvision(ctx context.Context, sessionID uint, adminDid string) {
	var session model.SetupSession
	if err := o.db.First(&session, sessionID).Error; err != nil {
		log.Printf("[orchestrator] provision %d: load failed: %v", sessionID, err)
		return
	}

	ns := o.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))

	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"status":     "provisioning",
		"admin_did":  adminDid,
		"updated_at": time.Now(),
	})
	log.Printf("[orchestrator] session %d: provisioning, importing admin DID %s", sessionID, adminDid)

	// Run `vta import-did --did <adminDid> --role admin` as a K8s Job.
	if err := o.k8s.CreateImportDidJob(ctx, ns, sessionID, session.VtaImage, adminDid); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "failed to create import-did job: "+err.Error())
		return
	}

	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, k8s.ImportDidJobName(sessionID))
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "import-did job watch error: "+err.Error())
		return
	}
	if !succeeded {
		importJobName := k8s.ImportDidJobName(sessionID)
		if jobLogs, logsErr := o.k8s.JobLogs(ctx, ns, importJobName); logsErr == nil && jobLogs != "" {
			failMsg = "import-did job failed: " + failMsg + "\n\n--- Job Logs ---\n" + jobLogs
		} else {
			failMsg = "import-did job failed: " + failMsg
		}
		o.markFailed(sessionID, failMsg)
		return
	}

	log.Printf("[orchestrator] session %d: admin DID imported", sessionID)

	log.Printf("[orchestrator] session %d: did-hosting configured=%v vta_did=%q", sessionID, o.didHosting != nil, session.VtaDid)
	if o.didHosting != nil {
		// Add the VTA DID to the hosting control plane ACL so the VTA instance
		// can register and update its own DID entries directly.
		aclLabel := fmt.Sprintf("VTA user-%d session-%d", session.UserID, sessionID)
		log.Printf("[orchestrator] session %d: adding VTA DID to hosting ACL (did=%s label=%s)", sessionID, session.VtaDid, aclLabel)
		if err := o.didHosting.CreateAcl(ctx, session.VtaDid, "service", aclLabel); err != nil {
			if ctx.Err() != nil {
				return
			}
			o.markFailed(sessionID, "failed to add VTA DID to hosting ACL: "+err.Error())
			return
		}
		log.Printf("[orchestrator] session %d: VTA DID added to hosting ACL", sessionID)

		// Link VTA to the did-hosting control server so it knows where to push
		// DID updates.
		log.Printf("[orchestrator] session %d: creating did-mgmt servers add job (control-did=%s)", sessionID, o.didHosting.ServerDid())
		didMgmtJobName := k8s.DidMgmtServersAddJobName(sessionID)
		if err := o.k8s.CreateDidMgmtServersAddJob(ctx, ns, sessionID, session.VtaImage, o.didHosting.ServerDid()); err != nil {
			if ctx.Err() != nil {
				return
			}
			o.markFailed(sessionID, "failed to create did-mgmt servers add job: "+err.Error())
			return
		}
		log.Printf("[orchestrator] session %d: waiting for did-mgmt servers add job %s", sessionID, didMgmtJobName)
		succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, didMgmtJobName)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			o.markFailed(sessionID, "did-mgmt servers add job watch error: "+err.Error())
			return
		}
		if !succeeded {
			if jobLogs, logsErr := o.k8s.JobLogs(ctx, ns, didMgmtJobName); logsErr == nil && jobLogs != "" {
				failMsg = failMsg + "\n\n--- Job Logs ---\n" + jobLogs
			}
			o.markFailed(sessionID, "did-mgmt servers add job failed: "+failMsg)
			return
		}
		log.Printf("[orchestrator] session %d: vta did-mgmt servers add completed", sessionID)
	} else {
		log.Printf("[orchestrator] session %d: skipping did-hosting steps (DID_HOSTING_CONTROL_URL not configured)", sessionID)
	}

	log.Printf("[orchestrator] session %d: starting VTA deployment", sessionID)

	if err := o.k8s.CreateVtaDeployment(ctx, ns, sessionID, session.VtaImage); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "failed to create VTA deployment: "+err.Error())
		return
	}

	if err := o.k8s.CreateVtaService(ctx, ns, sessionID); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "failed to create VTA service: "+err.Error())
		return
	}

	if err := o.k8s.CreateVtaIngress(ctx, ns, sessionID, session.FQDN()); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "failed to create VTA ingress: "+err.Error())
		return
	}

	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"status":     "running",
		"updated_at": time.Now(),
	})
	log.Printf("[orchestrator] session %d: VTA running at %s", sessionID, session.FQDN())
}

func (o *Orchestrator) markFailed(sessionID uint, msg string) {
	log.Printf("[orchestrator] session %d: failed: %s", sessionID, msg)
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"status":     "failed",
		"error_msg":  msg,
		"updated_at": time.Now(),
	})
}
