-- vtc_name is only meaningful for full_stack_with_vtc; rows from the other
-- modes were historically stamped with the column default 'personal-vtc'.
-- Correct the default to '' (000012 is fixed in place for fresh databases;
-- this fixes databases that already ran it) and blank the stale values.
ALTER TABLE setup_sessions ALTER COLUMN vtc_name SET DEFAULT '';

UPDATE setup_sessions SET vtc_name = '' WHERE mode <> 'full_stack_with_vtc';
