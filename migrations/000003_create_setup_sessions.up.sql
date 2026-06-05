CREATE TABLE setup_sessions (
    id            SERIAL PRIMARY KEY,
    user_id       INTEGER NOT NULL,
    status        VARCHAR(50)  NOT NULL DEFAULT 'pending',
    mode          VARCHAR(20)  NOT NULL,
    domain        VARCHAR(255) NOT NULL,
    subdomain     VARCHAR(100) NOT NULL,
    cf_record_id  VARCHAR(100),
    did_log       TEXT         NOT NULL DEFAULT '',
    error_msg     TEXT         NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ
);

CREATE INDEX idx_setup_sessions_user_id   ON setup_sessions(user_id);
CREATE INDEX idx_setup_sessions_subdomain ON setup_sessions(subdomain);
