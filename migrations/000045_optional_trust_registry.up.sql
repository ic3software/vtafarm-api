ALTER TABLE setup_sessions
    ADD COLUMN trust_registry_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN trust_registry_image text NOT NULL DEFAULT '',
    ADD COLUMN trust_registry_did text NOT NULL DEFAULT '',
    ADD COLUMN trust_registry_state text NOT NULL DEFAULT 'disabled',
    ADD COLUMN trust_registry_phase text NOT NULL DEFAULT '',
    ADD COLUMN trust_registry_error text NOT NULL DEFAULT '',
    ADD CONSTRAINT setup_sessions_trust_registry_mode CHECK
        (NOT trust_registry_enabled OR mode = 'full_stack'),
    ADD CONSTRAINT setup_sessions_trust_registry_state CHECK
        (trust_registry_state IN ('disabled', 'enabling', 'active', 'failed'));

ALTER TABLE resource_defaults DROP CONSTRAINT resource_defaults_component_check;
ALTER TABLE resource_defaults ADD CONSTRAINT resource_defaults_component_check
    CHECK (component IN ('vta', 'mediator', 'dids', 'vtc', 'trust-registry'));
ALTER TABLE workload_resources DROP CONSTRAINT workload_resources_component_check;
ALTER TABLE workload_resources ADD CONSTRAINT workload_resources_component_check
    CHECK (component IN ('vta', 'mediator', 'dids', 'vtc', 'trust-registry'));
