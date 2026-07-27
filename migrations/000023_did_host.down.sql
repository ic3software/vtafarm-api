DROP INDEX IF EXISTS setup_sessions_did_path_unique;

ALTER TABLE setup_sessions DROP COLUMN IF EXISTS did_host;
