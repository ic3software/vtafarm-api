CREATE TABLE placement_plans (
    id          BIGSERIAL   PRIMARY KEY,
    admin_id    BIGINT      REFERENCES admins(id) ON DELETE SET NULL,
    kind        VARCHAR(16) NOT NULL CHECK (kind IN ('reconcile', 'relocate', 'provision')),
    status      VARCHAR(16) NOT NULL DEFAULT 'running'
                CHECK (status IN ('running', 'paused', 'completed', 'cancelled')),
    error_msg   TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE placement_tasks (
    id                  BIGSERIAL   PRIMARY KEY,
    plan_id             BIGINT      NOT NULL REFERENCES placement_plans(id) ON DELETE CASCADE,
    session_id          BIGINT      NOT NULL REFERENCES setup_sessions(id) ON DELETE CASCADE,
    component           VARCHAR(32) NOT NULL DEFAULT 'vta',
    source_node         TEXT        NOT NULL DEFAULT '',
    target_node         TEXT        NOT NULL DEFAULT '',
    state               VARCHAR(40) NOT NULL DEFAULT 'requested',
    error_msg           TEXT        NOT NULL DEFAULT '',
    p95_memory_bytes    BIGINT      NOT NULL DEFAULT 0,
    original_replicas   INTEGER     NOT NULL DEFAULT 1,
    deployment_snapshot JSONB       NOT NULL DEFAULT '{}'::jsonb,
    namespace           TEXT        NOT NULL DEFAULT '',
    deployment_name     TEXT        NOT NULL DEFAULT '',
    deployment_uid      TEXT        NOT NULL DEFAULT '',
    pvc_name            TEXT        NOT NULL DEFAULT '',
    pvc_uid             TEXT        NOT NULL DEFAULT '',
    pv_name             TEXT        NOT NULL DEFAULT '',
    pv_uid              TEXT        NOT NULL DEFAULT '',
    csi_volume_handle   TEXT        NOT NULL DEFAULT '',
    longhorn_volume     TEXT        NOT NULL DEFAULT '',
    last_backup_name    TEXT        NOT NULL DEFAULT '',
    last_backup_at      TEXT        NOT NULL DEFAULT '',
    pod_uid             TEXT        NOT NULL DEFAULT '',
    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (plan_id, session_id, component)
);

CREATE INDEX placement_tasks_plan_state ON placement_tasks(plan_id, state);
CREATE INDEX placement_tasks_session ON placement_tasks(session_id);

CREATE TABLE placement_events (
    id          BIGSERIAL   PRIMARY KEY,
    plan_id     BIGINT      NOT NULL REFERENCES placement_plans(id) ON DELETE CASCADE,
    task_id     BIGINT      REFERENCES placement_tasks(id) ON DELETE CASCADE,
    state       VARCHAR(40) NOT NULL,
    message     TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX placement_events_plan_created ON placement_events(plan_id, created_at);

CREATE TABLE placement_reservations (
    id            BIGSERIAL   PRIMARY KEY,
    task_id       BIGINT      NOT NULL UNIQUE REFERENCES placement_tasks(id) ON DELETE CASCADE,
    node_name     TEXT        NOT NULL,
    memory_bytes  BIGINT      NOT NULL CHECK (memory_bytes > 0),
    pod_uid       TEXT        NOT NULL DEFAULT '',
    status        VARCHAR(16) NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'released')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    released_at   TIMESTAMPTZ
);

CREATE INDEX placement_reservations_active_node
    ON placement_reservations(node_name) WHERE status = 'active';

CREATE TABLE placement_maintenance_locks (
    session_id   BIGINT      PRIMARY KEY REFERENCES setup_sessions(id) ON DELETE CASCADE,
    task_id      BIGINT      NOT NULL UNIQUE REFERENCES placement_tasks(id) ON DELETE CASCADE,
    operation    VARCHAR(32) NOT NULL,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE placement_node_states (
    node_name      TEXT        PRIMARY KEY,
    memory_state   VARCHAR(16) NOT NULL DEFAULT 'unknown',
    eligible       BOOLEAN     NOT NULL DEFAULT FALSE,
    hot_since      TIMESTAMPTZ,
    recovery_since TIMESTAMPTZ,
    observed_at    TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
