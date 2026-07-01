package setup

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/ic3software/vtafarm-api/internal/didhosting"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/vault"
)

// This file drives the full_stack state machine (design §5/§6). It mirrors
// orchestrator.go's runSetup/runProvision in spirit — same WaitForJob +
// JobLogs + parse cycle, same markFailed/ctx.Err() handling — but uses the
// generic k8s.Component* helpers (k8s/component_jobs.go,
// k8s/component_resources.go) instead of the VTA-only ones, since it drives
// three components instead of one. orchestrator.go's runSetup/runProvision
// are left completely unchanged.

// fsLabels returns the selector labels shared by a component's Service and
// its Deployment — they must match exactly for the Service to route traffic.
func fsLabels(component string, sessionID uint) map[string]string {
	return map[string]string{"app": "fs-" + component, "session-id": fmt.Sprintf("%d", sessionID)}
}

// fsMediatorVaultEnv is the VAULT_TOKEN + VAULT_SKIP_VERIFY env shared by
// every mediator Job/Deployment (design §9).
func (o *Orchestrator) fsMediatorVaultEnv(sessionID uint) []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name: "VAULT_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: k8s.FSMediatorTokenSecret(sessionID)},
					Key:                  "token",
				},
			},
		},
		{Name: "VAULT_SKIP_VERIFY", Value: "true"},
	}
}

// vaultHostPort strips the scheme from a Vault address for the mediator's
// vault://host:port/... storage URL form.
func vaultHostPort(addr string) string {
	if u, err := url.Parse(addr); err == nil && u.Host != "" {
		return u.Host
	}
	return addr
}

// shellQuote single-quotes a value for safe interpolation into a `sh -c`
// command string. Applied to every parsed/external value (digests, DIDs)
// threaded into full_stack Job commands as defense in depth.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fsSetStatus updates only the status column — used between steps so
// GET /setup/:id reflects the design's §5 step vocabulary 1:1.
func (o *Orchestrator) fsSetStatus(sessionID uint, status string) {
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"status": status, "updated_at": time.Now(),
	})
}

// fsJobFailErr builds an error for a failed Job, appending its logs when
// available — mirrors the inline pattern in runSetup/runProvision.
func (o *Orchestrator) fsJobFailErr(ctx context.Context, ns, jobName, failMsg string) error {
	if jobLogs, logsErr := o.k8s.JobLogs(ctx, ns, jobName); logsErr == nil && jobLogs != "" {
		failMsg = failMsg + "\n\n--- Job Logs ---\n" + jobLogs
	}
	return fmt.Errorf("%s", failMsg)
}

// ── Phase 1: env_provision … deploy_mediator ────────────────────────────────

