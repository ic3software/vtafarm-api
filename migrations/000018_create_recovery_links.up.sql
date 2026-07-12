-- Admin-issued, single-use, short-lived login links that restore access to an
-- existing user account after a lost passkey. Consuming one revokes every
-- passkey on the account and logs the holder in to register a fresh one.
CREATE TABLE recovery_links (
    id            BIGSERIAL    PRIMARY KEY,
    token         VARCHAR(64)  NOT NULL UNIQUE,
    user_id       BIGINT       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    admin_id      BIGINT       NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    expires_at    TIMESTAMPTZ  NOT NULL,
    used_at       TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_recovery_links_user_id ON recovery_links(user_id);
