CREATE TABLE users (
    id            BIGSERIAL    PRIMARY KEY,
    unique_id     VARCHAR(12)  NOT NULL DEFAULT '',
    email         VARCHAR(255) NOT NULL,
    password      VARCHAR(255) NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_users_email     ON users(email);
CREATE UNIQUE INDEX idx_users_unique_id ON users(unique_id);