func (o *Orchestrator) runFullStack(ctx context.Context, sessionID uint) {
	var session model.SetupSession
	if err := o.db.First(&session, sessionID).Error; err != nil {
		log.Printf("[orchestrator] fs session %d: load failed: %v", sessionID, err)
		return
	}
	s := &session
	userIDStr := fmt.Sprintf("%d", s.UserID)
	ns := o.k8s.UserNamespace(userIDStr)

	if o.vault == nil {
		o.markFailed(sessionID, "vault not configured: set VAULT_ADDR/VAULT_ROLE_ID/VAULT_SECRET_ID")
		return
	}

	fail := func(prefix string, err error) bool {
		if err == nil {
			return false
		}
		if ctx.Err() != nil {
			return true
		}
		o.markFailed(sessionID, prefix+": "+err.Error())
		return true
	}

	// env_provision
	o.fsSetStatus(sessionID, "env_provision")
	if fail("failed to ensure k8s namespace", o.k8s.EnsureUserEnvironment(ctx, userIDStr)) {
		return
	}
	if fail("failed to provision vault access", o.vault.EnsureUserAccess(ctx, s.UserID, ns, k8s.VtaServiceAccount)) {
		return
	}
	mediatorToken, err := o.vault.MintMediatorToken(ctx, s.UserID, sessionID)
	if fail("failed to mint mediator vault token", err) {
		return
	}
	if fail("failed to store mediator vault token", o.k8s.CreateComponentSecret(ctx, ns, k8s.FSMediatorTokenSecret(sessionID), "token", mediatorToken)) {
		return
	}

	// k8s_provision
	o.fsSetStatus(sessionID, "k8s_provision")
	if fail("failed to provision k8s resources", o.fsK8sProvision(ctx, ns, s)) {
		return
	}

	// step_vta_setup
	o.fsSetStatus(sessionID, "step_vta_setup")
	vtaDid, mediatorDid, vtaDidLog, mediatorDidLog, err := o.fsStepVtaSetup(ctx, ns, s)
	if fail("vta setup failed", err) {
		return
	}
	s.VtaDid, s.MediatorDid = vtaDid, mediatorDid
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"vta_did": vtaDid, "mediator_did": mediatorDid, "updated_at": time.Now(),
	})
	log.Printf("[orchestrator] fs session %d: vta_did=%s mediator_did=%s", sessionID, vtaDid, mediatorDid)

	// step_mediator_p1
	o.fsSetStatus(sessionID, "step_mediator_p1")
	if fail("mediator setup (phase 1) failed", o.fsStepMediatorP1(ctx, ns, s)) {
		return
	}

	// step_mediator_reprov
	o.fsSetStatus(sessionID, "step_mediator_reprov")
	digest2a, mediatorAdminDid, err := o.fsStepMediatorReprov(ctx, ns, s)
	if fail("mediator reprovision failed", err) {
		return
	}
	s.MediatorAdminDid = mediatorAdminDid
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Update("mediator_admin_did", mediatorAdminDid)

	// step_mediator_p2
	o.fsSetStatus(sessionID, "step_mediator_p2")
	mediatorAdminKey, err := o.fsStepMediatorP2(ctx, ns, s, digest2a)
	if fail("mediator setup (phase 2) failed", err) {
		return
	}
	s.MediatorAdminKey = mediatorAdminKey
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Update("mediator_admin_key", mediatorAdminKey)

	// step_dids_p1
	o.fsSetStatus(sessionID, "step_dids_p1")
	if fail("dids setup (offline-prepare) failed", o.fsStepDidsP1(ctx, ns, s)) {
		return
	}

	// step_dids_provision
	o.fsSetStatus(sessionID, "step_dids_provision")
	digest3a, err := o.fsStepDidsProvision(ctx, ns, s)
	if fail("dids provision-integration failed", err) {
		return
	}

	// step_dids_p2
	o.fsSetStatus(sessionID, "step_dids_p2")
	webvhAdminDid, webvhAdminKey, daemonDid, err := o.fsStepDidsP2(ctx, ns, s, digest3a)
	if fail("dids setup (offline-complete) failed", err) {
		return
	}
	s.WebvhAdminDid, s.WebvhAdminKey, s.DidsDaemonDid = webvhAdminDid, webvhAdminKey, daemonDid
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"webvh_admin_did": webvhAdminDid, "webvh_admin_key": webvhAdminKey,
		"dids_daemon_did": daemonDid, "updated_at": time.Now(),
	})

	// step_dids_invite — must run before deploy_dids, while no daemon pod holds the PVC.
	o.fsSetStatus(sessionID, "step_dids_invite")
	enrollURL, err := o.fsStepDidsInvite(ctx, ns, s)
	if fail("dids invite failed", err) {
		return
	}
	s.DidsEnrollURL = enrollURL
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Update("dids_enroll_url", enrollURL)

	// deploy_dids
	o.fsSetStatus(sessionID, "deploy_dids")
	if fail("failed to deploy dids daemon", o.fsDeployDids(ctx, ns, s)) {
		return
	}

	// step_upload_didlogs — best-effort; manual upload via the dids admin UI
	// is the documented fallback (design §6/§14), so failures here don't
	// fail the whole machine.
	o.fsSetStatus(sessionID, "step_upload_didlogs")
	o.fsUploadDidLogs(ctx, ns, s, vtaDidLog, mediatorDidLog)

	// deploy_mediator
	o.fsSetStatus(sessionID, "deploy_mediator")
	if fail("failed to deploy mediator", o.fsDeployMediator(ctx, ns, s)) {
		return
	}

	// Gate: PNM admin DID. Mirrors runSetup's auto-trigger of Provision()
	// when admin_did was supplied upfront.
	if s.AdminDid != "" {
		log.Printf("[orchestrator] fs session %d: admin_did present, auto-finishing", sessionID)
		o.Provision(sessionID, s.AdminDid)
		return
	}
	o.fsSetStatus(sessionID, "awaiting_admin_did")
	log.Printf("[orchestrator] fs session %d: awaiting admin DID", sessionID)
}

