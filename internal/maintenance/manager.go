package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/model"
)

// advisoryKey is a process-independent farm-wide read/write lock. Workflow
// requests hold the shared form; maintenance transitions take the exclusive
// form. The value is fixed so API and worker replicas coordinate through
// PostgreSQL rather than process memory.
const advisoryKey int64 = 6219552945673551181

var (
	ErrEnabled              = errors.New("maintenance mode is enabled")
	ErrOperationsInProgress = errors.New("operations are in progress")
)

type State struct {
	Enabled   bool       `json:"enabled"`
	Message   string     `json:"message"`
	EnabledAt *time.Time `json:"enabled_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type ActiveOperation struct {
	OperationType string    `json:"operation_type" gorm:"column:operation_type"`
	Target        string    `json:"target"`
	StartedAt     time.Time `json:"started_at" gorm:"column:started_at"`
}

type Snapshot struct {
	State
	CanEnable        bool              `json:"can_enable"`
	ActiveOperations []ActiveOperation `json:"active_operations"`
}

type Manager struct {
	db  *gorm.DB
	sql *sql.DB
}

func New(db *gorm.DB) (*Manager, error) {
	pool, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("maintenance database pool: %w", err)
	}
	return &Manager{db: db, sql: pool}, nil
}

func (m *Manager) State(ctx context.Context) (State, error) {
	var row model.MaintenanceMode
	if err := m.db.WithContext(ctx).First(&row, 1).Error; err != nil {
		return State{}, err
	}
	return stateFromModel(row), nil
}

func stateFromModel(row model.MaintenanceMode) State {
	return State{Enabled: row.Enabled, Message: row.Message, EnabledAt: row.EnabledAt, UpdatedAt: row.UpdatedAt}
}

// Acquire admits one workflow and holds a shared advisory lock until Release.
// Long-running asynchronous workflows persist their own active state before
// releasing this admission lease; Set rechecks those durable states while it
// owns the exclusive lock.
func (m *Manager) Acquire(ctx context.Context, operationType, target string, allowDuringMaintenance bool) (func(), error) {
	conn, err := m.sql.Conn(ctx)
	if err != nil {
		return nil, err
	}
	cleanupConn := true
	defer func() {
		if cleanupConn {
			_ = conn.Close()
		}
	}()

	if _, err = conn.ExecContext(ctx, "SELECT pg_advisory_lock_shared($1)", advisoryKey); err != nil {
		return nil, err
	}
	locked := true
	defer func() {
		if locked {
			_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock_shared($1)", advisoryKey)
		}
	}()

	var enabled bool
	if err = conn.QueryRowContext(ctx, "SELECT enabled FROM maintenance_mode WHERE id = 1").Scan(&enabled); err != nil {
		return nil, err
	}
	if enabled && !allowDuringMaintenance {
		return nil, ErrEnabled
	}

	operationType = strings.TrimSpace(operationType)
	if operationType == "" {
		operationType = "workflow"
	}
	target = strings.TrimSpace(target)
	var id uint
	if err = conn.QueryRowContext(ctx,
		"INSERT INTO operation_locks (operation_type, target, lock_key) VALUES ($1, $2, $3) RETURNING id",
		operationType, target, advisoryKey,
	).Scan(&id); err != nil {
		return nil, err
	}

	cleanupConn = false
	locked = false
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = conn.ExecContext(ctx, "DELETE FROM operation_locks WHERE id = $1", id)
			_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock_shared($1)", advisoryKey)
			_ = conn.Close()
		})
	}, nil
}

func (m *Manager) Snapshot(ctx context.Context) (Snapshot, error) {
	state, err := m.State(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	operations, err := m.activeOperations(ctx, true)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{State: state, CanEnable: !state.Enabled && len(operations) == 0, ActiveOperations: operations}, nil
}

// Set changes the persisted switch only while holding the exclusive advisory
// lock. A failed try means a synchronous workflow still owns a shared lock.
// Once exclusive, durable async states are checked before the transition.
func (m *Manager) Set(ctx context.Context, enabled bool, message string, adminID uint) (Snapshot, error) {
	conn, err := m.sql.Conn(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer conn.Close()

	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", advisoryKey).Scan(&locked); err != nil {
		return Snapshot{}, err
	}
	if !locked {
		operations, listErr := m.activeOperations(ctx, false)
		if listErr != nil {
			return Snapshot{}, listErr
		}
		state, stateErr := m.State(ctx)
		if stateErr != nil {
			return Snapshot{}, stateErr
		}
		return Snapshot{State: state, CanEnable: false, ActiveOperations: operations}, ErrOperationsInProgress
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryKey) }()

	// No live shared holder remains, so any display rows left behind belong to
	// dead database connections and are safe to discard.
	if _, err := conn.ExecContext(ctx, "DELETE FROM operation_locks WHERE lock_key = $1", advisoryKey); err != nil {
		return Snapshot{}, err
	}
	operations, err := m.durableOperations(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if len(operations) > 0 {
		state, stateErr := m.State(ctx)
		if stateErr != nil {
			return Snapshot{}, stateErr
		}
		return Snapshot{State: state, CanEnable: false, ActiveOperations: operations}, ErrOperationsInProgress
	}

	now := time.Now().UTC()
	var enabledAt any
	var enabledBy any
	if enabled {
		enabledAt, enabledBy = now, adminID
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE maintenance_mode
		SET enabled = $1, message = $2, enabled_at = $3,
		    enabled_by_admin_id = $4, updated_at = $5
		WHERE id = 1`, enabled, strings.TrimSpace(message), enabledAt, enabledBy, now); err != nil {
		return Snapshot{}, err
	}

	state, err := m.State(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{State: state, CanEnable: !state.Enabled, ActiveOperations: []ActiveOperation{}}, nil
}

