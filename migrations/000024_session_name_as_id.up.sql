-- vta_name becomes the session's public identifier: the :id in /setup/<name>,
-- and the word typed to confirm a delete. The opaque 8-char unique_id minted
-- per session goes with it — an agent called "myvta" was reachable only as
-- "ex9re34d", which nobody can recognise in a URL or retype from memory.
--
-- That promotion is what forces the index below to be global. It was partial on
-- domain_type = 'managed' because vta_name was only a hostname there; on a
-- fixed-label domain it reaches no hostname and survives as a free-form label,
-- so duplicates across users were harmless. An identifier has no such licence:
-- the admin routes look a session up by name ALONE, with no user_id to scope
-- the query, so two rows sharing a name would make those routes ambiguous.
--
-- The cost is real and worth stating: a label on a custom domain now lives in a
-- global namespace, so two users cannot both call their session "main".
DROP INDEX setup_sessions_vta_name_unique;
CREATE UNIQUE INDEX setup_sessions_vta_name_unique ON setup_sessions (vta_name);

-- setup_sessions_did_path_unique (did_hosting_server_url, vta_name) is now
-- implied by the above — a globally unique name cannot repeat under any host.
-- It is kept deliberately. It states the narrower, permanent rule: DID paths
-- collide per daemon, which is true regardless of how sessions are addressed.
-- If the identifier ever stops being vta_name, that rule must not leave with it.

-- Dropping the column drops setup_sessions_unique_id_unique with it.
ALTER TABLE setup_sessions DROP COLUMN unique_id;
