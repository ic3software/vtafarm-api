DROP INDEX setup_sessions_vtc_name_unique;

CREATE UNIQUE INDEX setup_sessions_vtc_name_unique ON setup_sessions (vtc_name)
    WHERE mode = 'full_stack_with_vtc';

UPDATE setup_sessions SET mode = 'full_stack_with_vtc' WHERE mode = 'full_stack';
