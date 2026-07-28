DROP INDEX IF EXISTS setup_sessions_did_path_unique;

ALTER TABLE setup_sessions
    DROP COLUMN IF EXISTS did_hosting_control_url,
    DROP COLUMN IF EXISTS did_hosting_server_url;
