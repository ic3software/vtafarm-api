CREATE TABLE users (
    id            BIGSERIAL    PRIMARY KEY,
    unique_id     VARCHAR(12)  NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_users_unique_id ON users(unique_id);