// fsK8sProvision creates the three PVCs + Services + Ingresses up front
// (design §2: "created up front; 503 until pods come up").
func (o *Orchestrator) fsK8sProvision(ctx context.Context, ns string, s *model.SetupSession) error {
	for _, name := range []string{k8s.FSVtaName(s.ID), k8s.FSMediatorName(s.ID), k8s.FSDidsName(s.ID)} {
		if err := o.k8s.CreateComponentPVC(ctx, ns, name); err != nil {
			return err
		}
	}

	type svc struct {
		name, fqdn string
		port       int32
		labels     map[string]string
	}
	svcs := []svc{
		{k8s.FSVtaName(s.ID), s.VtaFQDN(), 8100, fsLabels("vta", s.ID)},
		{k8s.FSMediatorName(s.ID), s.MediatorFQDN(), 7037, fsLabels("mediator", s.ID)},
		{k8s.FSDidsName(s.ID), s.DidsFQDN(), 8534, fsLabels("dids", s.ID)},
	}
	for _, sv := range svcs {
		if err := o.k8s.CreateComponentService(ctx, ns, sv.name, sv.labels, sv.port); err != nil {
			return err
		}
		if err := o.k8s.CreateComponentIngress(ctx, ns, sv.name, sv.name, sv.port, sv.fqdn); err != nil {
			return err
		}
	}
	return nil
}

// fsStepVtaSetup runs `vta setup` with [messaging] kind=create_mediator,
// capturing the VTA DID (1a), mediator DID (1b), and both DID-log artifacts
// for the later step_upload_didlogs call.
func (o *Orchestrator) fsStepVtaSetup(ctx context.Context, ns string, s *model.SetupSession) (vtaDid, mediatorDid, vtaDidLog, mediatorDidLog string, err error) {
	toml, err := RenderFullStackVtaSetupTOML(s, VaultSecrets{
		Addr:       o.vaultAddr,
		SecretPath: vault.SeedPath(s.UserID, s.ID),
		KVMount:    o.vault.KVMount(),
		K8sRole:    vault.UserName(s.UserID),
		SkipVerify: true,
	})
	if err != nil {
		return "", "", "", "", fmt.Errorf("render vta-setup.toml: %w", err)
	}

	jobName := k8s.FSJobVtaSetup(s.ID)
	cmd := "vta setup --from /config/vta-setup.toml" +
		" && echo '---ARTIFACT:VTA-did.jsonl---' && cat data/vta/did-logs/VTA-did.jsonl" +
		" && echo '---ARTIFACT:mediator-did.jsonl---' && cat data/vta/did-logs/mediator-did.jsonl"

	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.VtaImage,
		Command:        []string{"sh", "-c", cmd},
		WorkingDir:     "/work/vta",
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "vta-data", ClaimName: k8s.FSVtaName(s.ID), MountPath: "/work/vta"}},
		ConfigMapName:  jobName,
		ConfigMapKey:   "vta-setup.toml",
		ConfigMapData:  toml,
	}); err != nil {
		return "", "", "", "", fmt.Errorf("create job: %w", err)
	}

	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		return "", "", "", "", err
	}
	if !succeeded {
		return "", "", "", "", o.fsJobFailErr(ctx, ns, jobName, failMsg)
	}
	logs, err := o.k8s.JobLogs(ctx, ns, jobName)
	if err != nil {
		return "", "", "", "", fmt.Errorf("read job logs: %w", err)
	}

	vtaDid, err = ParseVtaDID(logs)
	if err != nil {
		return "", "", "", "", err
	}
	mediatorDid, err = ParseMediatorDID(logs)
	if err != nil {
		return "", "", "", "", err
	}
	vtaDidLog = ParseArtifact(logs, "VTA-did.jsonl")
	mediatorDidLog = ParseArtifact(logs, "mediator-did.jsonl")
	return vtaDid, mediatorDid, vtaDidLog, mediatorDidLog, nil
}

