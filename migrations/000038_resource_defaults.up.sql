CREATE TABLE resource_defaults (
    component TEXT PRIMARY KEY CHECK (component IN ('vta', 'mediator', 'dids', 'vtc')),
    memory_request TEXT NOT NULL,
    memory_limit TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
