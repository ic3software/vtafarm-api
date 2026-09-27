ALTER TABLE setup_sessions
    ADD COLUMN failed_stage TEXT NOT NULL DEFAULT '';
