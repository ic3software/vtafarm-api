DROP INDEX setup_sessions_maintenance_active;

CREATE INDEX setup_sessions_maintenance_active
    ON setup_sessions(updated_at)
    WHERE status NOT IN ('running', 'failed', 'awaiting_admin_did', 'awaiting_did_publication', 'pool_prepared', 'pool_waiting');
