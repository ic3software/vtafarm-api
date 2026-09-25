DROP INDEX IF EXISTS setup_sessions_share_code_unique;

ALTER TABLE setup_sessions DROP COLUMN IF EXISTS share_code;
