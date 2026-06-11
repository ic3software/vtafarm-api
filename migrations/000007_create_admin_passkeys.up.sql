CREATE TABLE admin_passkeys (
    id            BIGSERIAL    PRIMARY KEY,
    admin_id      BIGINT       NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    credential_id BYTEA        NOT NULL,
    credential    BYTEA        NOT NULL,
    name          TEXT         NOT NULL DEFAULT '',
    last_used_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_admin_passkeys_credential_id ON admin_passkeys(credential_id);
CREATE INDEX idx_admin_passkeys_admin_id ON admin_passkeys(admin_id);
