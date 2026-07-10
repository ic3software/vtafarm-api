-- Image upgrade batches: an admin picks a component + target image and a set
-- of sessions; a background runner works through the tasks with bounded
-- concurrency. These tables are the queue AND the audit trail — every state
-- change the runner makes is persisted here, so batches survive API restarts.

CREATE TABLE upgrade_batches (
    id           BIGSERIAL    PRIMARY KEY,
    admin_id     BIGINT       NOT NULL REFERENCES admins(id),
    component    VARCHAR(16)  NOT NULL,
    image        TEXT         NOT NULL,
    concurrency  INT          NOT NULL DEFAULT 3,
    -- running | paused | completed | cancelled
    status       VARCHAR(16)  NOT NULL DEFAULT 'running',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE upgrade_tasks (
    id           BIGSERIAL    PRIMARY KEY,
    batch_id     BIGINT       NOT NULL REFERENCES upgrade_batches(id) ON DELETE CASCADE,
    session_id   BIGINT       NOT NULL REFERENCES setup_sessions(id) ON DELETE CASCADE,
    -- image the session ran before the upgrade, kept for manual revert
    from_image   TEXT         NOT NULL DEFAULT '',
    -- pending | running | succeeded | failed | skipped
    status       VARCHAR(16)  NOT NULL DEFAULT 'pending',
    error_msg    TEXT         NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_upgrade_tasks_batch ON upgrade_tasks(batch_id);
