ALTER TABLE setup_sessions ADD COLUMN share_code TEXT NULL;

CREATE UNIQUE INDEX setup_sessions_share_code_unique
    ON setup_sessions (share_code)
    WHERE share_code IS NOT NULL;
