ALTER TABLE setup_sessions DROP CONSTRAINT IF EXISTS setup_sessions_unique_id_unique;
ALTER TABLE setup_sessions DROP COLUMN IF EXISTS unique_id;
