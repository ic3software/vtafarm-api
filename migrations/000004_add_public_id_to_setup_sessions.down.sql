ALTER TABLE setup_sessions DROP CONSTRAINT IF EXISTS setup_sessions_public_id_unique;
ALTER TABLE setup_sessions DROP COLUMN IF EXISTS public_id;