// fsStepMediatorP1 runs `mediator-setup` phase 1, writing
// bootstrap-request.json to the mediator PVC and the ephemeral HPKE seed to
// Vault (via the injected VAULT_TOKEN).
func (o *Orchestrator) fsStepMediatorP1(ctx context.Context, ns string, s *model.SetupSession) error {
	recipe, err := RenderMediatorRecipeTOML(s, MediatorVaultSecrets{
		HostPort: vaultHostPort(o.vaultAddr),
		KVMount:  o.vault.KVMount(),
		Prefix:   vault.MediatorPrefix(s.UserID, s.ID),
	})
	if err != nil {
		return fmt.Errorf("render mediator-recipe.toml: %w", err)
	}

	jobName := k8s.FSJobMediatorP1(s.ID)
	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.MediatorImage,
		Command:        []string{"sh", "-c", "mediator-setup --from /config/mediator-recipe.toml"},
		WorkingDir:     "/work/mediator",
		ServiceAccount: k8s.PodOperatorServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "mediator-data", ClaimName: k8s.FSMediatorName(s.ID), MountPath: "/work/mediator"}},
		ConfigMapName:  jobName,
		ConfigMapKey:   "mediator-recipe.toml",
		ConfigMapData:  recipe,
		Env:            o.fsMediatorVaultEnv(s.ID),
	}); err != nil {
		return fmt.Errorf("create job: %w", err)
	}

	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		return err
	}
	if !succeeded {
		return o.fsJobFailErr(ctx, ns, jobName, failMsg)
	}
	return nil
}

// fsStepMediatorReprov runs `vta contexts reprovision`, mounting both the
// VTA and mediator PVCs (design §4 cross-component handoff).
func (o *Orchestrator) fsStepMediatorReprov(ctx context.Context, ns string, s *model.SetupSession) (digest, adminDid string, err error) {
	jobName := k8s.FSJobMediatorReprov(s.ID)
	cmd := "vta contexts reprovision --id mediator --recipient /work/mediator/bootstrap-request.json --out /work/mediator/bundle.armor"

	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.VtaImage,
		Command:        []string{"sh", "-c", cmd},
		WorkingDir:     "/work/vta",
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts: []k8s.PVCMount{
			{Name: "vta-data", ClaimName: k8s.FSVtaName(s.ID), MountPath: "/work/vta"},
			{Name: "mediator-data", ClaimName: k8s.FSMediatorName(s.ID), MountPath: "/work/mediator"},
		},
	}); err != nil {
		return "", "", fmt.Errorf("create job: %w", err)
	}

	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		return "", "", err
	}
	if !succeeded {
		return "", "", o.fsJobFailErr(ctx, ns, jobName, failMsg)
	}
	logs, err := o.k8s.JobLogs(ctx, ns, jobName)
	if err != nil {
		return "", "", fmt.Errorf("read job logs: %w", err)
	}
	digest, err = ParseDigest(logs)
	if err != nil {
		return "", "", err
	}
	adminDid, err = ParseMediatorAdminDID(logs)
	if err != nil {
		return "", "", err
	}
	return digest, adminDid, nil
}

