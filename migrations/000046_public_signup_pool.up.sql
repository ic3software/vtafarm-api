ALTER TABLE pool_batches
    ADD COLUMN purpose text NOT NULL DEFAULT 'admin_station',
    ADD COLUMN public_signup_enabled_at timestamptz,
    ADD CONSTRAINT pool_batches_purpose_check
        CHECK (purpose IN ('admin_station', 'public_signup')),
    ADD CONSTRAINT pool_batches_public_signup_enabled_check
        CHECK (public_signup_enabled_at IS NULL OR purpose = 'public_signup');

CREATE INDEX pool_batches_public_signup_enabled_idx
    ON pool_batches(public_signup_enabled_at, id)
    WHERE purpose = 'public_signup' AND public_signup_enabled_at IS NOT NULL;

ALTER TABLE mobile_connections
    ADD COLUMN public_access_digest bytea,
    ADD CONSTRAINT mobile_connections_public_access_digest_check
        CHECK (public_access_digest IS NULL OR octet_length(public_access_digest) = 32);

CREATE UNIQUE INDEX mobile_connections_public_access_digest_unique
    ON mobile_connections(public_access_digest)
    WHERE public_access_digest IS NOT NULL;

CREATE TABLE pool_batch_additions (
    id bigserial PRIMARY KEY,
    batch_id bigint NOT NULL REFERENCES pool_batches(id) ON DELETE CASCADE,
    created_by bigint NOT NULL REFERENCES admins(id) ON DELETE RESTRICT,
    requested_count integer NOT NULL CHECK (requested_count > 0),
    idempotency_key text NOT NULL,
    request_digest text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (created_by, idempotency_key)
);

CREATE INDEX pool_batch_additions_batch_id ON pool_batch_additions(batch_id);
