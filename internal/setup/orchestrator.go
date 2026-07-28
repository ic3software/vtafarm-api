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
	"github.com/ic3software/vtafarm-api/internal/dnscheck"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/vault"
)

// Orchestrator drives a setup session through its full lifecycle.
// Phase 1 (Start):      dns_provisioned → vta_setup_running → vta_setup_complete
// Phase 2 (Provision):  vta_setup_complete → provisioning → running
// Cancellation stops the goroutine; the Delete handler owns K8s + DB cleanup.
type Orchestrator struct {
	db        *gorm.DB
	k8s       *k8s.Client
	vault     *vault.Client // nil when VAULT_ADDR not configured
	vaultAddr string        // in-cluster Vault addr rendered into the VTA [secrets] block
	// didHosting builds a client for the control URL a session was provisioned
	// against, rather than one fixed at startup — the daemon a vta_only session
	// uploads to is the platform stack's, which does not exist on first boot.
	// nil when no client keypair is configured.
	didHosting *didhosting.Factory
	// ingressIP is what a custom domain's records must resolve to; acmeIssuer
	// names the cert-manager ClusterIssuer signing its certificate. Both are
	// used only by the custom-domain branches — dns_wait and tls_provision.
	ingressIP  string
	acmeIssuer string
	dns        *dnscheck.Checker
	mu         sync.Mutex
	cancels    map[uint]context.CancelFunc
}

func NewOrchestrator(
	db *gorm.DB,
	k8sClient *k8s.Client,
	vaultClient *vault.Client,
	vaultAddr string,
	dhFactory *didhosting.Factory,
	ingressIP, acmeIssuer string,
) *Orchestrator {
	return &Orchestrator{
		db:         db,
		k8s:        k8sClient,
		vault:      vaultClient,
		vaultAddr:  vaultAddr,
		didHosting: dhFactory,
		ingressIP:  ingressIP,
		acmeIssuer: acmeIssuer,
		dns:        dnscheck.New(),
		cancels:    make(map[uint]context.CancelFunc),
	}
}

// Start launches Phase 1 for a session. Safe to call from an HTTP handler.
// Dispatches to the full_stack state machine (orchestrator_fullstack.go),
// while vta_only runs the original runSetup below unchanged.
func (o *Orchestrator) Start(sessionID uint) {
	o.launch(sessionID, func(ctx context.Context) {
		switch o.sessionMode(sessionID) {
		case model.ModeFullStack:
			o.runFullStack(ctx, sessionID)
		default:
			o.runSetup(ctx, sessionID)
		}
	})
}

// Provision launches Phase 2 for a session. Called after the user provides
// their admin DID. Dispatches to the mode's finishing chain — full_stack's
// wraps the VTC steps around import-did + deploy_vta
// (orchestrator_vtc.go).
func (o *Orchestrator) Provision(sessionID uint, adminDid string) {
	o.launch(sessionID, func(ctx context.Context) {
		switch o.sessionMode(sessionID) {
		case model.ModeFullStack:
			o.runFullStackFinish(ctx, sessionID, adminDid)
		default:
			o.runProvision(ctx, sessionID, adminDid)
		}
	})
}

// sessionMode looks up a session's mode without loading the full row.
// Returns "" (never equal to a real mode) if the session can't be found.
func (o *Orchestrator) sessionMode(sessionID uint) string {
	var session model.SetupSession
	if err := o.db.Select("mode").First(&session, sessionID).Error; err != nil {
		return ""
	}
	return session.Mode
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

	o.resumeFullStack()
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
		// Extract path from the full URL e.g. https://dids.fpp2.ic3.dev/pvta-vta → pvta-vta
		path := session.VtaDidUrl
		if u, err := url.Parse(path); err == nil {
			path = strings.TrimPrefix(u.Path, "/")
		}
		// The daemon this session was provisioned against, not whichever one is
		// current — the two differ the moment the platform stack is rebuilt.
		dh, err := o.didHosting.For(session.DidHostingControlURL)
		if err != nil {
			log.Printf("[orchestrator] session %d: DID upload FAILED (no client for %q): %v",
				sessionID, session.DidHostingControlURL, err)
		} else {
			log.Printf("[orchestrator] session %d: uploading DID log to hosting service (path=%s)", sessionID, path)
			if err := dh.RegisterDid(ctx, path, didLog); err != nil {
				log.Printf("[orchestrator] session %d: DID upload FAILED: %v", sessionID, err)
			} else {
				log.Printf("[orchestrator] session %d: DID log uploaded to hosting service", sessionID)
			}
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

	// If did-hosting is configured: create the ACL entry first, then run the
	// combined provision job (import-did + did-mgmt servers add). The ACL must
	// exist before the VTA pod starts pushing DID updates.
	var controlDid string
	log.Printf("[orchestrator] session %d: did-hosting configured=%v vta_did=%q", sessionID, o.didHosting != nil, session.VtaDid)
	if o.didHosting != nil {
		dh, err := o.didHosting.For(session.DidHostingControlURL)
		if err != nil {
			o.markFailed(sessionID, "failed to reach DID hosting control API: "+err.Error())
			return
		}
		aclLabel := fmt.Sprintf("VTA user-%d session-%d", session.UserID, sessionID)
		log.Printf("[orchestrator] session %d: adding VTA DID to hosting ACL (did=%s label=%s)", sessionID, session.VtaDid, aclLabel)
		if err := dh.CreateAcl(ctx, session.VtaDid, "service", aclLabel); err != nil {
			if ctx.Err() != nil {
				return
			}
			o.markFailed(sessionID, "failed to add VTA DID to hosting ACL: "+err.Error())
			return
		}
		log.Printf("[orchestrator] session %d: VTA DID added to hosting ACL", sessionID)
		controlDid = dh.ServerDid()
	} else {
		log.Printf("[orchestrator] session %d: skipping did-hosting steps (no client keypair configured)", sessionID)
	}

	provisionJobName := k8s.ProvisionJobName(sessionID)
	log.Printf("[orchestrator] session %d: creating provision job (controlDid=%q)", sessionID, controlDid)
	if err := o.k8s.CreateProvisionJob(ctx, ns, sessionID, session.VtaImage, adminDid, controlDid); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "failed to create provision job: "+err.Error())
		return
	}

	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, provisionJobName)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "provision job watch error: "+err.Error())
		return
	}
	if !succeeded {
		if jobLogs, logsErr := o.k8s.JobLogs(ctx, ns, provisionJobName); logsErr == nil && jobLogs != "" {
			failMsg = failMsg + "\n\n--- Job Logs ---\n" + jobLogs
		}
		o.markFailed(sessionID, "provision job failed: "+failMsg)
		return
	}
	log.Printf("[orchestrator] session %d: provision job completed", sessionID)

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
