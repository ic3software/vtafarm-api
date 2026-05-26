CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL    PRIMARY KEY,
    email         VARCHAR(255) NOT NULL,
    username      VARCHAR(255) NOT NULL,
    password      VARCHAR(255) NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ
);

CREATE INDEX        IF NOT EXISTS idx_users_deleted_at      ON users(deleted_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email_active    ON users(email)    WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_username_active ON users(username) WHERE deleted_at IS NULL;
