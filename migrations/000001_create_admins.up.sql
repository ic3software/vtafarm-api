CREATE TABLE admins (
    id            BIGSERIAL    PRIMARY KEY,
    email         VARCHAR(255) NOT NULL,
    username      VARCHAR(255) NOT NULL,
    password      VARCHAR(255) NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ
);

CREATE INDEX        IF NOT EXISTS idx_admins_deleted_at      ON admins(deleted_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_admins_email_active    ON admins(email)    WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_admins_username_active ON admins(username) WHERE deleted_at IS NULL;
