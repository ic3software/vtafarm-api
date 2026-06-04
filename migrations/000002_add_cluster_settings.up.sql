CREATE TABLE cluster_settings (
    id         SERIAL PRIMARY KEY,
    name       VARCHAR(100) NOT NULL,
    ingress_ip VARCHAR(45)  NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
