CREATE TABLE signup_requests (
    id             BIGSERIAL    PRIMARY KEY,
    email          VARCHAR(254) NOT NULL,
    status         VARCHAR(16)  NOT NULL DEFAULT 'pending',
    admin_id       BIGINT       REFERENCES admins(id) ON DELETE SET NULL,
    invitation_id  BIGINT       REFERENCES invitation_links(id) ON DELETE SET NULL,
    email_sent_at  TIMESTAMPTZ,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_signup_requests_email ON signup_requests(email);
