-- Rolling back recreates the schema only. Rows removed by the up migration
-- cannot be recovered.
CREATE TABLE vta_admin_grants (
    id           BIGSERIAL   PRIMARY KEY,
    session_id   BIGINT      NOT NULL REFERENCES setup_sessions(id) ON DELETE CASCADE,
    did          TEXT        NOT NULL,
    label        TEXT        NOT NULL DEFAULT '',
    status       TEXT        NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'granted', 'failed')),
    error_msg    TEXT        NOT NULL DEFAULT '',
    requested_by BIGINT      NULL REFERENCES admins(id) ON DELETE SET NULL,
    granted_at   TIMESTAMPTZ NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX vta_admin_grants_live_unique
    ON vta_admin_grants (session_id, did)
    WHERE status IN ('pending', 'granted');

CREATE INDEX vta_admin_grants_session_idx ON vta_admin_grants (session_id);

CREATE UNIQUE INDEX vta_admin_grants_one_pending_per_session
    ON vta_admin_grants (session_id)
    WHERE status = 'pending';
