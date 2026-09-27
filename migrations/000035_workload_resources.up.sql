CREATE TABLE workload_resources (
    id               BIGSERIAL   PRIMARY KEY,
    setup_session_id BIGINT      NOT NULL
                     REFERENCES setup_sessions(id) ON DELETE CASCADE,
    component        TEXT        NOT NULL
                     CHECK (component IN ('vta', 'mediator', 'dids', 'vtc')),
    memory_request   TEXT        NOT NULL,
    memory_limit     TEXT        NOT NULL,
    apply_error      TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (setup_session_id, component)
);

-- Sessions that predate configurable resources were created with the former
-- fixed profiles. Preserve those values as their desired state so the Admin UI
-- can distinguish them from the safer defaults used for new workloads.
INSERT INTO workload_resources (setup_session_id, component, memory_request, memory_limit)
SELECT id, 'vta', '32Mi', '64Mi' FROM setup_sessions;

INSERT INTO workload_resources (setup_session_id, component, memory_request, memory_limit)
SELECT id, 'mediator', '128Mi', '256Mi' FROM setup_sessions WHERE mode = 'full_stack';

INSERT INTO workload_resources (setup_session_id, component, memory_request, memory_limit)
SELECT id, 'dids', '64Mi', '128Mi' FROM setup_sessions WHERE mode = 'full_stack';

INSERT INTO workload_resources (setup_session_id, component, memory_request, memory_limit)
SELECT id, 'vtc', '32Mi', '64Mi' FROM setup_sessions WHERE mode = 'full_stack';
