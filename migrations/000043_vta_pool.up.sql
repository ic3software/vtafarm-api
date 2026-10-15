CREATE TABLE pool_batches (
    id bigserial PRIMARY KEY,
    created_by bigint NOT NULL REFERENCES admins(id) ON DELETE RESTRICT,
    label text NOT NULL,
    requested_count integer NOT NULL CHECK (requested_count > 0),
    idempotency_key text NOT NULL,
    request_digest text NOT NULL,
    cleanup_requested_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (created_by, idempotency_key)
);

CREATE TABLE pool_sessions (
    session_id bigint PRIMARY KEY REFERENCES setup_sessions(id) ON DELETE RESTRICT,
    batch_id bigint NOT NULL REFERENCES pool_batches(id) ON DELETE RESTRICT
);

CREATE INDEX pool_sessions_batch_id ON pool_sessions(batch_id);
