CREATE TABLE admins (
    id            BIGSERIAL    PRIMARY KEY,
    unique_id     VARCHAR(12)  NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_admins_unique_id ON admins(unique_id);
