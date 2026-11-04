package setup

import (
	"context"
	"log"
	"strconv"
	"time"

	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
)

const poolPrewarmReconcileInterval = 10 * time.Second

type poolPrewarmRun struct {
	cancel context.CancelFunc
}

// KickPoolPrewarm reconciles one pool member after a claim, rejection, or
// expiry transition. A newer durable transition cancels an older in-process
// reconcile so rejection cannot lose a race to a stale prewarm write.
func (o *Orchestrator) KickPoolPrewarm(sessionID uint) {
	if o == nil || o.k8s == nil || sessionID == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &poolPrewarmRun{cancel: cancel}
	o.mu.Lock()
	if o.prewarms == nil {
		o.prewarms = make(map[uint]*poolPrewarmRun)
	}
	if running := o.prewarms[sessionID]; running != nil {
		running.cancel()
	}
	o.prewarms[sessionID] = run
	o.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			o.mu.Lock()
			if o.prewarms[sessionID] == run {
				delete(o.prewarms, sessionID)
			}
			o.mu.Unlock()
		}()
		o.reconcilePoolPrewarm(ctx, sessionID)
	}()
}

func (o *Orchestrator) reconcilePoolPrewarm(ctx context.Context, sessionID uint) {
	var session model.SetupSession
	if err := o.db.WithContext(ctx).First(&session, sessionID).Error; err != nil {
		return
	}
	pooled, err := o.isPoolSession(ctx, sessionID)
	if err != nil || !pooled {
		return
	}
	claim, err := connection.Current(o.db.WithContext(ctx), sessionID)
	if err != nil || claim == nil {
		return
	}
	activationID := PoolActivationID(sessionID, claim.ID)

	if claim.Status == "claimed" {
		if err := connection.Reconcile(ctx, o.db, claim, &session); err != nil {
			log.Printf("[pool-prewarm] session=%d stage=reconcile_claim outcome=failed error=%v", sessionID, err)
			return
		}
	}
	if claim.Status != "claimed" {
		switch claim.Status {
		case "confirmed", "provisioning", "released":
			return
		}
		if err := o.k8s.CancelPoolPrewarm(ctx, o.k8s.UserNamespace(strconv.FormatUint(uint64(session.UserID), 10)), sessionID, activationID); err != nil && ctx.Err() == nil {
			log.Printf("[pool-prewarm] session=%d activation=%s stage=cancel outcome=failed error=%v", sessionID, activationID, err)
		}
		return
	}
	if session.Status != model.StatusPoolWaiting || session.AdminDid != "" || claim.Operation != connection.OperationProvisionVTA {
		return
	}
	if o.poolActivationGateBase == "" {
		log.Printf("[pool-prewarm] session=%d activation=%s stage=start outcome=disabled reason=activation_gate_unavailable", sessionID, activationID)
		return
	}
	controlDID, err := o.poolPrewarmControlDID(&session)
	if err != nil {
		log.Printf("[pool-prewarm] session=%d activation=%s stage=load_control_did outcome=failed error=%v", sessionID, activationID, err)
		return
	}
	namespace := o.k8s.UserNamespace(strconv.FormatUint(uint64(session.UserID), 10))
	err = o.k8s.PrewarmParkedVtaDeployment(ctx, namespace, k8s.VtaActivationSpec{
		SessionID: session.ID, Image: session.VtaImage, AdminDID: claim.AdminDid,
		AdminLabel: connection.MobileAdminLabel, ControlDID: controlDID,
		ActivationID: activationID, GateURL: o.poolActivationGateURL(session.ID, activationID),
	})
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[pool-prewarm] session=%d activation=%s stage=start outcome=failed error=%v", sessionID, activationID, err)
		}
		return
	}
	log.Printf("[pool-prewarm] session=%d activation=%s stage=pod_requested outcome=ready", sessionID, activationID)
	if err := o.k8s.WaitForPoolPrewarm(ctx, namespace, sessionID, activationID, 2*time.Minute); err != nil {
		if ctx.Err() == nil {
			log.Printf("[pool-prewarm] session=%d activation=%s stage=gate_ready outcome=failed error=%v", sessionID, activationID, err)
		}
		return
	}
	log.Printf("[pool-prewarm] session=%d activation=%s stage=gate_ready outcome=ready", sessionID, activationID)
}

func (o *Orchestrator) poolPrewarmControlDID(session *model.SetupSession) (string, error) {
	if session.ConnectionSource == model.ConnectionExternal {
		return session.DIDHostingDid, nil
	}
	if o.didHosting == nil {
		return "", nil
	}
	dh, err := o.didHosting.For(session.DidHostingControlURL, session.DIDHostingDid)
	if err != nil {
		return "", err
	}
	return dh.ServerDid(), nil
}

// RunPoolPrewarmQueue repairs claimed prewarms and removes abandoned gate Pods
// after an API restart. Only Deployments carrying the prewarm label are scanned.
func (o *Orchestrator) RunPoolPrewarmQueue(ctx context.Context) {
	ticker := time.NewTicker(poolPrewarmReconcileInterval)
	defer ticker.Stop()
	for {
		o.reconcilePoolPrewarmQueue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (o *Orchestrator) reconcilePoolPrewarmQueue(ctx context.Context) {
	var claimed []uint
	if err := o.db.WithContext(ctx).Table("mobile_connections mc").
		Distinct("mc.session_id").
		Joins("JOIN pool_sessions ps ON ps.session_id = mc.session_id").
		Where("mc.status = ?", "claimed").
		Scan(&claimed).Error; err != nil {
		if ctx.Err() == nil {
			log.Printf("[pool-prewarm] stage=reconcile_claimed outcome=failed error=%v", err)
		}
		return
	}
	seen := make(map[uint]struct{}, len(claimed))
	for _, sessionID := range claimed {
		seen[sessionID] = struct{}{}
		o.KickPoolPrewarm(sessionID)
	}
	prewarms, err := o.k8s.ListPoolPrewarms(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[pool-prewarm] stage=list outcome=failed error=%v", err)
		}
		return
	}
	for i := range prewarms {
		if _, ok := seen[prewarms[i].SessionID]; ok {
			continue
		}
		o.KickPoolPrewarm(prewarms[i].SessionID)
	}
}
