CREATE TABLE user_passkeys (
    id            BIGSERIAL    PRIMARY KEY,
    user_id       BIGINT       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id BYTEA        NOT NULL,
    credential    BYTEA        NOT NULL,
    name          TEXT         NOT NULL DEFAULT '',
    last_used_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_user_passkeys_credential_id ON user_passkeys(credential_id);
CREATE INDEX idx_user_passkeys_user_id ON user_passkeys(user_id);