// fsStepMediatorP2 runs `mediator-setup --bundle --digest`, provisioning the
// unified secret backend into Vault and writing conf/mediator.toml +
// conf/atm-functions.lua to the mediator PVC.
func (o *Orchestrator) fsStepMediatorP2(ctx context.Context, ns string, s *model.SetupSession, digest2a string) (adminKey string, err error) {
	recipe, err := RenderMediatorRecipeTOML(s, MediatorVaultSecrets{
		HostPort: vaultHostPort(o.vaultAddr),
		KVMount:  o.vault.KVMount(),
		Prefix:   vault.MediatorPrefix(s.UserID, s.ID),
	})
	if err != nil {
		return "", fmt.Errorf("render mediator-recipe.toml: %w", err)
	}

	jobName := k8s.FSJobMediatorP2(s.ID)
	cmd := fmt.Sprintf("mediator-setup --from /config/mediator-recipe.toml --bundle bundle.armor --digest %s", shellQuote(digest2a))

	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.MediatorImage,
		Command:        []string{"sh", "-c", cmd},
		WorkingDir:     "/work/mediator",
		ServiceAccount: k8s.PodOperatorServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "mediator-data", ClaimName: k8s.FSMediatorName(s.ID), MountPath: "/work/mediator"}},
		ConfigMapName:  jobName,
		ConfigMapKey:   "mediator-recipe.toml",
		ConfigMapData:  recipe,
		Env:            o.fsMediatorVaultEnv(s.ID),
	}); err != nil {
		return "", fmt.Errorf("create job: %w", err)
	}

	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		return "", err
	}
	if !succeeded {
		return "", o.fsJobFailErr(ctx, ns, jobName, failMsg)
	}
	logs, err := o.k8s.JobLogs(ctx, ns, jobName)
	if err != nil {
		return "", fmt.Errorf("read job logs: %w", err)
	}
	adminKey, err = ParseMediatorAdminKey(logs)
	if err != nil {
		return "", err
	}
	return adminKey, nil
}

// fsStepDidsP1 runs `did-hosting-daemon setup` (offline-prepare), writing
// bootstrap-request.json to the dids PVC.
func (o *Orchestrator) fsStepDidsP1(ctx context.Context, ns string, s *model.SetupSession) error {
	recipe, err := RenderWebvhRecipeTOML(s, WebvhPhasePrepare, "")
	if err != nil {
		return fmt.Errorf("render webvh-recipe.toml: %w", err)
	}

	jobName := k8s.FSJobDidsP1(s.ID)
	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.DidsImage,
		Command:        []string{"sh", "-c", "did-hosting-daemon setup --from /config/webvh-recipe.toml"},
		WorkingDir:     "/work/dids",
		ServiceAccount: k8s.PodOperatorServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "dids-data", ClaimName: k8s.FSDidsName(s.ID), MountPath: "/work/dids"}},
		ConfigMapName:  jobName,
		ConfigMapKey:   "webvh-recipe.toml",
		ConfigMapData:  recipe,
	}); err != nil {
		return fmt.Errorf("create job: %w", err)
	}

	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		return err
	}
	if !succeeded {
		return o.fsJobFailErr(ctx, ns, jobName, failMsg)
	}
	return nil
}

// fsStepDidsProvision runs `vta bootstrap provision-integration`, mounting
// both the VTA and dids PVCs (design §4 cross-component handoff).
func (o *Orchestrator) fsStepDidsProvision(ctx context.Context, ns string, s *model.SetupSession) (digest string, err error) {
	jobName := k8s.FSJobDidsProvision(s.ID)
	cmd := "vta bootstrap provision-integration --request /work/dids/bootstrap-request.json --out /work/dids/bundle.armor --create-context"

	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.VtaImage,
		Command:        []string{"sh", "-c", cmd},
		WorkingDir:     "/work/vta",
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts: []k8s.PVCMount{
			{Name: "vta-data", ClaimName: k8s.FSVtaName(s.ID), MountPath: "/work/vta"},
			{Name: "dids-data", ClaimName: k8s.FSDidsName(s.ID), MountPath: "/work/dids"},
		},
	}); err != nil {
		return "", fmt.Errorf("create job: %w", err)
	}

	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		return "", err
	}
	if !succeeded {
		return "", o.fsJobFailErr(ctx, ns, jobName, failMsg)
	}
	logs, err := o.k8s.JobLogs(ctx, ns, jobName)
	if err != nil {
		return "", fmt.Errorf("read job logs: %w", err)
	}
	digest, err = ParseDigest(logs)
	if err != nil {
		return "", err
	}
	return digest, nil
}

