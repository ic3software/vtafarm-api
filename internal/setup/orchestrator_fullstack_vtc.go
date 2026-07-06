package setup

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/vault"
)

// This file drives full_stack_with_vtc's post-gate finish phase (design
// docs/full-stack-with-vtc-setup-design.md §5/§6/§8). The pre-gate pipeline
// is full_stack's runFullStack, reused unchanged — Start dispatches both
// modes there, and fsK8sProvision adds the fourth component's PVC/Service/
// Ingress. Only the finish differs: two offline steps land in the post-gate
// VTA-store window before deploy_vta (the ephemeral setup key + its
// context/ACL grant — placing the grant post-gate keeps its 1h expiry window
// at minutes no matter how long the admin-DID gate takes), then the one
// genuinely live step of the whole mode (`vtc setup --from`) and the VTC
// Deployment run after it, when the VTA/mediator/dids are all up.

// runFullStackWithVtcFinish mirrors runFullStackFinish, wrapping the four
// VTC steps around the shared import/deploy helpers:
// step_import_admin_did → step_vtc_setup_key → step_vtc_acl_grant →
// deploy_vta → step_vtc_setup → deploy_vtc → running.
func (o *Orchestrator) runFullStackWithVtcFinish(ctx context.Context, sessionID uint, adminDid string) {
	var session model.SetupSession
	if err := o.db.First(&session, sessionID).Error; err != nil {
		log.Printf("[orchestrator] fs-vtc finish %d: load failed: %v", sessionID, err)
		return
	}
	s := &session
	ns := o.k8s.UserNamespace(fmt.Sprintf("%d", s.UserID))

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

	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"status": "step_import_admin_did", "admin_did": adminDid, "updated_at": time.Now(),
	})
	log.Printf("[orchestrator] fs-vtc session %d: importing admin DID %s", sessionID, adminDid)
	if fail("import-admin-did failed", o.fsStepImportAdminDid(ctx, ns, s, adminDid)) {
		return
	}

	// step_vtc_setup_key
	o.fsSetStatus(sessionID, "step_vtc_setup_key")
	setupKeyDid, err := o.fsStepVtcSetupKey(ctx, ns, s)
	if fail("vtc setup-key generation failed", err) {
		return
	}
	// Persisted for audit/debug only — step_vtc_acl_grant takes the value
	// straight from this run, never back from the DB.
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Update("vtc_setup_key_did", setupKeyDid)
	log.Printf("[orchestrator] fs-vtc session %d: setup_key_did=%s", sessionID, setupKeyDid)

	// step_vtc_acl_grant
	o.fsSetStatus(sessionID, "step_vtc_acl_grant")
	if fail("vtc acl grant failed", o.fsStepVtcAclGrant(ctx, ns, s, setupKeyDid)) {
		return
	}

	// deploy_vta
	o.fsSetStatus(sessionID, "deploy_vta")
	if fail("failed to deploy vta", o.fsDeployVta(ctx, ns, s)) {
		return
	}

	// step_vtc_setup — the mode's only live step: a real network round-trip
	// against the VTA (plus mediator resolution via [messaging] and the
	// daemon-hosted publish via [webvh]), so it runs after deploy_vta with
	// all three earlier components already up (design §5).
	o.fsSetStatus(sessionID, "step_vtc_setup")
	outcome, err := o.fsStepVtcSetup(ctx, ns, s)
	if fail("vtc setup failed", err) {
		return
	}
	s.VtcDid, s.VtcAdminDid = outcome.VtcDid, outcome.AdminDid
	s.VtcInstallURL, s.VtcClaimCode = outcome.InstallURL, outcome.ClaimCode
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"vtc_did": outcome.VtcDid, "vtc_admin_did": outcome.AdminDid,
		"vtc_install_url": outcome.InstallURL, "vtc_claim_code": outcome.ClaimCode,
		"vtc_install_used": false, "updated_at": time.Now(),
	})
	log.Printf("[orchestrator] fs-vtc session %d: vtc_did=%s", sessionID, outcome.VtcDid)

	// deploy_vtc
	o.fsSetStatus(sessionID, "deploy_vtc")
	if fail("failed to deploy vtc", o.fsDeployVtc(ctx, ns, s)) {
		return
	}

	o.fsSetStatus(sessionID, "running")
	log.Printf("[orchestrator] fs-vtc session %d: running at https://%s", sessionID, s.VtcFQDN())
}

// fsStepVtcSetupKey runs `vtc setup generate-key`, persisting the ephemeral
// setup key to the VTC PVC (setup-key.json — later loaded by step_vtc_setup's
// TOML via setup_key_file) and capturing its did:key from stdout. No Vault
// access needed for this step, so it runs as the plain pod-operator SA.
func (o *Orchestrator) fsStepVtcSetupKey(ctx context.Context, ns string, s *model.SetupSession) (string, error) {
	jobName := k8s.FSJobVtcSetupKey(s.ID)
	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.VtcImage,
		Command:        []string{"sh", "-c", "vtc setup generate-key --out /work/vtc/setup-key.json"},
		WorkingDir:     "/work/vtc",
		ServiceAccount: k8s.PodOperatorServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "vtc-data", ClaimName: k8s.FSVtcName(s.ID), MountPath: "/work/vtc"}},
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
	return ParseVtcSetupKeyDid(logs)
}

