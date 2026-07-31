-- A share code now identifies its stack on its own: the connect flow takes one
-- code and nothing else, and resolves it with
--
--     SELECT ... FROM setup_sessions WHERE share_code = ?
--
-- so the code has to be unique or that lookup is ambiguous. At 75 bits a
-- collision is not a practical concern; the index is here so it is not a
-- concern at all, and so the ambiguity is impossible rather than merely
-- unlikely.
--
-- Partial, because NULL means "not shared" and any number of stacks may be in
-- that state.
--
-- This replaces the JSON bundle the first cut used. That carried the stack
-- name, the farm, and the three DID/URL values a session is built from — but
-- those values were only ever compared, never used (the row they came from is
-- authoritative), and the confirmation the recipient sees was always rendered
-- from the server's own answer rather than from the pasted text. So everything
-- except the code was doing no work that survived contact with the design, and
-- a single code is what a person can actually read down a phone.
CREATE UNIQUE INDEX setup_sessions_share_code_unique
    ON setup_sessions (share_code)
    WHERE share_code IS NOT NULL;
