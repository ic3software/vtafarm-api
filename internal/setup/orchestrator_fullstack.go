package setup

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

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

// fsNoColorEnv disables ANSI color codes so every full_stack Job/Deployment's
// streamed and captured logs stay plain text — some of the binaries
// colorize stdout even when piped (not a real TTY), which can otherwise
// corrupt regex-captured values (see stripANSI, the parsing-side safety
// net). Set on every full_stack Job and Deployment, not just the ones whose
// output gets parsed.
func fsNoColorEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "NO_COLOR", Value: "1"},
		{Name: "CLICOLOR", Value: "0"},
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
	if jobLogs, logsErr := o.fsJobLogs(ctx, ns, jobName); logsErr == nil && jobLogs != "" {
		failMsg = failMsg + "\n\n--- Job Logs ---\n" + jobLogs
	}
	return fmt.Errorf("%s", failMsg)
}

// fsJobLogs reads a Job's logs and strips ANSI escape sequences before
// they're regex-parsed — some full_stack binaries colorize stdout even when
// piped (not a real TTY), which can otherwise corrupt \S+-captured values.
func (o *Orchestrator) fsJobLogs(ctx context.Context, ns, jobName string) (string, error) {
	logs, err := o.k8s.JobLogs(ctx, ns, jobName)
	if err != nil {
		return "", err
	}
	return stripANSI(logs), nil
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

	// k8s_provision
	o.fsSetStatus(sessionID, "k8s_provision")
	if fail("failed to provision k8s resources", o.fsK8sProvision(ctx, ns, s)) {
		return
	}

	// step_vta_setup
	o.fsSetStatus(sessionID, "step_vta_setup")
	vtaDid, mediatorDid, err := o.fsStepVtaSetup(ctx, ns, s)
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
	didHostingAdminDid, didHostingAdminKey, didHostingDid, err := o.fsStepDidsP2(ctx, ns, s, digest3a)
	if fail("dids setup (offline-complete) failed", err) {
		return
	}
	s.DIDHostingAdminDid, s.WebvhAdminKey, s.DIDHostingDid = didHostingAdminDid, didHostingAdminKey, didHostingDid
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"did_hosting_admin_did": didHostingAdminDid, "webvh_admin_key": didHostingAdminKey,
		"did_hosting_did": didHostingDid, "updated_at": time.Now(),
	})

	// step_dids_invite — must run before deploy_dids, while no daemon pod holds the PVC.
	o.fsSetStatus(sessionID, "step_dids_invite")
	enrollURL, err := o.fsStepDidsInvite(ctx, ns, s)
	if fail("dids invite failed", err) {
		return
	}
	s.DidsEnrollURL = enrollURL
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Update("dids_enroll_url", enrollURL)

	// step_dids_load_did — loads the VTA + mediator DID logs into the dids
	// daemon's local store directly (did-hosting-daemon load-did). Must also
	// run before deploy_dids, same reason as step_dids_invite: it opens the
	// local store directly, so no daemon pod can be holding the PVC yet.
	// Loading offline like this (instead of registering over the daemon's
	// control API after it's up) is what lets both the dids daemon and the
	// mediator resolve these DIDs successfully on their very first boot.
	o.fsSetStatus(sessionID, "step_dids_load_did")
	if fail("dids load-did failed", o.fsStepDidsLoadDid(ctx, ns, s)) {
		return
	}

	// No separate ACL grant for the VTA needed here: did-hosting-daemon's
	// offline-complete finalizer (step_dids_p2) already seeds an idempotent
	// Admin-role ACL entry for its provisioning VTA (upstream commit "a
	// VTA-provisioned daemon trusts its provisioning VTA to publish") —
	// a dedicated step_dids_grant_vta trying to add the same DID again just
	// 409s against that entry. Admin is a superset of the "service" role this
	// used to request, so nothing is lost by removing it.

	// deploy_dids
	o.fsSetStatus(sessionID, "deploy_dids")
	if fail("failed to deploy dids daemon", o.fsDeployDids(ctx, ns, s)) {
		return
	}

	// deploy_mediator
	o.fsSetStatus(sessionID, "deploy_mediator")
	if fail("failed to deploy mediator", o.fsDeployMediator(ctx, ns, s)) {
		return
	}

	// step_vta_register_dids — register the session's own dids daemon in the
	// VTA's server registry, the full_stack counterpart of vta_only's
	// `did-mgmt servers add --id control`. Runs after deploy_dids because
	// `servers add` live-resolves the daemon's DID (only served once the
	// daemon is up), and before deploy_vta because it writes the VTA's
	// fjall store offline.
	o.fsSetStatus(sessionID, "step_vta_register_dids")
	if fail("vta register-dids failed", o.fsStepVtaRegisterDids(ctx, ns, s)) {
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
		{k8s.FSVtaName(s.ID), s.FQDN(), 8100, fsLabels("vta", s.ID)},
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
// capturing the VTA DID (1a) and mediator DID (1b). Their DID-log files stay
// on the vta PVC for fsStepDidsLoadDid to read directly later.
func (o *Orchestrator) fsStepVtaSetup(ctx context.Context, ns string, s *model.SetupSession) (vtaDid, mediatorDid string, err error) {
	toml, err := RenderFullStackVtaSetupTOML(s, VaultSecrets{
		Addr:       o.vaultAddr,
		SecretPath: vault.SeedPath(s.UserID, s.ID),
		KVMount:    o.vault.KVMount(),
		K8sRole:    vault.UserName(s.UserID),
		SkipVerify: true,
	})
	if err != nil {
		return "", "", fmt.Errorf("render vta-setup.toml: %w", err)
	}

	// data/vta/did-logs/{VTA,mediator}-did.jsonl are left on the vta PVC —
	// fsStepDidsLoadDid reads them directly from there, mounted read-only
	// alongside the dids PVC, instead of round-tripping them through the
	// orchestrator.
	jobName := k8s.FSJobVtaSetup(s.ID)
	cmd := "vta setup --from /config/vta-setup.toml"

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
		Env:            fsNoColorEnv(),
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
	logs, err := o.fsJobLogs(ctx, ns, jobName)
	if err != nil {
		return "", "", fmt.Errorf("read job logs: %w", err)
	}

	vtaDid, err = ParseVtaDID(logs)
	if err != nil {
		return "", "", err
	}
	mediatorDid, err = ParseMediatorDID(logs)
	if err != nil {
		return "", "", err
	}
	return vtaDid, mediatorDid, nil
}

// fsStepMediatorP1 runs `mediator-setup` phase 1, writing
// bootstrap-request.json to the mediator PVC and the ephemeral HPKE seed to
// Vault (kubernetes auth — the mediator's own pod ServiceAccount JWT).
func (o *Orchestrator) fsStepMediatorP1(ctx context.Context, ns string, s *model.SetupSession) error {
	recipe, err := RenderMediatorRecipeTOML(s, MediatorVaultSecrets{
		HostPort:   vaultHostPort(o.vaultAddr),
		KVMount:    o.vault.KVMount(),
		Prefix:     vault.MediatorPrefix(s.UserID, s.ID),
		K8sRole:    vault.UserName(s.UserID),
		SkipVerify: true,
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
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "mediator-data", ClaimName: k8s.FSMediatorName(s.ID), MountPath: "/work/mediator"}},
		ConfigMapName:  jobName,
		ConfigMapKey:   "mediator-recipe.toml",
		ConfigMapData:  recipe,
		Env:            fsNoColorEnv(),
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
		Env: fsNoColorEnv(),
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
	logs, err := o.fsJobLogs(ctx, ns, jobName)
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
		HostPort:   vaultHostPort(o.vaultAddr),
		KVMount:    o.vault.KVMount(),
		Prefix:     vault.MediatorPrefix(s.UserID, s.ID),
		K8sRole:    vault.UserName(s.UserID),
		SkipVerify: true,
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
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "mediator-data", ClaimName: k8s.FSMediatorName(s.ID), MountPath: "/work/mediator"}},
		ConfigMapName:  jobName,
		ConfigMapKey:   "mediator-recipe.toml",
		ConfigMapData:  recipe,
		Env:            fsNoColorEnv(),
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
	logs, err := o.fsJobLogs(ctx, ns, jobName)
	if err != nil {
		return "", fmt.Errorf("read job logs: %w", err)
	}
	adminKey, err = ParseMediatorAdminKey(logs)
	if err != nil {
		return "", err
	}
	return adminKey, nil
}

