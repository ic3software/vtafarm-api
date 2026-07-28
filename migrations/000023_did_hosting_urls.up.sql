-- Where a session's did:webvh identifiers are served, and where the daemon
-- serving them is administered. Two columns, not one, because the two are only
-- the same URL by accident of deployment: the daemon build we run today answers
-- both on one host, while a standalone DID-hosting service splits resolution
-- from its management API. Collapsing them now would have to be undone the
-- first time a user points a session at one.
--
-- They are recorded per session rather than read from configuration because a
-- did:webvh bakes its host into the identifier at mint time. The URL a session
-- was built against is a fact about that session, not a current setting — which
-- is also why teardown must delete a DID through the control URL the row
-- carries rather than whatever the platform stack answers on today.
ALTER TABLE setup_sessions
    ADD COLUMN did_hosting_server_url  TEXT NOT NULL DEFAULT '',
    ADD COLUMN did_hosting_control_url TEXT NOT NULL DEFAULT '';

-- Backfill. The server URL is recovered exactly: vta_only rows carry it inside
-- the vta_did_url they were built with, and a full_stack row's daemon is the
-- dids hostname it already stores.
UPDATE setup_sessions
   SET did_hosting_server_url = substring(vta_did_url from '^[a-z]+://[^/]+')
 WHERE mode = 'vta_only' AND vta_did_url <> '';

UPDATE setup_sessions
   SET did_hosting_server_url = 'https://' || dids_subdomain || '.' || domain
 WHERE mode = 'full_stack' AND dids_subdomain <> '';

-- The control URL is not recoverable from any row — it was a configuration
-- value — but every daemon deployed so far answers both roles on one host, so
-- for existing rows it equals the server URL. The first standalone service to
-- appear will be the first row where these two differ.
UPDATE setup_sessions
   SET did_hosting_control_url = did_hosting_server_url
 WHERE did_hosting_server_url <> '';

-- One index for one namespace: two DID logs must not be resolvable at the same
-- URL. Keyed on the SERVER url — the control URL is only where an upload is
-- authenticated, so two sessions sharing a server collide however they were
-- administered.
--
-- Which sessions share a server does not follow from mode or domain_type:
--
--   vta_only     deploys no daemon; its DID goes to the shared one. A custom
--                domain would move its hostnames, not its DID host, so this
--                stays true once custom + vta_only is allowed.
--   full_stack   runs its own at dids[-<name>].<zone> — shares with nothing.
--   platform     is a full_stack whose own daemon IS the shared one, so its
--                paths sit alongside every vta_only session's.
--
-- It compares vta_name rather than the rendered path because every indexed
-- path is <vta_name>-vta — vta_only included, which is why that mode carries
-- the same suffix full_stack uses. That makes the comparison exact: a row's
-- other two paths (<vta_name>-mediator / -vtc) are unindexed, but they end in
-- suffixes no -vta path can equal, so nothing slips past by being compared
-- under the wrong name.
--
-- Partial on did_hosting_server_url <> '' so a row whose host is unknown is
-- left out rather than colliding with every other such row.
CREATE UNIQUE INDEX setup_sessions_did_path_unique
    ON setup_sessions (did_hosting_server_url, vta_name)
    WHERE did_hosting_server_url <> '';
