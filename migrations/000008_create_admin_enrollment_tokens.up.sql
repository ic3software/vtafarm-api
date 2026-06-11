CREATE TABLE admin_enrollment_tokens (
    id            BIGSERIAL    PRIMARY KEY,
    token         VARCHAR(64)  NOT NULL UNIQUE,
    expires_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW() + INTERVAL '24 hours',
    used_at       TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_admin_enrollment_tokens_token ON admin_enrollment_tokens(token);
