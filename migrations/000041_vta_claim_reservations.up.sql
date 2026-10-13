ALTER TABLE mobile_connections
    ADD COLUMN claim_handle varchar(22),
    ADD COLUMN claimant_digest char(64),
    ADD COLUMN confirm_by timestamptz,
    ADD COLUMN claimed_at timestamptz,
    ADD COLUMN confirmed_at timestamptz,
    ADD COLUMN released_at timestamptz,
    ADD COLUMN redeem_poll_id text,
    ADD COLUMN redeem_poll_until timestamptz;

ALTER TABLE mobile_connections
    DROP CONSTRAINT mobile_connections_status_check,
    ADD CONSTRAINT mobile_connections_status_check CHECK (status IN (
        'pending', 'accepted', 'cancelled', 'expired', 'failed',
        'open', 'claimed', 'confirmed', 'provisioning', 'released',
        'rejected', 'lapsed'
    )),
    ADD CONSTRAINT mobile_connections_claim_handle_check CHECK (
        claim_handle IS NULL OR claim_handle ~ '^[A-Za-z0-9_-]{22}$'
    ),
    ADD CONSTRAINT mobile_connections_claimant_digest_check CHECK (
        claimant_digest IS NULL OR claimant_digest ~ '^[0-9a-f]{64}$'
    ),
    ADD CONSTRAINT mobile_connections_claim_timestamps_check CHECK (
        (status <> 'claimed' OR (claimed_at IS NOT NULL AND confirm_by IS NOT NULL))
        AND (status <> 'confirmed' OR confirmed_at IS NOT NULL)
        AND (status <> 'released' OR released_at IS NOT NULL)
    ),
    ADD CONSTRAINT mobile_connections_redeem_poll_check CHECK (
        (redeem_poll_id IS NULL) = (redeem_poll_until IS NULL)
    );

CREATE UNIQUE INDEX mobile_connections_claim_handle_unique
    ON mobile_connections(claim_handle)
    WHERE claim_handle IS NOT NULL;

DROP INDEX mobile_connections_pending_session;
DROP INDEX mobile_connections_active_session;
CREATE UNIQUE INDEX mobile_connections_active_session
    ON mobile_connections(session_id)
    WHERE status IN ('pending', 'open', 'claimed', 'confirmed', 'provisioning')
       OR (status = 'accepted' AND connected_at IS NULL);

CREATE TABLE trust_task_executions (
    document_id text PRIMARY KEY,
    document_digest text NOT NULL,
    state text NOT NULL CHECK (state IN ('executing', 'completed')),
    response_bytes bytea,
    retain_until timestamptz NOT NULL,
    lease_expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT trust_task_executions_digest_check CHECK (
        document_digest ~ '^[0-9a-f]{64}$'
    ),
    CONSTRAINT trust_task_executions_response_check CHECK (
        state <> 'executing' OR response_bytes IS NULL
    )
);

CREATE INDEX trust_task_executions_retain_until
    ON trust_task_executions(retain_until);