// fsStepDidsP2 runs `did-hosting-daemon setup` (offline-complete), capturing
// the webvh admin DID (3b), admin private key (3c), and daemon DID (3d).
func (o *Orchestrator) fsStepDidsP2(ctx context.Context, ns string, s *model.SetupSession, digest3a string) (adminDid, adminKey, daemonDid string, err error) {
	recipe, err := RenderWebvhRecipeTOML(s, WebvhPhaseComplete, digest3a)
	if err != nil {
		return "", "", "", fmt.Errorf("render webvh-recipe.toml: %w", err)
	}

	jobName := k8s.FSJobDidsP2(s.ID)
	cmd := "did-hosting-daemon setup --from /config/webvh-recipe.toml" +
		" && echo '---ARTIFACT:server_did---' && grep '^server_did' config.toml"

	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.DidsImage,
		Command:        []string{"sh", "-c", cmd},
		WorkingDir:     "/work/dids",
		ServiceAccount: k8s.PodOperatorServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "dids-data", ClaimName: k8s.FSDidsName(s.ID), MountPath: "/work/dids"}},
		ConfigMapName:  jobName,
		ConfigMapKey:   "webvh-recipe.toml",
		ConfigMapData:  recipe,
	}); err != nil {
		return "", "", "", fmt.Errorf("create job: %w", err)
	}

	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		return "", "", "", err
	}
	if !succeeded {
		return "", "", "", o.fsJobFailErr(ctx, ns, jobName, failMsg)
	}
	logs, err := o.k8s.JobLogs(ctx, ns, jobName)
	if err != nil {
		return "", "", "", fmt.Errorf("read job logs: %w", err)
	}

	adminDid, err = ParseWebvhAdminDID(logs)
	if err != nil {
		return "", "", "", err
	}
	adminKey, err = ParseWebvhAdminKey(logs)
	if err != nil {
		return "", "", "", err
	}
	daemonDid, err = ParseServerDid(ParseArtifact(logs, "server_did"))
	if err != nil {
		return "", "", "", err
	}
	return adminDid, adminKey, daemonDid, nil
}

// fsStepDidsInvite mints the dids admin-panel enrollment URL (3e). Must run
// before fsDeployDids — it opens the local store directly, so no daemon pod
// can be holding the PVC yet.
func (o *Orchestrator) fsStepDidsInvite(ctx context.Context, ns string, s *model.SetupSession) (enrollURL string, err error) {
	jobName := k8s.FSJobDidsInvite(s.ID)
	cmd := fmt.Sprintf("did-hosting-daemon invite --role admin --did %s", shellQuote(s.WebvhAdminDid))

	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.DidsImage,
		Command:        []string{"sh", "-c", cmd},
		WorkingDir:     "/work/dids",
		ServiceAccount: k8s.PodOperatorServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "dids-data", ClaimName: k8s.FSDidsName(s.ID), MountPath: "/work/dids"}},
	}); err != nil {
		return "", fmt.Errorf("create job: %w", err)
	}

	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		return "", err
	}
	if !succeeded {
		return "", o.fsJobFailErr(ctx, ns, jobName, failMsg)
	}
	logs, err := o.k8s.JobLogs(ctx, ns, jobName)
	if err != nil {
		return "", fmt.Errorf("read job logs: %w", err)
	}
	enrollURL, err = ParseDidsEnrollURL(logs)
	if err != nil {
		return "", err
	}
	return enrollURL, nil
}

// fsDeployDids starts the dids daemon Deployment.
func (o *Orchestrator) fsDeployDids(ctx context.Context, ns string, s *model.SetupSession) error {
	name := k8s.FSDidsName(s.ID)
	return o.k8s.CreateComponentDeployment(ctx, ns, k8s.ComponentDeploymentSpec{
		Name:           name,
		Image:          s.DidsImage,
		Command:        []string{"did-hosting-daemon"},
		WorkingDir:     "/work/dids",
		ServiceAccount: k8s.PodOperatorServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "dids-data", ClaimName: name, MountPath: "/work/dids"}},
		Port:           8534,
		Labels:         fsLabels("dids", s.ID),
	})
}