// fsStepVtcAclGrant creates the VTA context the VTC's community lives under
// (id = VtcName, avoiding collisions if the VTA ever hosts more than one
// context) with the ephemeral setup key as its admin, expiring in 1h —
// `vta contexts create` is an offline fjall write with an atomic ACL entry.
//
// Resume tolerance (design §8): on re-run the create 409s with "context
// already exists" while a freshly re-minted setup key still needs its grant,
// so the command falls back to the exists-tolerant, context-scoped
// `vta import-did --role admin` upsert — the same command family the PNM
// import uses. No expiry flag on that path; acceptable for a retry. Any
// non-Conflict failure still fails the Job with its original exit code.
func (o *Orchestrator) fsStepVtcAclGrant(ctx context.Context, ns string, s *model.SetupSession, setupKeyDid string) error {
	create := fmt.Sprintf(`vta contexts create --id %s --name "VTC" --admin-did %s --admin-expires 1h`,
		shellQuote(s.VtcName), shellQuote(setupKeyDid))
	regrant := fmt.Sprintf(`vta import-did --did %s --role admin --context %s --label vtc-setup`,
		shellQuote(setupKeyDid), shellQuote(s.VtcName))
	cmd := fmt.Sprintf(`out=$(%s 2>&1); code=$?; echo "$out"; `+
		`if [ "$code" -ne 0 ]; then case "$out" in *"already exists"*) %s;; *) exit "$code";; esac; fi`,
		create, regrant)

	jobName := k8s.FSJobVtcAclGrant(s.ID)
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

// fsStepVtcSetup runs the live `vtc setup --from vtc-setup.toml`:
// authenticates to the running VTA with the granted ephemeral key, resolves
// the mediator from [messaging], publishes the VTC's did:webvh through the
// registered "dids" server, and writes the key bundle to Vault (kubernetes
// auth — hence SA vta). Captures the terse completion block.
func (o *Orchestrator) fsStepVtcSetup(ctx context.Context, ns string, s *model.SetupSession) (VtcSetupOutcome, error) {
	toml, err := RenderVtcSetupTOML(s, VtcVaultSecrets{
		Addr:       o.vaultAddr,
		KVMount:    o.vault.KVMount(),
		SecretPath: vault.VtcPrefix(s.UserID, s.ID) + "/key-bundle",
		K8sRole:    vault.UserName(s.UserID),
		SkipVerify: true,
	})
	if err != nil {
		return VtcSetupOutcome{}, fmt.Errorf("render vtc-setup.toml: %w", err)
	}

	jobName := k8s.FSJobVtcSetup(s.ID)
	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          s.VtcImage,
		Command:        []string{"sh", "-c", "vtc setup --from /config/vtc-setup.toml"},
		WorkingDir:     "/work/vtc",
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "vtc-data", ClaimName: k8s.FSVtcName(s.ID), MountPath: "/work/vtc"}},
		ConfigMapName:  jobName,
		ConfigMapKey:   "vtc-setup.toml",
		ConfigMapData:  toml,
		Env:            fsNoColorEnv(),
	}); err != nil {
		return VtcSetupOutcome{}, fmt.Errorf("create job: %w", err)
	}

	succeeded, failMsg, err := o.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		return VtcSetupOutcome{}, err
	}
	if !succeeded {
		return VtcSetupOutcome{}, o.fsJobFailErr(ctx, ns, jobName, failMsg)
	}
	logs, err := o.fsJobLogs(ctx, ns, jobName)
	if err != nil {
		return VtcSetupOutcome{}, fmt.Errorf("read job logs: %w", err)
	}
	return ParseVtcSetupOutput(logs)
}

// fsDeployVtc starts the VTC Deployment (image entrypoint — REST + admin SPA
// + public website on 8200). Runs as SA vta — it reads its Vault key bundle
// at every boot. Waits for Ready, keeping the deploy_* invariant the other
// components hold.
func (o *Orchestrator) fsDeployVtc(ctx context.Context, ns string, s *model.SetupSession) error {
	name := k8s.FSVtcName(s.ID)
	if err := o.k8s.CreateComponentDeployment(ctx, ns, k8s.ComponentDeploymentSpec{
		Name:           name,
		Image:          s.VtcImage,
		Command:        nil, // image entrypoint
		WorkingDir:     "/work/vtc",
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "vtc-data", ClaimName: name, MountPath: "/work/vtc"}},
		Env:            fsNoColorEnv(),
		Port:           8200,
		Labels:         fsLabels("vtc", s.ID),
	}); err != nil {
		return err
	}
	return o.k8s.WaitForComponentDeploymentReady(ctx, ns, name, 2*time.Minute)
}
