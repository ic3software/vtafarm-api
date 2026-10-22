DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM setup_sessions WHERE trust_registry_enabled) THEN
        RAISE EXCEPTION 'Trust Registry sessions still exist; refusing to discard their inventory';
    END IF;
END $$;
ALTER TABLE setup_sessions
    DROP CONSTRAINT setup_sessions_trust_registry_mode,
    DROP CONSTRAINT setup_sessions_trust_registry_state,
    DROP COLUMN trust_registry_enabled,
    DROP COLUMN trust_registry_image,
    DROP COLUMN trust_registry_did,
    DROP COLUMN trust_registry_state,
    DROP COLUMN trust_registry_phase,
    DROP COLUMN trust_registry_error;

DELETE FROM resource_defaults WHERE component = 'trust-registry';
DELETE FROM workload_resources WHERE component = 'trust-registry';
ALTER TABLE resource_defaults DROP CONSTRAINT resource_defaults_component_check;
ALTER TABLE resource_defaults ADD CONSTRAINT resource_defaults_component_check
    CHECK (component IN ('vta', 'mediator', 'dids', 'vtc'));
ALTER TABLE workload_resources DROP CONSTRAINT workload_resources_component_check;
ALTER TABLE workload_resources ADD CONSTRAINT workload_resources_component_check
    CHECK (component IN ('vta', 'mediator', 'dids', 'vtc'));