// fsUploadDidLogs registers the mediator + VTA DID logs on the now-running
// dids daemon via its in-cluster control API. Best-effort: per design §6/§14
// the documented fallback is for the user to upload manually via the dids
// admin UI, so failures here are logged, not fatal.
func (o *Orchestrator) fsUploadDidLogs(ctx context.Context, ns string, s *model.SetupSession, vtaDidLog, mediatorDidLog string) {
	if s.WebvhAdminDid == "" || s.WebvhAdminKey == "" {
		log.Printf("[orchestrator] fs session %d: skipping DID log upload — missing dids admin credentials", s.ID)
		return
	}
	controlURL := fmt.Sprintf("http://%s.%s.svc:8534", k8s.FSDidsName(s.ID), ns)
	client, err := didhosting.NewFromMultibaseKey(controlURL, s.WebvhAdminDid, s.WebvhAdminKey)
	if err != nil {
		log.Printf("[orchestrator] fs session %d: DID log upload skipped — client init failed (manual fallback via dids admin UI): %v", s.ID, err)
		return
	}
	if mediatorDidLog != "" {
		if err := client.RegisterDid(ctx, "mediator", mediatorDidLog); err != nil {
			log.Printf("[orchestrator] fs session %d: mediator DID log upload FAILED (manual fallback via dids admin UI): %v", s.ID, err)
		}
	}
	if vtaDidLog != "" {
		if err := client.RegisterDid(ctx, "vta", vtaDidLog); err != nil {
			log.Printf("[orchestrator] fs session %d: VTA DID log upload FAILED (manual fallback via dids admin UI): %v", s.ID, err)
		}
	}
}

// fsDeployMediator starts the mediator Deployment.
func (o *Orchestrator) fsDeployMediator(ctx context.Context, ns string, s *model.SetupSession) error {
	name := k8s.FSMediatorName(s.ID)
	return o.k8s.CreateComponentDeployment(ctx, ns, k8s.ComponentDeploymentSpec{
		Name:           name,
		Image:          s.MediatorImage,
		Command:        []string{"mediator"},
		WorkingDir:     "/work/mediator",
		ServiceAccount: k8s.PodOperatorServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "mediator-data", ClaimName: name, MountPath: "/work/mediator"}},
		Env:            o.fsMediatorVaultEnv(s.ID),
		Port:           7037,
		Labels:         fsLabels("mediator", s.ID),
	})
}

// ── Phase 2: step_import_admin_did → deploy_vta → completed ────────────────

// runFullStackFinish mirrors runProvision: import the user's PNM admin DID
// then start the VTA Deployment. Called both from runFullStack's auto-
// trigger path (admin_did supplied at POST /setup) and from Provision()'s
// full_stack dispatch (POST /setup/:id/admin).
func (o *Orchestrator) runFullStackFinish(ctx context.Context, sessionID uint, adminDid string) {
	var session model.SetupSession
	if err := o.db.First(&session, sessionID).Error; err != nil {
		log.Printf("[orchestrator] fs finish %d: load failed: %v", sessionID, err)
		return
	}
	s := &session
	ns := o.k8s.UserNamespace(fmt.Sprintf("%d", s.UserID))

	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"status": "step_import_admin_did", "admin_did": adminDid, "updated_at": time.Now(),
	})
	log.Printf("[orchestrator] fs session %d: importing admin DID %s", sessionID, adminDid)

	jobName := k8s.FSJobImportAdminDid(sessionID)
	cmd := fmt.Sprintf("vta import-did --role admin --label pnm-bootstrap --did %s", shellQuote(adminDid))
	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.VtaImage,
		Command:        []string{"sh", "-c", cmd},
		WorkingDir:     "/work/vta",
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "vta-data", ClaimName: k8s.FSVtaName(sessionID), MountPath: "/work/vta"}},
	}); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "failed to create import-admin-did job: "+err.Error())
		return
	}

	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "import-admin-did job watch error: "+err.Error())
		return
	}
	if !succeeded {
		o.markFailed(sessionID, "import-admin-did job failed: "+o.fsJobFailErr(ctx, ns, jobName, failMsg).Error())
		return
	}

	o.fsSetStatus(sessionID, "deploy_vta")

	name := k8s.FSVtaName(sessionID)
	if err := o.k8s.CreateComponentDeployment(ctx, ns, k8s.ComponentDeploymentSpec{
		Name:           name,
		Image:          s.VtaImage,
		Command:        []string{"vta"},
		WorkingDir:     "/work/vta",
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "vta-data", ClaimName: name, MountPath: "/work/vta"}},
		Env: []corev1.EnvVar{
			{Name: "NO_COLOR", Value: "1"},
			{Name: "CLICOLOR", Value: "0"},
		},
		Port:   8100,
		Labels: fsLabels("vta", sessionID),
	}); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "failed to create vta deployment: "+err.Error())
		return
	}

	o.fsSetStatus(sessionID, "completed")
	log.Printf("[orchestrator] fs session %d: completed", sessionID)
}

