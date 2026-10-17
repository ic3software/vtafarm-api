package setup

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
)

// RecoverDeployment retries only the failed deployment, then continues from
// the next state-machine step. It is intentionally separate from image
// upgrades: these sessions never reached running, so there is no healthy
// deployment to roll back to.
func (o *Orchestrator) RecoverDeployment(sessionID uint) {
	o.launch(sessionID, func(ctx context.Context) {
		o.recoverDeployment(ctx, sessionID)
	})
}

func (o *Orchestrator) recoverDeployment(ctx context.Context, sessionID uint) {
	var session model.SetupSession
	if err := o.db.First(&session, sessionID).Error; err != nil {
		log.Printf("[orchestrator] deployment recovery %d: load failed: %v", sessionID, err)
		return
	}

	s := &session
	ns := o.k8s.UserNamespace(fmt.Sprintf("%d", s.UserID))
	fail := func(stage, prefix string, err error) bool {
		if err == nil {
			return false
		}
		if ctx.Err() != nil {
			return true
		}
		o.markFailedAt(sessionID, stage, prefix+": "+err.Error())
		return true
	}
	if s.Mode == model.ModeVtaOnly {
		if s.Status != "deploy_vta" {
			return
		}
		if fail("deploy_vta", "failed to reconcile vta deployment image", o.k8s.RecoverDeploymentImage(
			ctx, ns, k8s.VtaDeploymentName(s.ID), s.VtaImage,
		)) {
			return
		}
		pooled, err := o.deployVtaOnly(ctx, ns, s)
		if fail("deploy_vta", "failed to deploy vta", err) {
			return
		}
		o.finishVtaOnlyDeployment(ctx, s, pooled)
		return
	}
	if s.Mode != model.ModeFullStack {
		return
	}
	if fail(s.Status, "failed to reconcile recovery images", o.reconcileFullStackRecoveryImages(ctx, ns, s)) {
		return
	}

	switch s.Status {
	case "deploy_dids":
		if fail("deploy_dids", "failed to deploy dids daemon", o.fsDeployDids(ctx, ns, s)) {
			return
		}
		o.fsSetStatus(sessionID, "deploy_mediator")
		if fail("deploy_mediator", "failed to deploy mediator", o.fsDeployMediator(ctx, ns, s)) {
			return
		}
		o.continueRecoveredPreGate(ctx, ns, s)

	case "deploy_mediator":
		if fail("deploy_mediator", "failed to deploy mediator", o.fsDeployMediator(ctx, ns, s)) {
			return
		}
		o.continueRecoveredPreGate(ctx, ns, s)

	case "deploy_vta":
		if fail("deploy_vta", "failed to deploy vta", o.fsDeployVta(ctx, ns, s)) {
			return
		}
		o.continueRecoveredPostGate(ctx, ns, s, fail)

	case "deploy_vtc":
		if fail("deploy_vtc", "failed to deploy vtc", o.fsDeployVtc(ctx, ns, s)) {
			return
		}
		o.finishRecoveredSession(sessionID, s)
	}
}

func (o *Orchestrator) reconcileFullStackRecoveryImages(ctx context.Context, ns string, s *model.SetupSession) error {
	components := []struct {
		stage string
		name  string
		image string
	}{
		{stage: "deploy_dids", name: k8s.FSDidsName(s.ID), image: s.DidsImage},
		{stage: "deploy_mediator", name: k8s.FSMediatorName(s.ID), image: s.MediatorImage},
		{stage: "deploy_vta", name: k8s.FSVtaName(s.ID), image: s.VtaImage},
		{stage: "deploy_vtc", name: k8s.FSVtcName(s.ID), image: s.VtcImage},
	}
	failedIndex := -1
	for i, component := range components {
		if component.stage == s.Status {
			failedIndex = i
			break
		}
	}
	if failedIndex < 0 {
		return fmt.Errorf("unsupported recovery stage %s", s.Status)
	}
	for i := 0; i <= failedIndex; i++ {
		component := components[i]
		if err := o.k8s.RecoverDeploymentImage(ctx, ns, component.name, component.image); err != nil {
			return fmt.Errorf("reconcile %s: %w", component.name, err)
		}
		// The failed component's normal deploy helper below creates any missing
		// resources and performs its readiness wait. Components that were already
		// running must finish their replacement rollout before setup continues.
		if i < failedIndex {
			if err := o.k8s.WaitForComponentDeploymentReady(ctx, ns, component.name, 2*time.Minute); err != nil {
				return fmt.Errorf("wait for %s: %w", component.name, err)
			}
		}
	}
	return nil
}

func (o *Orchestrator) continueRecoveredPreGate(ctx context.Context, ns string, s *model.SetupSession) {
	o.fsSetStatus(s.ID, "step_vta_register_dids")
	if err := o.fsStepVtaRegisterDids(ctx, ns, s); err != nil {
		if ctx.Err() == nil {
			o.markFailedAt(s.ID, "step_vta_register_dids", "vta register-dids failed: "+err.Error())
		}
		return
	}
	if s.AdminDid != "" {
		// The DID was accepted before this deployment failed. Do not send it
		// through AcceptManual again; restore a post-gate status and let the
		// crash-safe provision worker continue with the persisted DID.
		o.fsSetStatus(s.ID, "step_import_admin_did")
		o.KickProvision(s.ID)
		return
	}
	o.fsSetStatus(s.ID, "awaiting_admin_did")
	log.Printf("[orchestrator] recovered full-stack session %d: awaiting admin DID", s.ID)
}

func (o *Orchestrator) continueRecoveredPostGate(
	ctx context.Context,
	ns string,
	s *model.SetupSession,
	fail func(stage, prefix string, err error) bool,
) {
	o.fsSetStatus(s.ID, "step_vtc_setup")
	outcome, err := o.fsStepVtcSetup(ctx, ns, s)
	if fail("step_vtc_setup", "vtc setup failed", err) {
		return
	}
	s.VtcDid, s.VtcAdminDid = outcome.VtcDid, outcome.AdminDid
	s.VtcInstallURL, s.VtcClaimCode = outcome.InstallURL, outcome.ClaimCode
	if err := o.db.WithContext(ctx).Model(&model.SetupSession{}).Where("id = ?", s.ID).Updates(map[string]any{
		"vtc_did": outcome.VtcDid, "vtc_admin_did": outcome.AdminDid,
		"vtc_install_url": outcome.InstallURL, "vtc_claim_code": outcome.ClaimCode,
		"vtc_install_used": false, "updated_at": time.Now(),
	}).Error; err != nil {
		if ctx.Err() == nil {
			o.markFailedAt(s.ID, "step_vtc_setup", "persist vtc setup output failed: "+err.Error())
		}
		return
	}

	o.fsSetStatus(s.ID, "deploy_vtc")
	if fail("deploy_vtc", "failed to deploy vtc", o.fsDeployVtc(ctx, ns, s)) {
		return
	}
	o.finishRecoveredSession(s.ID, s)
}

func (o *Orchestrator) finishRecoveredSession(sessionID uint, s *model.SetupSession) {
	o.db.Model(&model.SetupSession{}).Where("id = ?", sessionID).Updates(map[string]any{
		"status": "running", "error_msg": "", "failed_stage": "", "updated_at": time.Now(),
	})
	log.Printf("[orchestrator] recovered full-stack session %d: running at https://%s", sessionID, s.VtcFQDN())
}
