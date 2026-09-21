-- The handler checks for an in-flight grant before inserting, but two API
-- replicas can both observe zero and then insert different DIDs. One pending
-- row per session is the cross-replica lock that makes the check atomic.
CREATE UNIQUE INDEX vta_admin_grants_one_pending_per_session
    ON vta_admin_grants (session_id)
    WHERE status = 'pending';
