CREATE TABLE incidents (
    id BIGSERIAL PRIMARY KEY,
    public_id TEXT NOT NULL UNIQUE,
    setup_session_id BIGINT NOT NULL REFERENCES setup_sessions(id) ON DELETE CASCADE,
    cluster TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    alert_name TEXT NOT NULL,
    incident_type TEXT NOT NULL,
    component TEXT NOT NULL,
    resource TEXT NOT NULL DEFAULT '',
    severity TEXT NOT NULL CHECK (severity IN ('warning', 'critical')),
    status TEXT NOT NULL CHECK (status IN ('firing', 'resolved')),
    title TEXT NOT NULL,
    message TEXT NOT NULL,
    usage_percent DOUBLE PRECISION,
    started_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    last_seen_at TIMESTAMPTZ NOT NULL,
    read_at TIMESTAMPTZ,
    suppressed BOOLEAN NOT NULL DEFAULT FALSE,
    labels JSONB NOT NULL DEFAULT '{}'::jsonb,
    annotations JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT incidents_alert_delivery_unique UNIQUE (cluster, fingerprint, started_at)
);

CREATE INDEX incidents_session_started_idx
    ON incidents (setup_session_id, started_at DESC);

CREATE INDEX incidents_active_idx
    ON incidents (status, severity, started_at DESC)
    WHERE suppressed = FALSE;
