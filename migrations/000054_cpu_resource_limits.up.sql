ALTER TABLE workload_resources
    ADD COLUMN cpu_request TEXT NOT NULL DEFAULT '',
    ADD COLUMN cpu_limit TEXT NOT NULL DEFAULT '';

-- Preserve the resources actually deployed for existing sessions. Their CPU
-- limits stay empty until an administrator explicitly applies the new
-- defaults, so desired and observed state remain aligned after migration.
UPDATE workload_resources SET cpu_request = CASE component
    WHEN 'mediator' THEN '50m'
    ELSE '10m'
END;

ALTER TABLE resource_defaults
    ADD COLUMN cpu_request TEXT NOT NULL DEFAULT '',
    ADD COLUMN cpu_limit TEXT NOT NULL DEFAULT '';

UPDATE resource_defaults SET
    cpu_request = CASE component
        WHEN 'mediator' THEN '50m'
        ELSE '10m'
    END,
    cpu_limit = CASE component
        WHEN 'mediator' THEN '250m'
        ELSE '100m'
    END;

ALTER TABLE workload_resources ALTER COLUMN cpu_request DROP DEFAULT;
ALTER TABLE workload_resources ALTER COLUMN cpu_limit DROP DEFAULT;
ALTER TABLE resource_defaults ALTER COLUMN cpu_request DROP DEFAULT;
ALTER TABLE resource_defaults ALTER COLUMN cpu_limit DROP DEFAULT;
