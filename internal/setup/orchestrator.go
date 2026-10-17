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

	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/didhosting"
	"github.com/ic3software/vtafarm-api/internal/dnscheck"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/resourceprofile"
	"github.com/ic3software/vtafarm-api/internal/vault"
	"github.com/ic3software/vtafarm-api/internal/webvh"
)

// Orchestrator drives a setup session through its full lifecycle.
// Phase 1 (Start):      dns_provisioned → vta_setup_running → vta_setup_complete
// External DID hosting waits at awaiting_did_publication before completion.
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
	provisions map[uint]context.CancelFunc
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
	o.queueProvision(sessionID, adminDid)
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

// TeardownVaultSecrets deletes all of a session's VTA secrets from Vault (best-effort).
// Called by the Delete handler. No-op when Vault isn't configured.
func (o *Orchestrator) TeardownVaultSecrets(ctx context.Context, userID, sessionID uint) {
	if o.vault == nil {
		return
	}
	if err := o.vault.DeleteVtaSecrets(ctx, userID, sessionID); err != nil {
		log.Printf("[orchestrator] warn: delete VTA vault secrets (user %d session %d): %v", userID, sessionID, err)
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
func (o *Orchestrator) Cancel(sessionID uint) error {
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	if err := connection.Stop(ctx, o.db, sessionID); err != nil {
		return err
	}
	o.mu.Lock()
	if cancel, ok := o.provisions[sessionID]; ok {
		cancel()
	}
	if cancel, ok := o.cancels[sessionID]; ok {
		cancel()
		delete(o.cancels, sessionID)
	}
	o.mu.Unlock()
	// A worker on another replica observes the deleting state on its next
	// heartbeat. Wait for its lock before deleting the resources it can create.
	return o.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return tx.Exec("SELECT pg_advisory_xact_lock($1, $2)", 565441, int64(sessionID)).Error
	})
}

