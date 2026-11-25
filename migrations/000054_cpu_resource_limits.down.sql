ALTER TABLE resource_defaults
    DROP COLUMN cpu_limit,
    DROP COLUMN cpu_request;

ALTER TABLE workload_resources
    DROP COLUMN cpu_limit,
    DROP COLUMN cpu_request;
