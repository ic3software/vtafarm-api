package setup

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"time"

	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
)

type poolActivation struct {
	id         string
	requestID  string
	entryPoint string
	confirmed  time.Time
}

// runPoolActivation handles every confirmed pool claim, regardless of whether
// it came from the admin station or public signup. It returns false only for a
// non-pool session or when recovery must continue an already-created legacy
// provision Job.
func (o *Orchestrator) runPoolActivation(
	ctx context.Context,
	namespace string,
	session *model.SetupSession,
	adminDID, adminLabel, controlDID string,
	started time.Time,
) bool {
	pooled, err := o.isPoolSession(ctx, session.ID)
	if err != nil {
		o.failPoolActivation(session.ID, "", "identify_pool", err)
		return true
	}
	if !pooled {
		return false
	}

	// A provision Job means an older worker already crossed the legacy write
	// boundary. Finish that exact path; never introduce a second PVC writer.
	legacy, err := o.k8s.HasProvisionJob(ctx, namespace, session.ID)
	if err != nil {
		o.failPoolActivation(session.ID, "", "legacy_check", err)
		return true
	}
	if legacy {
		return false
	}

	activation, err := o.loadPoolActivation(ctx, session.ID, adminDID)
	if err != nil {
		o.failPoolActivation(session.ID, "", "load_claim", err)
		return true
	}
	o.logPoolActivation(session.ID, activation, "claim_confirmed", started)
	o.logPoolActivation(session.ID, activation, "hosting_acl_ready", started)

	err = o.k8s.ActivateParkedVtaDeployment(ctx, namespace, k8s.VtaActivationSpec{
		SessionID: session.ID, Image: session.VtaImage, AdminDID: adminDID,
		AdminLabel: adminLabel, ControlDID: controlDID, ActivationID: activation.id,
		GateURL: o.poolActivationGateURL(session.ID, activation.id),
	})
	if err != nil {
		o.failPoolActivation(session.ID, activation.requestID, "patch_deployment", err)
		return true
	}
	o.logPoolActivation(session.ID, activation, "deployment_activation_patched", started)

	if err := o.k8s.WaitForVtaActivation(ctx, namespace, session.ID, 2*time.Minute, func(stage string) {
		o.logPoolActivation(session.ID, activation, stage, started)
	}); err != nil {
		if ctx.Err() == nil {
			o.failPoolActivation(session.ID, activation.requestID, "wait_for_readiness", err)
		}
		return true
	}
	o.logPoolActivation(session.ID, activation, "deployment_ready", started)

	o.finishVtaOnlyDeployment(ctx, session, true)
	o.logPoolActivation(session.ID, activation, "claim_released", started)
	return true
}

func (o *Orchestrator) loadPoolActivation(ctx context.Context, sessionID uint, adminDID string) (*poolActivation, error) {
	var claim model.MobileConnection
	err := o.db.WithContext(ctx).
		Where("session_id = ? AND operation = ? AND admin_did = ? AND confirmed_at IS NOT NULL AND status IN ?",
			sessionID, connection.OperationProvisionVTA, adminDID,
			[]string{"confirmed", "provisioning", "released"}).
		Order("created_at DESC").First(&claim).Error
	if err != nil {
		return nil, fmt.Errorf("load confirmed pool claim: %w", err)
	}
	entryPoint := model.PoolPurposeAdminStation
	if len(claim.PublicAccessDigest) != 0 {
		entryPoint = model.PoolPurposePublicSignup
	}
	confirmed := claim.CreatedAt
	if claim.ConfirmedAt != nil {
		confirmed = *claim.ConfirmedAt
	}
	return &poolActivation{
		id: PoolActivationID(sessionID, claim.ID), requestID: claim.ID,
		entryPoint: entryPoint, confirmed: confirmed,
	}, nil
}

func PoolActivationID(sessionID uint, requestID string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("pool-activation:v1:%d:%s", sessionID, requestID)))
	return fmt.Sprintf("%x", digest[:16])
}

func (o *Orchestrator) poolActivationGateURL(sessionID uint, activationID string) string {
	if o.poolActivationGateBase == "" {
		return ""
	}
	return fmt.Sprintf("%s/%d/%s", o.poolActivationGateBase, sessionID, activationID)
}

func (o *Orchestrator) failPoolActivation(sessionID uint, requestID, stage string, err error) {
	safe := fmt.Sprintf("pooled VTA activation failed during %s", stage)
	log.Printf("[pool-activation] session=%d stage=%s outcome=failed error=%v", sessionID, stage, err)
	o.markFailedAt(sessionID, "pool_activation", safe)
	if requestID != "" {
		if updateErr := o.db.Model(&model.MobileConnection{}).
			Where("id = ? AND status IN ?", requestID, []string{"confirmed", "provisioning"}).
			Update("provision_error", safe).Error; updateErr != nil {
			log.Printf("[pool-activation] session=%d stage=record_failure error=%v", sessionID, updateErr)
		}
	}
}

func (o *Orchestrator) logPoolActivation(sessionID uint, activation *poolActivation, stage string, started time.Time) {
	log.Printf(
		"[pool-activation] session=%d activation=%s entry_point=%s stage=%s elapsed_ms=%d confirmation_ms=%d",
		sessionID, activation.id, activation.entryPoint, stage,
		time.Since(started).Milliseconds(), time.Since(activation.confirmed).Milliseconds(),
	)
}