// fsWebvhVaultSecrets builds the dids daemon's Vault [secrets] params —
// kubernetes auth via the same per-user role the VTA and mediator use.
func (o *Orchestrator) fsWebvhVaultSecrets(s *model.SetupSession) WebvhVaultSecrets {
	return WebvhVaultSecrets{
		Addr:       o.vaultAddr,
		KVMount:    o.vault.KVMount(),
		SecretPath: vault.DidsPrefix(s.UserID, s.ID) + "/server-secrets",
		K8sRole:    vault.UserName(s.UserID),
		SkipVerify: true,
	}
}

// fsStepDidsP1 runs `did-hosting-daemon setup` (offline-prepare), writing
// bootstrap-request.json to the dids PVC and the offline-bootstrap seed to
// Vault (kubernetes auth).
func (o *Orchestrator) fsStepDidsP1(ctx context.Context, ns string, s *model.SetupSession) error {
	recipe, err := RenderWebvhRecipeTOML(s, WebvhPhasePrepare, "", o.fsWebvhVaultSecrets(s))
	if err != nil {
		return fmt.Errorf("render webvh-recipe.toml: %w", err)
	}

	jobName := k8s.FSJobDidsP1(s.ID)
	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.DidsImage,
		Command:        []string{"sh", "-c", "did-hosting-daemon setup --from /config/webvh-recipe.toml"},
		WorkingDir:     "/work/dids",
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "dids-data", ClaimName: k8s.FSDidsName(s.ID), MountPath: "/work/dids"}},
		ConfigMapName:  jobName,
		ConfigMapKey:   "webvh-recipe.toml",
		ConfigMapData:  recipe,
		Env:            fsNoColorEnv(),
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
		Env: fsNoColorEnv(),
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
	logs, err := o.fsJobLogs(ctx, ns, jobName)
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
	recipe, err := RenderWebvhRecipeTOML(s, WebvhPhaseComplete, digest3a, o.fsWebvhVaultSecrets(s))
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
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "dids-data", ClaimName: k8s.FSDidsName(s.ID), MountPath: "/work/dids"}},
		ConfigMapName:  jobName,
		ConfigMapKey:   "webvh-recipe.toml",
		ConfigMapData:  recipe,
		Env:            fsNoColorEnv(),
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
	logs, err := o.fsJobLogs(ctx, ns, jobName)
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
	cmd := fmt.Sprintf("did-hosting-daemon invite --role admin --did %s", shellQuote(s.DIDHostingAdminDid))

	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.DidsImage,
		Command:        []string{"sh", "-c", cmd},
		WorkingDir:     "/work/dids",
		ServiceAccount: k8s.PodOperatorServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "dids-data", ClaimName: k8s.FSDidsName(s.ID), MountPath: "/work/dids"}},
		Env:            fsNoColorEnv(),
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
	logs, err := o.fsJobLogs(ctx, ns, jobName)
	if err != nil {
		return "", fmt.Errorf("read job logs: %w", err)
	}
	enrollURL, err = ParseDidsEnrollURL(logs)
	if err != nil {
		return "", err
	}
	return enrollURL, nil
}

