ALTER TABLE resource_defaults
    ADD COLUMN storage_size TEXT NOT NULL DEFAULT '200Mi';

-- Keep the operational defaults in the database even on installations where
-- no administrator has saved the Resource defaults form yet.
INSERT INTO resource_defaults (
    component, cpu_request, cpu_limit, memory_request, memory_limit, storage_size
) VALUES
    ('vta', '10m', '100m', '16Mi', '64Mi', '200Mi'),
    ('mediator', '50m', '250m', '128Mi', '256Mi', '200Mi'),
    ('dids', '10m', '100m', '64Mi', '128Mi', '200Mi'),
    ('vtc', '10m', '100m', '64Mi', '256Mi', '200Mi'),
    ('trust-registry', '10m', '100m', '64Mi', '256Mi', '200Mi')
ON CONFLICT (component) DO NOTHING;

ALTER TABLE resource_defaults ALTER COLUMN storage_size DROP DEFAULT;
