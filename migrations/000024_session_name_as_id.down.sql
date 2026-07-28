-- Restores unique_id, regenerating values for existing rows the same way
-- 000004 first did — the originals are gone, and nothing outside this table
-- referenced them.
ALTER TABLE setup_sessions
    ADD COLUMN unique_id VARCHAR(8) NOT NULL DEFAULT '';

UPDATE setup_sessions
    SET unique_id = (
        SELECT string_agg(substr('abcdefghijklmnopqrstuvwxyz0123456789', floor(random() * 36 + 1)::int, 1), '')
        FROM generate_series(1, 8)
        WHERE setup_sessions.id = setup_sessions.id
    );

ALTER TABLE setup_sessions
    ADD CONSTRAINT setup_sessions_unique_id_unique UNIQUE (unique_id);

DROP INDEX setup_sessions_vta_name_unique;
CREATE UNIQUE INDEX setup_sessions_vta_name_unique
    ON setup_sessions (vta_name) WHERE domain_type = 'managed';