func (m *Manager) activeOperations(ctx context.Context, cleanStale bool) ([]ActiveOperation, error) {
	if cleanStale {
		conn, err := m.sql.Conn(ctx)
		if err != nil {
			return nil, err
		}
		var exclusive bool
		err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", advisoryKey).Scan(&exclusive)
		if err == nil && exclusive {
			_, err = conn.ExecContext(ctx, "DELETE FROM operation_locks WHERE lock_key = $1", advisoryKey)
			_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryKey)
		}
		_ = conn.Close()
		if err != nil {
			return nil, err
		}
	}

	transient := make([]ActiveOperation, 0)
	if err := m.db.WithContext(ctx).Model(&model.OperationLock{}).
		Select("operation_type, target, started_at").Order("started_at").Scan(&transient).Error; err != nil {
		return nil, err
	}
	durable, err := m.durableOperations(ctx)
	if err != nil {
		return nil, err
	}
	return append(transient, durable...), nil
}

func (m *Manager) durableOperations(ctx context.Context) ([]ActiveOperation, error) {
	const query = `
		SELECT 'vta_workflow' AS operation_type, vta_name AS target, updated_at AS started_at
		FROM setup_sessions
		WHERE status NOT IN ('running', 'failed', 'vta_setup_complete', 'awaiting_admin_did', 'awaiting_did_publication', 'pool_prepared', 'pool_waiting')
		UNION ALL
		SELECT 'image_upgrade', 'Upgrade batch #' || b.id::text, b.created_at
		FROM upgrade_batches b
		WHERE b.status = 'running'
		UNION ALL
		SELECT 'trust_registry', vta_name, updated_at
		FROM setup_sessions
		WHERE trust_registry_state = 'enabling'
		UNION ALL
		SELECT 'mobile_connection', s.vta_name, m.created_at
		FROM mobile_connections m
		JOIN setup_sessions s ON s.id = m.session_id
		WHERE m.status IN ('claimed', 'confirmed', 'provisioning')
		UNION ALL
		SELECT 'placement', 'Placement plan #' || id::text, created_at
		FROM placement_plans
		WHERE status = 'running'
		UNION ALL
		SELECT 'load_test', 'Load test #' || id::text, created_at
		FROM load_test_runs
		WHERE status IN ('creating', 'deleting')
		ORDER BY started_at`
	operations := make([]ActiveOperation, 0)
	if err := m.db.WithContext(ctx).Raw(query).Scan(&operations).Error; err != nil {
		return nil, err
	}
	return operations, nil
}
