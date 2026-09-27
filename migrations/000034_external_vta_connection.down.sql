ALTER TABLE setup_sessions DROP COLUMN vta_did_log;

ALTER TABLE setup_sessions DROP CONSTRAINT setup_sessions_connection_source_check;

ALTER TABLE setup_sessions ADD CONSTRAINT setup_sessions_connection_source_check
    CHECK (connection_source IN ('platform', 'in_farm'));