// fsStepDidsLoadDid loads the VTA + mediator DID logs directly into the dids
// daemon's local store (did-hosting-daemon load-did), reading them straight
// off the vta PVC where step_vta_setup wrote them. Must run before
// fsDeployDids — same reason as fsStepDidsInvite: it opens the local store
// directly, so no daemon pod can be holding the dids PVC yet. Loading these
// offline (rather than registering them over the daemon's control API after
// it's already running) is what lets the dids daemon and the mediator both
// resolve their own DIDs successfully on first boot.
func (o *Orchestrator) fsStepDidsLoadDid(ctx context.Context, ns string, s *model.SetupSession) error {
	jobName := k8s.FSJobDidsLoadDid(s.ID)
	cmd := "did-hosting-daemon load-did --path mediator --did-log /work/vta/data/vta/did-logs/mediator-did.jsonl" +
		" && did-hosting-daemon load-did --path vta --did-log /work/vta/data/vta/did-logs/VTA-did.jsonl"

	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.DidsImage,
		Command:        []string{"sh", "-c", cmd},
		WorkingDir:     "/work/dids",
		ServiceAccount: k8s.PodOperatorServiceAccount,
		PVCMounts: []k8s.PVCMount{
			{Name: "vta-data", ClaimName: k8s.FSVtaName(s.ID), MountPath: "/work/vta"},
			{Name: "dids-data", ClaimName: k8s.FSDidsName(s.ID), MountPath: "/work/dids"},
		},
		Env: fsNoColorEnv(),
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

// fsStepVtaRegisterDids registers the session's dids daemon (3d) in the VTA's
// webvh server registry (`vta did-mgmt servers add --id dids`) — the
// full_stack counterpart of vta_only's `--id control` registration in
// CreateProvisionJob. `servers add` resolves the daemon's DID live and
// requires a hosting service in its DID document, so this must run after
// fsDeployDids; it writes the VTA's fjall store offline, so it must run
// before deploy_vta.
func (o *Orchestrator) fsStepVtaRegisterDids(ctx context.Context, ns string, s *model.SetupSession) error {
	jobName := k8s.FSJobVtaRegisterDids(s.ID)
	cmd := fmt.Sprintf("vta did-mgmt servers add --id dids --did %s --label 'Session DID Hosting Daemon'", shellQuote(s.DIDHostingDid))

	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.VtaImage,
		Command:        []string{"sh", "-c", cmd},
		WorkingDir:     "/work/vta",
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "vta-data", ClaimName: k8s.FSVtaName(s.ID), MountPath: "/work/vta"}},
		Env:            fsNoColorEnv(),
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

