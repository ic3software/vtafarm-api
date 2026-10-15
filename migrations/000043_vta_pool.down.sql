DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pool_sessions) OR EXISTS (SELECT 1 FROM pool_batches) THEN
        RAISE EXCEPTION 'cannot roll back while pool records exist';
    END IF;
END $$;
DROP TABLE pool_sessions;
DROP TABLE pool_batches;
