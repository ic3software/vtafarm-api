ALTER TABLE setup_sessions
    ADD COLUMN public_id VARCHAR(8) NOT NULL DEFAULT '';

UPDATE setup_sessions
    SET public_id = (
        SELECT string_agg(substr('abcdefghijklmnopqrstuvwxyz0123456789', floor(random() * 36 + 1)::int, 1), '')
        FROM generate_series(1, 8)
        WHERE setup_sessions.id = setup_sessions.id
    );

ALTER TABLE setup_sessions
    ADD CONSTRAINT setup_sessions_public_id_unique UNIQUE (public_id);