// fsDeployDids starts the dids daemon Deployment. Runs as SA vta — it reads
// its Vault-backed secrets at every boot. Waits for the pod to actually
// become Ready before returning: step_vta_register_dids (right after
// deploy_mediator) live-resolves this daemon's DID over HTTPS, so a
// Deployment object existing isn't enough — without this wait it's a race
// that 503s until the pod's up and the Service/Ingress endpoint propagates.
func (o *Orchestrator) fsDeployDids(ctx context.Context, ns string, s *model.SetupSession) error {
	name := k8s.FSDidsName(s.ID)
	if err := o.k8s.CreateComponentDeployment(ctx, ns, k8s.ComponentDeploymentSpec{
		Name:           name,
		Image:          s.DidsImage,
		Command:        []string{"did-hosting-daemon"},
		WorkingDir:     "/work/dids",
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "dids-data", ClaimName: name, MountPath: "/work/dids"}},
		Env:            fsNoColorEnv(),
		Port:           8534,
		Labels:         fsLabels("dids", s.ID),
	}); err != nil {
		return err
	}
	return o.k8s.WaitForComponentDeploymentReady(ctx, ns, name, 2*time.Minute)
}

// fsDeployMediator starts the mediator Deployment. Runs as SA vta — it reads
// its Vault-backed secrets at every boot (kubernetes auth, same as the VTA).
// Waits for Ready for the same reason as fsDeployDids — nothing in plain
// full_stack resolves the mediator over HTTP right after this, but
// full_stack_with_vtc's step_vtc_setup does (§9 [messaging]), so the
// invariant "deploy_* returns only once the component is actually up" holds
// for every component, not just the one known to need it today.
func (o *Orchestrator) fsDeployMediator(ctx context.Context, ns string, s *model.SetupSession) error {
	name := k8s.FSMediatorName(s.ID)
	if err := o.k8s.CreateComponentDeployment(ctx, ns, k8s.ComponentDeploymentSpec{
		Name:           name,
		Image:          s.MediatorImage,
		Command:        []string{"mediator"},
		WorkingDir:     "/work/mediator",
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "mediator-data", ClaimName: name, MountPath: "/work/mediator"}},
		Env:            fsNoColorEnv(),
		Port:           7037,
		Labels:         fsLabels("mediator", s.ID),
	}); err != nil {
		return err
	}
	return o.k8s.WaitForComponentDeploymentReady(ctx, ns, name, 2*time.Minute)
}

// ── Phase 2: step_import_admin_did → deploy_vta → running ──────────────────

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
		Env:            fsNoColorEnv(),
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
		Env:            fsNoColorEnv(),
		Port:           8100,
		Labels:         fsLabels("vta", sessionID),
	}); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "failed to create vta deployment: "+err.Error())
		return
	}
	if err := o.k8s.WaitForComponentDeploymentReady(ctx, ns, name, 2*time.Minute); err != nil {
		if ctx.Err() != nil {
			return
		}
		o.markFailed(sessionID, "vta deployment did not become ready: "+err.Error())
		return
	}

	o.fsSetStatus(sessionID, "running")
	log.Printf("[orchestrator] fs session %d: running at %s", sessionID, s.PublicURL())
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
		"step_dids_load_did", "deploy_dids", "deploy_mediator",
		"step_vta_register_dids",
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

// TeardownMediatorVault deletes the mediator's KV secrets. Best-effort,
// mirrors TeardownVaultSeed. No token to revoke — the mediator authenticates
// via kubernetes auth (design §9), not a minted VAULT_TOKEN.
func (o *Orchestrator) TeardownMediatorVault(ctx context.Context, userID, sessionID uint) {
	if o.vault == nil {
		return
	}
	if err := o.vault.DeleteMediatorSecrets(ctx, userID, sessionID); err != nil {
		log.Printf("[orchestrator] warn: delete mediator secrets (user %d session %d): %v", userID, sessionID, err)
	}
}

// TeardownDidsVault deletes the dids daemon's KV secrets. Best-effort,
// mirrors TeardownMediatorVault.
func (o *Orchestrator) TeardownDidsVault(ctx context.Context, userID, sessionID uint) {
	if o.vault == nil {
		return
	}
	if err := o.vault.DeleteDidsSecrets(ctx, userID, sessionID); err != nil {
		log.Printf("[orchestrator] warn: delete dids secrets (user %d session %d): %v", userID, sessionID, err)
	}
}

// TeardownVtcVault deletes the VTC's KV secrets (full_stack_with_vtc only).
// Best-effort, mirrors TeardownMediatorVault — no token to revoke for the
// VTC either, it authenticates via kubernetes auth like the other three.
func (o *Orchestrator) TeardownVtcVault(ctx context.Context, userID, sessionID uint) {
	if o.vault == nil {
		return
	}
	if err := o.vault.DeleteVtcSecrets(ctx, userID, sessionID); err != nil {
		log.Printf("[orchestrator] warn: delete vtc secrets (user %d session %d): %v", userID, sessionID, err)
	}
}
