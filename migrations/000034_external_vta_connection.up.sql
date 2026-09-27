ALTER TABLE setup_sessions DROP CONSTRAINT setup_sessions_connection_source_check;

ALTER TABLE setup_sessions ADD CONSTRAINT setup_sessions_connection_source_check
    CHECK (connection_source IN ('platform', 'in_farm', 'external'));

ALTER TABLE setup_sessions ADD COLUMN vta_did_log TEXT NOT NULL DEFAULT '';
