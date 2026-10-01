package setup

import (
	"context"
	"database/sql/driver"
	"log"
	"time"

	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/model"
)

var provisionStatuses = []string{
	"provisioning", "step_import_admin_did", "deploy_vta", "step_vtc_setup_key",
	"step_vtc_acl_grant", "step_vtc_setup", "deploy_vtc",
}

// RunProvisionQueue recovers setup work committed before an API crash.
// Development against the shared database leaves this disabled with
// ORCHESTRATOR_RESUME.
func (o *Orchestrator) RunProvisionQueue(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	defer func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		for _, cancel := range o.provisions {
			cancel()
		}
	}()
	for {
		var work []model.SetupSession
		if err := o.db.WithContext(ctx).
			Where("admin_did <> '' AND status IN ?", provisionStatuses).
			Find(&work).Error; err == nil {
			for i := range work {
				o.KickProvision(work[i].ID)
			}
		} else if ctx.Err() == nil {
			log.Printf("[provision] recovery query failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (o *Orchestrator) KickProvision(sessionID uint) {
	ctx, cancel := context.WithCancel(context.Background())
	o.mu.Lock()
	if o.provisions == nil {
		o.provisions = make(map[uint]context.CancelFunc)
	}
	if _, running := o.provisions[sessionID]; running {
		o.mu.Unlock()
		cancel()
		return
	}
	o.provisions[sessionID] = cancel
	o.mu.Unlock()
	go func() {
		defer func() { cancel(); o.mu.Lock(); delete(o.provisions, sessionID); o.mu.Unlock() }()
		o.runProvisionOperation(ctx, sessionID, func(ctx context.Context, s *model.SetupSession, did string) {
			if s.IsFullStack() {
				o.runFullStackFinish(ctx, s.ID, did)
			} else {
				o.runProvision(ctx, s.ID, did)
			}
		})
	}()
}

func (o *Orchestrator) runProvisionOperation(ctx context.Context, sessionID uint, run func(context.Context, *model.SetupSession, string)) {
	pool, err := o.db.DB()
	if err != nil {
		return
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		return
	}
	defer conn.Close()
	// A dedicated SQL connection keeps the advisory lock across transactions.
	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1, $2)", 565441, int64(sessionID)).Scan(&locked); err != nil || !locked {
		return
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(releaseCtx, "SELECT pg_advisory_unlock($1, $2)", 565441, int64(sessionID)); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tick.C:
				pingCtx, stop := context.WithTimeout(runCtx, 3*time.Second)
				var active bool
				err := conn.QueryRowContext(pingCtx, "SELECT EXISTS (SELECT 1 FROM setup_sessions WHERE id = $1 AND status <> 'deleting')", sessionID).Scan(&active)
				stop()
				if err != nil || !active {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-heartbeatDone }()
	var s model.SetupSession
	if err := o.db.WithContext(runCtx).First(&s, sessionID).Error; err != nil || !connection.ResumableProvisionStatus(s.Status) || s.AdminDid == "" {
		return
	}
	run(runCtx, &s, s.AdminDid)
}

func (o *Orchestrator) queueProvision(sessionID uint, adminDid string) {
	if err := connection.AcceptManual(context.Background(), o.db, sessionID, adminDid, true); err != nil {
		log.Printf("[provision] could not queue session %d: %v", sessionID, err)
		return
	}
	o.KickProvision(sessionID)
}
