CREATE TABLE wallet_login_bindings (
    id bigserial PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    login_did text NOT NULL,
    source_session_id bigint NOT NULL REFERENCES setup_sessions(id) ON DELETE CASCADE,
    source_claim_id uuid NOT NULL REFERENCES mobile_connections(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    CONSTRAINT wallet_login_bindings_login_did_check CHECK (
        login_did ~ '^did:key:z6Mk[1-9A-HJ-NP-Za-km-z]+$'
    ),
    UNIQUE (user_id, login_did),
    UNIQUE (source_claim_id)
);

CREATE INDEX wallet_login_bindings_login_did_active
    ON wallet_login_bindings(login_did)
    WHERE revoked_at IS NULL;

CREATE TABLE wallet_login_requests (
    request_id varchar(43) PRIMARY KEY,
    state text NOT NULL CHECK (state IN (
        'pending', 'claimed', 'identified', 'approved', 'declined',
        'cancelled', 'expired', 'consumed'
    )),
    starter_did text NOT NULL,
    starter_network_digest bytea,
    approver_did text,
    origin text NOT NULL,
    match_number char(2),
    match_number_delivered boolean NOT NULL DEFAULT false,
    requester_browser text NOT NULL DEFAULT 'unknown',
    requester_os text NOT NULL DEFAULT 'unknown',
    identified_did text,
    binding_id bigint REFERENCES wallet_login_bindings(id) ON DELETE SET NULL,
    user_id bigint REFERENCES users(id) ON DELETE CASCADE,
    step1 jsonb,
    step2_digest text,
    grant_document jsonb,
    session_ends_at timestamptz,
    wallet_session_id uuid,
    redeem_poll_id text,
    redeem_poll_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    claim_closes_at timestamptz NOT NULL,
    decision_closes_at timestamptz,
    ended_at timestamptz,
    CONSTRAINT wallet_login_requests_match_number_check CHECK (
        match_number IS NULL OR match_number ~ '^[0-9]{2}$'
    ),
    CONSTRAINT wallet_login_requests_poll_check CHECK (
        (redeem_poll_id IS NULL) = (redeem_poll_until IS NULL)
    )
);

CREATE INDEX wallet_login_requests_expiry
    ON wallet_login_requests(claim_closes_at, decision_closes_at)
    WHERE state IN ('pending', 'claimed', 'identified', 'approved');

CREATE TABLE wallet_login_sessions (
    id uuid PRIMARY KEY,
    binding_id bigint NOT NULL REFERENCES wallet_login_bindings(id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);

ALTER TABLE wallet_login_requests
    ADD CONSTRAINT wallet_login_requests_wallet_session_fk
    FOREIGN KEY (wallet_session_id) REFERENCES wallet_login_sessions(id) ON DELETE SET NULL;

CREATE INDEX wallet_login_sessions_active
    ON wallet_login_sessions(id, expires_at)
    WHERE revoked_at IS NULL;
