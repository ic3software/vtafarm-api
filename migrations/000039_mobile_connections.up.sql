CREATE TABLE mobile_connections (
    id uuid PRIMARY KEY,
    session_id bigint NOT NULL REFERENCES setup_sessions(id) ON DELETE CASCADE,
    vta_did text NOT NULL,
    operation text NOT NULL DEFAULT ''
        CHECK (operation IN ('', 'provision_vta', 'grant_acl')),
    admin_did text NOT NULL DEFAULT '',
    status text NOT NULL CHECK (status IN ('pending', 'accepted', 'cancelled', 'expired', 'failed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    connected_at timestamptz,
    provisioned_at timestamptz,
    provision_error text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX mobile_connections_pending_session ON mobile_connections(session_id) WHERE status = 'pending';
CREATE UNIQUE INDEX mobile_connections_active_session
    ON mobile_connections(session_id)
    WHERE status = 'accepted' AND connected_at IS NULL;
CREATE INDEX mobile_connections_session_created ON mobile_connections(session_id, created_at DESC);
