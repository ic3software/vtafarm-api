CREATE TABLE setup_sessions (
    id                  SERIAL       PRIMARY KEY,
    user_id             INTEGER      NOT NULL,
    status              VARCHAR(50)  NOT NULL DEFAULT 'pending',
    mode                VARCHAR(20)  NOT NULL,
    domain              VARCHAR(255) NOT NULL,
    subdomain           VARCHAR(100) NOT NULL,
    cf_record_id        VARCHAR(100),
    error_msg           TEXT         NOT NULL DEFAULT '',
    -- VTA config inputs
    vta_name            VARCHAR(100) NOT NULL DEFAULT 'personal-vta',
    mediator_did        TEXT         NOT NULL DEFAULT '',
    vta_did_url         TEXT         NOT NULL DEFAULT '',
    portable            BOOLEAN      NOT NULL DEFAULT TRUE,
    pre_rotation_count  INTEGER      NOT NULL DEFAULT 1,
    vta_image           TEXT         NOT NULL DEFAULT '',
    -- Output populated after vta setup runs
    vta_did             TEXT         NOT NULL DEFAULT '',
    admin_did           TEXT         NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_setup_sessions_user_id   ON setup_sessions(user_id);
CREATE INDEX idx_setup_sessions_subdomain ON setup_sessions(subdomain);