// Resume re-attaches goroutines for sessions that were interrupted mid-run at startup.
func (o *Orchestrator) Resume(ctx context.Context) {
	var running []model.SetupSession
	query := o.db.Where("status = ?", "vta_setup_running")
	if o.db.Migrator().HasTable(&model.PoolSession{}) {
		query = query.Where("NOT EXISTS (SELECT 1 FROM pool_sessions WHERE session_id = setup_sessions.id)")
	}
	if err := query.Find(&running).Error; err != nil {
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
	pooled, err := o.isPoolSession(ctx, sessionID)
	if err != nil {
		log.Printf("[orchestrator] session %d: pool membership lookup failed: %v", sessionID, err)
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

	if session.ConnectionSource == model.ConnectionExternal || pooled {
		if didLog == "" || len(didLog) >= MaxVtaDIDLogBytes {
			o.markFailed(sessionID, "VTA setup produced no usable DID log for external publication")
			return
		}
	}
	readyStatus := "vta_setup_complete"
	storedDidLog := ""
	if session.ConnectionSource == model.ConnectionExternal {
		readyStatus = "awaiting_did_publication"
		storedDidLog = didLog
	}
	if pooled {
		readyStatus, storedDidLog = model.StatusPoolPublishing, didLog
	}
	if err := o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"status":      readyStatus,
		"vta_did":     vtaDID,
		"vta_did_log": storedDidLog,
		"updated_at":  time.Now(),
	}).Error; err != nil {
		o.markFailed(sessionID, "failed to save VTA setup result: "+err.Error())
		return
	}
	log.Printf("[orchestrator] session %d: setup complete, VTA DID=%s", sessionID, vtaDID)
	if pooled {
		if err := o.publishPoolDID(ctx, sessionID); err != nil && ctx.Err() == nil {
			o.markFailed(sessionID, err.Error())
		}
		return
	}
	if session.ConnectionSource == model.ConnectionExternal {
		return
	}

	// Publishing the DID log is not a best-effort side errand: an unpublished
	// did:webvh cannot be resolved, so the agent this session is building can
	// never be reached. When a hosting client is configured, publication
	// failures below fail the session.
	//
	// This must stay AFTER the vta_setup_complete write above. Resume only
	// re-runs sessions still in vta_setup_running, so a session that reaches
	// here is never replayed through it — which is what keeps the upload from
	// happening twice. registerAtomic sends force=false and errors on any
	// non-2xx, so a second attempt at an already-published path would fail, and
	// with the change above that failure would now kill an otherwise healthy
	// session. Moving the status write later requires making the upload
	// idempotent first.
	//
	// A crash between the status write and upload leaves a session in
	// vta_setup_complete with no published DID and no retry.
	log.Printf("[orchestrator] session %d: did-hosting=%v didLog_len=%d vtaDidUrl=%q",
		sessionID, o.didHosting != nil, len(didLog), session.VtaDidUrl)
	if o.didHosting != nil {
		switch {
		case didLog == "":
			// `vta setup` reported a DID but no did.jsonl followed it in the
			// logs. Nothing to publish, and nothing that publishes it later.
			o.markFailedAt(sessionID, "vta_setup_running", "VTA setup produced no DID log to publish — the agent's DID would never resolve")
			return
		case session.VtaDidUrl == "":
			o.markFailedAt(sessionID, "vta_setup_running", "session has no VTA DID URL — cannot publish the agent's DID")
			return
		}

		// Extract path from the full URL e.g. https://dids.fpp2.ic3.dev/pvta-vta → pvta-vta
		path := session.VtaDidUrl
		if u, err := url.Parse(path); err == nil {
			path = strings.TrimPrefix(u.Path, "/")
		}
		// The daemon this session was provisioned against, not whichever one is
		// current — the two differ the moment the platform stack is rebuilt.
		dh, err := o.didHosting.For(session.DidHostingControlURL, session.DIDHostingDid)
		if err != nil {
			o.markFailedAt(sessionID, "vta_setup_running", "cannot reach the DID hosting control API at "+
				session.DidHostingControlURL+": "+err.Error())
			return
		}
		log.Printf("[orchestrator] session %d: uploading DID log to hosting service (path=%s)", sessionID, path)
		if err := dh.RegisterDid(ctx, path, didLog); err != nil {
			if ctx.Err() != nil {
				return
			}
			o.markFailedAt(sessionID, "vta_setup_running", "failed to publish the agent's DID to "+
				session.DidHostingControlURL+": "+err.Error())
			return
		}
		log.Printf("[orchestrator] session %d: DID log uploaded to hosting service", sessionID)
	} else {
		// No keypair configured at all. Left as a warning rather than a failure
		// because it is a deployment-wide state, not a property of this session
		// — runProvision's ACL step already treats it the same way, and failing
		// here would break every local environment that runs without one.
		log.Printf("[orchestrator] session %d: DID_HOSTING_DID unset — the agent's DID will not be published "+
			"and will not resolve", sessionID)
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

	// Farm-managed hosting gets an ACL entry before the VTA starts pushing DID
	// updates. For external hosting the owner manages that ACL; only its DID is
	// passed to the provision job.
	var controlDid string
	log.Printf("[orchestrator] session %d: did-hosting configured=%v vta_did=%q", sessionID, o.didHosting != nil, session.VtaDid)
	if session.ConnectionSource == model.ConnectionExternal {
		controlDid = session.DIDHostingDid
		log.Printf("[orchestrator] session %d: external DID host; owner manages its ACL", sessionID)
	} else if o.didHosting != nil {
		dh, err := o.didHosting.For(session.DidHostingControlURL, session.DIDHostingDid)
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
	adminLabel := o.provisionAdminLabel(sessionID, adminDid, "")
	if err := o.k8s.CreateProvisionJob(ctx, ns, sessionID, session.VtaImage, adminDid, adminLabel, controlDid); err != nil {
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
	pooled, err := o.deployVtaOnly(ctx, ns, &session)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailedAt(sessionID, "deploy_vta", err.Error())
		return
	}

	o.finishVtaOnlyDeployment(ctx, &session, pooled)
}

func (o *Orchestrator) deployVtaOnly(ctx context.Context, ns string, session *model.SetupSession) (bool, error) {
	profiles, err := resourceprofile.Load(ctx, o.db)
	if err != nil {
		return false, fmt.Errorf("failed to load resource defaults: %w", err)
	}
	pooled, err := o.isPoolSession(ctx, session.ID)
	if err != nil {
		return false, fmt.Errorf("failed to identify pool session: %w", err)
	}
	if pooled {
		var saved model.WorkloadResource
		if err := o.db.Where("setup_session_id = ? AND component = 'vta'", session.ID).First(&saved).Error; err != nil {
			return false, fmt.Errorf("pool resource profile is missing: %w", err)
		}
		p := profiles[k8s.ComponentVTA]
		p.MemoryRequest, p.MemoryLimit = saved.MemoryRequest, saved.MemoryLimit
		profiles[k8s.ComponentVTA] = p
	}
	if err := o.k8s.CreateVtaDeployment(ctx, ns, session.ID, session.VtaImage, profiles.Requirements(k8s.ComponentVTA)); err != nil {
		return pooled, fmt.Errorf("failed to create VTA deployment: %w", err)
	}
	if pooled {
		if err := o.k8s.ScaleComponentDeployment(ctx, ns, k8s.VtaDeploymentName(session.ID), 1); err != nil {
			return pooled, fmt.Errorf("failed to start prebuilt VTA deployment: %w", err)
		}
	}
	if err := o.k8s.CreateVtaService(ctx, ns, session.ID); err != nil {
		return pooled, fmt.Errorf("failed to create VTA service: %w", err)
	}
	if err := o.k8s.CreateVtaIngress(ctx, ns, session.ID, session.FQDN()); err != nil {
		return pooled, fmt.Errorf("failed to create VTA ingress: %w", err)
	}

	// A Deployment exists before the VTA has finished initializing. Its
	// /health readiness probe is the serving boundary: do not advertise this
	// session as running until Kubernetes confirms at least one ready replica.
	if err := o.k8s.WaitForComponentDeploymentReady(
		ctx, ns, k8s.VtaDeploymentName(session.ID), 2*time.Minute,
	); err != nil {
		return pooled, fmt.Errorf("VTA deployment did not become ready: %w", err)
	}
	return pooled, nil
}

func (o *Orchestrator) finishVtaOnlyDeployment(ctx context.Context, session *model.SetupSession, pooled bool) {
	o.db.Model(&model.SetupSession{}).Where("id = ?", session.ID).Updates(map[string]any{
		"status":       "running",
		"error_msg":    "",
		"failed_stage": "",
		"updated_at":   time.Now(),
	})
	log.Printf("[orchestrator] session %d: VTA running at %s", session.ID, session.FQDN())
	if pooled {
		if current, err := connection.Current(o.db, session.ID); err == nil && current != nil {
			_ = connection.MarkReleased(ctx, o.db, current.ID)
		}
	}
}

// BuildPool runs synchronously under the pool worker's cross-replica lock.
func (o *Orchestrator) BuildPool(ctx context.Context, sessionID uint) error {
	var session model.SetupSession
	if err := o.db.WithContext(ctx).First(&session, sessionID).Error; err != nil {
		return err
	}
	pooled, err := o.isPoolSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if !pooled || session.AdminDid != "" || session.Status == "deleting" {
		return connection.ErrConflict
	}
	if session.Status == model.StatusPoolWaiting {
		return nil
	}
	if session.VtaDid == "" {
		o.runSetup(ctx, sessionID)
	} else if err := o.publishPoolDID(ctx, sessionID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.db.First(&session, sessionID).Error; err != nil {
		return err
	}
	if session.Status != model.StatusPoolPrepared {
		return fmt.Errorf("pool setup did not complete: %s", session.ErrorMsg)
	}
	profiles, err := resourceprofile.Load(ctx, o.db)
	if err != nil {
		return err
	}
	var saved model.WorkloadResource
	if err := o.db.Where("setup_session_id = ? AND component = 'vta'", session.ID).First(&saved).Error; err != nil {
		return err
	}
	profile := profiles[k8s.ComponentVTA]
	profile.MemoryRequest, profile.MemoryLimit = saved.MemoryRequest, saved.MemoryLimit
	profiles[k8s.ComponentVTA] = profile
	ns := o.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
	if err := o.k8s.CreateParkedVtaDeployment(ctx, ns, session.ID, session.VtaImage, profiles.Requirements(k8s.ComponentVTA)); err != nil {
		return err
	}
	if err := o.k8s.CreateVtaService(ctx, ns, session.ID); err != nil {
		return err
	}
	if err := o.k8s.CreateVtaIngress(ctx, ns, session.ID, session.FQDN()); err != nil {
		return err
	}
	return o.db.WithContext(ctx).Model(&session).Updates(map[string]any{
		"status": model.StatusPoolWaiting, "error_msg": "", "failed_stage": "", "updated_at": time.Now(),
	}).Error
}

func (o *Orchestrator) publishPoolDID(ctx context.Context, sessionID uint) error {
	var s model.SetupSession
	if err := o.db.WithContext(ctx).First(&s, sessionID).Error; err != nil {
		return err
	}
	if o.didHosting == nil || s.VtaDid == "" || s.VtaDidLog == "" {
		return fmt.Errorf("pool DID publication is not configured or its log is missing")
	}
	dh, err := o.didHosting.For(s.DidHostingControlURL, s.DIDHostingDid)
	if err != nil {
		return err
	}
	resolver := webvh.NewResolver(5 * time.Second)
	// Resolving the exact self-certifying DID validates a prior successful
	// publication. Never overwrite a different document after a conflict.
	if err := resolver.ResolveDID(ctx, s.VtaDid); err != nil {
		if err := dh.RegisterDid(ctx, VtaDidPath(s.VtaName), s.VtaDidLog); err != nil {
			return err
		}
		if err := resolver.ResolveDID(ctx, s.VtaDid); err != nil {
			return err
		}
	}
	return o.db.WithContext(ctx).Model(&s).Where("status <> 'deleting'").Updates(map[string]any{"status": model.StatusPoolPrepared, "error_msg": "", "failed_stage": ""}).Error
}

func (o *Orchestrator) isPoolSession(ctx context.Context, sessionID uint) (bool, error) {
	if !o.db.Migrator().HasTable(&model.PoolSession{}) {
		return false, nil
	}
	var count int64
	err := o.db.WithContext(ctx).Model(&model.PoolSession{}).Where("session_id = ?", sessionID).Count(&count).Error
	return count == 1, err
}

func (o *Orchestrator) markFailed(sessionID uint, msg string) {
	o.markFailedAt(sessionID, "", msg)
}

// stage is only supplied when crash-safety requires the persisted status to
// advance before the work represented by the previous UI phase is complete.
func (o *Orchestrator) markFailedAt(sessionID uint, stage, msg string) {
	log.Printf("[orchestrator] session %d: failed: %s", sessionID, msg)
	failedStage := gorm.Expr("CASE WHEN status = 'failed' THEN failed_stage ELSE status END")
	if stage != "" {
		failedStage = gorm.Expr("CASE WHEN status = 'failed' THEN failed_stage ELSE ? END", stage)
	}
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"failed_stage": failedStage,
		"status":       "failed",
		"error_msg":    msg,
		"updated_at":   time.Now(),
	})
}
