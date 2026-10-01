CREATE TABLE initial_provisions (
    session_id bigint PRIMARY KEY REFERENCES setup_sessions(id) ON DELETE CASCADE,
    admin_did text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);

CREATE TABLE mobile_connections (
    id uuid PRIMARY KEY,
    session_id bigint NOT NULL REFERENCES setup_sessions(id) ON DELETE CASCADE,
    vta_did text NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'accepted', 'cancelled', 'expired')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    connected_at timestamptz
);
CREATE UNIQUE INDEX mobile_connections_pending_session ON mobile_connections(session_id) WHERE status = 'pending';
CREATE UNIQUE INDEX mobile_connections_accepted_session ON mobile_connections(session_id) WHERE status = 'accepted';
CREATE INDEX mobile_connections_session_created ON mobile_connections(session_id, created_at DESC);
