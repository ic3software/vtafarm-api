CREATE TABLE IF NOT EXISTS pod_deployments (
    id           SERIAL       PRIMARY KEY,
    user_id      BIGINT       NOT NULL REFERENCES users(id),
    name         VARCHAR(255) NOT NULL,
    namespace    VARCHAR(255) NOT NULL,
    yaml_content TEXT         NOT NULL,
    status       VARCHAR(50)  NOT NULL DEFAULT 'pending',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_pod_deployments_user_id    ON pod_deployments(user_id);
CREATE INDEX IF NOT EXISTS idx_pod_deployments_deleted_at ON pod_deployments(deleted_at);
