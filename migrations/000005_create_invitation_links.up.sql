CREATE TABLE invitation_links (
    id            SERIAL PRIMARY KEY,
    token         VARCHAR(64)  NOT NULL UNIQUE,
    admin_id      INTEGER      NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    expires_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW() + INTERVAL '24 hours',
    used_at       TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_invitation_links_token ON invitation_links(token);
