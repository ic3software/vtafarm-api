-- The vtc_name partial unique index was gated on mode = 'full_stack_with_vtc'
-- (000013). That mode is gone — full_stack now always provisions a VTC — so
-- the old predicate matches nothing and the index silently stops enforcing
-- anything, leaving only the handler's racy check-then-insert.
--
-- Rows first, then the index: any surviving full_stack_with_vtc row must carry
-- the new mode string before the new predicate can see it.
UPDATE setup_sessions SET mode = 'full_stack' WHERE mode = 'full_stack_with_vtc';

DROP INDEX setup_sessions_vtc_name_unique;

-- Keyed on the value itself rather than on a mode string: "a session that has
-- a vtc_name must own it exclusively" is the actual invariant, and it survives
-- the next mode rename. vta_only rows carry the column default '' (000014) and
-- stay unindexed, which is what a full-table unique index could not allow.
CREATE UNIQUE INDEX setup_sessions_vtc_name_unique ON setup_sessions (vtc_name)
    WHERE vtc_name <> '';
