CREATE TABLE user_siop_identities (
    id                    BIGSERIAL PRIMARY KEY,
    user_id               BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    did                   TEXT NOT NULL,
    label                 TEXT NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_authenticated_at TIMESTAMPTZ NULL,
    last_kid              TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX user_siop_identities_did_unique
    ON user_siop_identities (did);
CREATE INDEX user_siop_identities_user_id_idx
    ON user_siop_identities (user_id);

CREATE TABLE admin_siop_identities (
    id                    BIGSERIAL PRIMARY KEY,
    admin_id              BIGINT NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    did                   TEXT NOT NULL,
    label                 TEXT NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_authenticated_at TIMESTAMPTZ NULL,
    last_kid              TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX admin_siop_identities_did_unique
    ON admin_siop_identities (did);
CREATE INDEX admin_siop_identities_admin_id_idx
    ON admin_siop_identities (admin_id);

CREATE TABLE siop_challenges (
    id           TEXT PRIMARY KEY,
    purpose      TEXT NOT NULL CHECK (purpose IN ('login', 'link')),
    account_role TEXT NOT NULL CHECK (account_role IN ('user', 'admin')),
    expected_did TEXT NOT NULL,
    user_id      BIGINT NULL REFERENCES users(id) ON DELETE CASCADE,
    admin_id     BIGINT NULL REFERENCES admins(id) ON DELETE CASCADE,
    nonce_sha256 BYTEA NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT siop_challenge_account_matches_purpose CHECK (
        (purpose = 'login' AND user_id IS NULL AND admin_id IS NULL)
        OR (purpose = 'link' AND (
            (account_role = 'user' AND user_id IS NOT NULL AND admin_id IS NULL)
            OR (account_role = 'admin' AND admin_id IS NOT NULL AND user_id IS NULL)
        ))
    )
);

CREATE INDEX siop_challenges_expires_at_idx ON siop_challenges (expires_at);
CREATE INDEX siop_challenges_pending_did_idx
    ON siop_challenges (account_role, expected_did, expires_at);
