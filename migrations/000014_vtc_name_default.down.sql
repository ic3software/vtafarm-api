ALTER TABLE setup_sessions ALTER COLUMN vtc_name SET DEFAULT 'personal-vtc';

UPDATE setup_sessions SET vtc_name = 'personal-vtc' WHERE mode <> 'full_stack_with_vtc';
