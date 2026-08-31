CREATE TABLE load_test_runs (
    id              BIGSERIAL   PRIMARY KEY,
    user_id         BIGINT      NULL REFERENCES users(id) ON DELETE SET NULL,
    requested_by    BIGINT      NULL REFERENCES admins(id) ON DELETE SET NULL,
    requested_count INTEGER     NOT NULL CHECK (requested_count BETWEEN 1 AND 50),
    created_count   INTEGER     NOT NULL DEFAULT 0 CHECK (created_count >= 0),
    vta_image       TEXT        NOT NULL,
    status          TEXT        NOT NULL
                    CHECK (status IN (
                        'creating', 'active', 'partial', 'failed',
                        'deleting', 'deleted', 'delete_failed'
                    )),
    error_msg       TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- There is one operator-controlled load test at a time. The handler performs
-- a readable preflight; this index closes the concurrent-request race across
-- API replicas.
CREATE UNIQUE INDEX load_test_runs_one_active
    ON load_test_runs ((TRUE))
    WHERE status IN ('creating', 'active', 'partial', 'deleting', 'delete_failed');

ALTER TABLE setup_sessions
    ADD COLUMN load_test_run_id BIGINT NULL
        REFERENCES load_test_runs(id) ON DELETE SET NULL;

CREATE INDEX setup_sessions_load_test_run_idx
    ON setup_sessions (load_test_run_id)
    WHERE load_test_run_id IS NOT NULL;