// ── Resume ───────────────────────────────────────────────────────────────────

// resumeFullStack re-attaches goroutines for full_stack sessions interrupted
// mid-run at startup. Every step is idempotent (AlreadyExists ignored,
// WaitForJob re-attaches by name), so resuming just restarts the relevant
// phase from its top — see plan decision #3.
func (o *Orchestrator) resumeFullStack() {
	preGate := []string{
		"dns_provision", "env_provision", "k8s_provision", "step_vta_setup",
		"step_mediator_p1", "step_mediator_reprov", "step_mediator_p2",
		"step_dids_p1", "step_dids_provision", "step_dids_p2", "step_dids_invite",
		"deploy_dids", "step_upload_didlogs", "deploy_mediator",
	}
	var inFlight []model.SetupSession
	if err := o.db.Where("mode = ? AND status IN ?", model.ModeFullStack, preGate).Find(&inFlight).Error; err != nil {
		log.Printf("[orchestrator] resume full_stack: query failed: %v", err)
	}
	for _, s := range inFlight {
		log.Printf("[orchestrator] resuming full_stack session %d (status=%s)", s.ID, s.Status)
		o.Start(s.ID)
	}

	postGate := []string{"step_import_admin_did", "deploy_vta"}
	var finishing []model.SetupSession
	if err := o.db.Where("mode = ? AND status IN ?", model.ModeFullStack, postGate).Find(&finishing).Error; err != nil {
		log.Printf("[orchestrator] resume full_stack: query failed: %v", err)
	}
	for _, s := range finishing {
		log.Printf("[orchestrator] resuming full_stack finish %d (status=%s)", s.ID, s.Status)
		o.Provision(s.ID, s.AdminDid)
	}

	var gatedWithAdmin []model.SetupSession
	if err := o.db.Where("mode = ? AND status = ? AND admin_did != ''", model.ModeFullStack, "awaiting_admin_did").Find(&gatedWithAdmin).Error; err != nil {
		log.Printf("[orchestrator] resume full_stack: query failed: %v", err)
	}
	for _, s := range gatedWithAdmin {
		log.Printf("[orchestrator] resuming full_stack gated session %d", s.ID)
		o.Provision(s.ID, s.AdminDid)
	}
}

// ── Teardown ─────────────────────────────────────────────────────────────────

// TeardownMediatorVault revokes the mediator's minted VAULT_TOKEN (read back
// from its K8s Secret before the caller deletes it) and deletes the
// mediator's KV secrets. Best-effort, mirrors TeardownVaultSeed. Call before
// deleting the fs-{sid}-mediator-vault-token Secret.
func (o *Orchestrator) TeardownMediatorVault(ctx context.Context, ns string, userID, sessionID uint) {
	if o.vault == nil {
		return
	}
	if token, err := o.k8s.GetComponentSecretValue(ctx, ns, k8s.FSMediatorTokenSecret(sessionID), "token"); err == nil && token != "" {
		if err := o.vault.RevokeToken(ctx, token); err != nil {
			log.Printf("[orchestrator] warn: revoke mediator token (user %d session %d): %v", userID, sessionID, err)
		}
	}
	if err := o.vault.DeleteMediatorSecrets(ctx, userID, sessionID); err != nil {
		log.Printf("[orchestrator] warn: delete mediator secrets (user %d session %d): %v", userID, sessionID, err)
	}
}
