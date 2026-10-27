package pool

import (
	"context"
	"database/sql/driver"
	"errors"
	"log"
	"time"

	"github.com/ic3software/vtafarm-api/internal/model"
	"gorm.io/gorm"
)

const (
	workerCount       = 4
	reconcileInterval = 10 * time.Second
	workerLockClass   = 565445
)

type Runner struct {
	db      *gorm.DB
	prepare func(context.Context, uint) error
	build   func(context.Context, uint) error
	cleanup func(context.Context, uint) error
	queue   chan uint
}

func NewRunner(
	db *gorm.DB,
	prepare func(context.Context, uint) error,
	build func(context.Context, uint) error,
	cleanup func(context.Context, uint) error,
) *Runner {
	return &Runner{db: db, prepare: prepare, build: build, cleanup: cleanup, queue: make(chan uint, 256)}
}

func (r *Runner) Start(ctx context.Context, reconcileExisting bool) {
	for range workerCount {
		go r.runWorker(ctx)
	}
	if !reconcileExisting {
		return
	}
	go func() {
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		for {
			r.reconcile(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (r *Runner) Kick(sessionID uint) {
	select {
	case r.queue <- sessionID:
	default:
		// The periodic reconciliation loop is the durable fallback.
	}
}

func (r *Runner) reconcile(ctx context.Context) {
	var sessionIDs []uint
	err := r.db.WithContext(ctx).Table("pool_sessions ps").
		Select("ps.session_id").
		Joins("JOIN setup_sessions s ON s.id = ps.session_id").
		Where("s.status IN ?", []string{
			model.StatusPoolCreating, "vta_setup_running", model.StatusPoolPublishing,
			model.StatusPoolPrepared, "deleting",
		}).Order("ps.session_id").Limit(500).Scan(&sessionIDs).Error
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[pool] reconcile failed: %v", err)
		}
		return
	}
	for _, sessionID := range sessionIDs {
		r.Kick(sessionID)
	}
	if err := FinalizeEmptyBatchCleanups(ctx, r.db); err != nil && ctx.Err() == nil {
		log.Printf("[pool] finalize empty batch cleanup failed: %v", err)
	}
}

func (r *Runner) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case sessionID := <-r.queue:
			r.runOne(ctx, sessionID)
		}
	}
}

func (r *Runner) runOne(ctx context.Context, sessionID uint) {
	pool, err := r.db.DB()
	if err != nil {
		return
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		return
	}
	defer conn.Close()
	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1, $2)", workerLockClass, int64(sessionID)).Scan(&locked); err != nil || !locked {
		return
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(releaseCtx, "SELECT pg_advisory_unlock($1, $2)", workerLockClass, int64(sessionID)); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()

	var session model.SetupSession
	if err := r.db.WithContext(ctx).First(&session, sessionID).Error; err != nil {
		return
	}
	if session.Status == "deleting" {
		if r.cleanup != nil {
			if err := r.cleanup(ctx, sessionID); err != nil && ctx.Err() == nil {
				r.recordCleanupError(sessionID, err)
			}
		}
		return
	}
	if session.Status == model.StatusPoolWaiting || session.Status == "running" || session.Status == "failed" {
		return
	}
	if r.prepare == nil || r.build == nil {
		r.recordBuildError(sessionID, errors.New("pool worker is not configured"))
		return
	}
	if err := r.prepare(ctx, sessionID); err != nil {
		if ctx.Err() == nil {
			r.recordBuildError(sessionID, err)
		}
		return
	}
	if err := r.build(ctx, sessionID); err != nil && ctx.Err() == nil {
		r.recordBuildError(sessionID, err)
	}
}

func (r *Runner) recordBuildError(sessionID uint, err error) {
	if updateErr := r.db.Model(&model.SetupSession{}).
		Where("id = ? AND status <> 'deleting'", sessionID).
		Updates(map[string]any{"status": "failed", "error_msg": err.Error()}).Error; updateErr != nil {
		log.Printf("[pool] session %d: failed to record build error: %v", sessionID, updateErr)
	}
}

func (r *Runner) recordCleanupError(sessionID uint, err error) {
	if updateErr := r.db.Model(&model.SetupSession{}).
		Where("id = ? AND status = 'deleting'", sessionID).
		Update("error_msg", err.Error()).Error; updateErr != nil {
		log.Printf("[pool] session %d: failed to record cleanup error: %v", sessionID, updateErr)
	}
}
