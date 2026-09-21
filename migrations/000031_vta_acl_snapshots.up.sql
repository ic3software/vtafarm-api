CREATE TABLE vta_acl_snapshots (
    session_id              BIGINT      PRIMARY KEY REFERENCES setup_sessions(id) ON DELETE CASCADE,
    synced_at               TIMESTAMPTZ NULL,
    entry_count             INTEGER     NOT NULL DEFAULT 0,
    maintenance_started_at  TIMESTAMPTZ NULL
);

CREATE TABLE vta_acl_entries (
    session_id      BIGINT NOT NULL REFERENCES vta_acl_snapshots(session_id) ON DELETE CASCADE,
    did             TEXT   NOT NULL,
    role            TEXT   NOT NULL,
    label           TEXT   NOT NULL DEFAULT '',
    contexts        TEXT   NOT NULL DEFAULT '',
    acl_created_at  TEXT   NOT NULL DEFAULT '',
    PRIMARY KEY (session_id, did)
);
