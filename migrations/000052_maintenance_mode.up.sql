CREATE TABLE maintenance_mode (
    id                  SMALLINT    PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    enabled             BOOLEAN     NOT NULL DEFAULT FALSE,
    message             TEXT        NOT NULL DEFAULT '',
    enabled_at          TIMESTAMPTZ,
    enabled_by_admin_id BIGINT      REFERENCES admins(id) ON DELETE SET NULL,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO maintenance_mode (id) VALUES (1);

CREATE TABLE operation_locks (
    id             BIGSERIAL   PRIMARY KEY,
    operation_type VARCHAR(64) NOT NULL,
    target         TEXT        NOT NULL DEFAULT '',
    lock_key       BIGINT      NOT NULL,
    started_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX operation_locks_started_at ON operation_locks(started_at);

-- The admin card polls readiness while it is open. These partial indexes stay
-- small because they contain only work that can block a maintenance transition.
CREATE INDEX setup_sessions_maintenance_active
    ON setup_sessions(updated_at)
    WHERE status NOT IN ('running', 'failed', 'vta_setup_complete', 'awaiting_admin_did', 'awaiting_did_publication', 'pool_prepared', 'pool_waiting');
CREATE INDEX setup_sessions_maintenance_registry
    ON setup_sessions(updated_at) WHERE trust_registry_state = 'enabling';
CREATE INDEX upgrade_batches_maintenance_active
    ON upgrade_batches(created_at) WHERE status = 'running';
CREATE INDEX mobile_connections_maintenance_active
    ON mobile_connections(created_at) WHERE status IN ('claimed', 'confirmed', 'provisioning');
CREATE INDEX placement_plans_maintenance_active
    ON placement_plans(created_at) WHERE status = 'running';
CREATE INDEX load_test_runs_maintenance_active
    ON load_test_runs(created_at) WHERE status IN ('creating', 'deleting');
