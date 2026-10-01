package setup

import (
	"context"
	"database/sql/driver"
	"log"
	"time"

	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/model"
)

// RunProvisionQueue recovers work committed before an API crash. Development
// against the shared database leaves this disabled with ORCHESTRATOR_RESUME.
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
		var work []model.InitialProvision
		if err := o.db.WithContext(ctx).Where("finished_at IS NULL").Find(&work).Error; err == nil {
			for _, op := range work {
				o.KickProvision(op.SessionID)
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
		o.runInitialProvision(ctx, sessionID, func(ctx context.Context, s *model.SetupSession, did string) {
			if s.IsFullStack() {
				o.runFullStackFinish(ctx, s.ID, did)
			} else {
				o.runProvision(ctx, s.ID, did)
			}
		})
	}()
}

func (o *Orchestrator) runInitialProvision(ctx context.Context, sessionID uint, run func(context.Context, *model.SetupSession, string)) {
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
	// Kubernetes Job identities remain stable even if the DB connection is lost.
	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1, $2)", 565441, int64(sessionID)).Scan(&locked); err != nil || !locked {
		return
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(releaseCtx, "SELECT pg_advisory_unlock($1, $2)", 565441, int64(sessionID)); err != nil {
			// Do not return a connection with an uncertain lock to the pool.
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
				var stillPending bool
				err := conn.QueryRowContext(pingCtx, "SELECT EXISTS (SELECT 1 FROM initial_provisions WHERE session_id = $1 AND finished_at IS NULL)", sessionID).Scan(&stillPending)
				stop()
				if err != nil || !stillPending {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-heartbeatDone }()
	var op model.InitialProvision
	if err := o.db.WithContext(runCtx).First(&op, "session_id = ?", sessionID).Error; err != nil || op.FinishedAt != nil {
		return
	}
	var s model.SetupSession
	if err := o.db.WithContext(runCtx).First(&s, sessionID).Error; err != nil {
		return
	}
	if s.Status != "running" && s.Status != "failed" {
		if s.AdminDid != op.AdminDid {
			log.Printf("[provision] session %d has conflicting admin DID", sessionID)
			return
		}
		run(runCtx, &s, op.AdminDid)
	}
	if runCtx.Err() != nil {
		return
	}
	if err := o.db.WithContext(runCtx).First(&s, sessionID).Error; err != nil {
		return
	}
	if s.Status == "running" || s.Status == "failed" {
		o.db.WithContext(runCtx).Model(&op).Update("finished_at", time.Now())
	}
}

func (o *Orchestrator) queueProvision(sessionID uint, adminDid string) {
	if _, err := connection.AcceptManual(context.Background(), o.db, sessionID, adminDid, true); err != nil {
		log.Printf("[provision] could not queue session %d: %v", sessionID, err)
		return
	}
	o.KickProvision(sessionID)
}
