DROP INDEX IF EXISTS setup_sessions_load_test_run_idx;
ALTER TABLE setup_sessions DROP COLUMN IF EXISTS load_test_run_id;
DROP INDEX IF EXISTS load_test_runs_one_active;
DROP TABLE IF EXISTS load_test_runs;
