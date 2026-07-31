-- Lets a vta_only session connect to a full_stack other than the platform one,
-- provided that stack is one this farm provisioned. Design:
-- docs/custom-stack-connection-design.md.
--
-- The three values a vta_only session is actually wired to — mediator_did,
-- did_hosting_server_url, did_hosting_control_url — are already per-session
-- columns (000023), so nothing here holds the connection itself. What is added
-- is the grant on the provider side and the link on the consumer side.

-- ── Provider ────────────────────────────────────────────────────────────────
-- NULL means "not shared". One nullable column rather than a boolean plus a
-- code, because the two would always have to agree and the code alone answers
-- both questions: minting one enables sharing, clearing it disables, and
-- replacing it invalidates every bundle already handed out without touching
-- anyone already connected.
--
-- Deliberately not hashed. It is a capability its owner displays to themselves
-- and reads aloud, not a password — and rotation handles everything hashing
-- would, without making the value unrecoverable to the person who has to share
-- it.
ALTER TABLE setup_sessions ADD COLUMN share_code TEXT NULL;

-- ── Consumer ────────────────────────────────────────────────────────────────
-- Neither column is needed to RUN a session: the three snapshotted values above
-- already do that, and they stay authoritative because a did:webvh bakes its
-- host in at mint time. These exist for the three things a snapshot cannot
-- answer — finding a stack's dependents cheaply, naming the provider in the UI
-- instead of showing a URL, and letting support see the topology without
-- correlating URLs by eye.
--
-- 'external' is not admitted. Stacks outside this farm are out of scope: the
-- farm's client DID is enrolled as an admin in every full_stack daemon it
-- provisioned (step_dids_grant_farm) and in nothing else, so a stack we did not
-- build would 401 on the first DID upload. Adding the value later is a one-line
-- ALTER; leaving it out now means the schema states the same scope the code
-- does.
ALTER TABLE setup_sessions
    ADD COLUMN connection_source TEXT NOT NULL DEFAULT 'platform'
        CHECK (connection_source IN ('platform', 'in_farm')),
    ADD COLUMN provider_session_id BIGINT NULL
        REFERENCES setup_sessions(id) ON DELETE SET NULL;

-- ON DELETE SET NULL is not a fallback, it IS the orphan mechanism. Deleting a
-- provider is allowed and blocks on nothing, so no handler writes to its
-- dependents; Postgres nulls the link in the same transaction, and
--
--     connection_source = 'in_farm' AND provider_session_id IS NULL
--
-- is permanently "the stack this agent connected to is gone". Nothing has to
-- have been running at the moment of deletion and there is no reconciler to
-- drift. RESTRICT would instead pin a provider forever, since there is
-- deliberately no way to remove a single connection.
CREATE INDEX setup_sessions_provider_idx
    ON setup_sessions (provider_session_id)
    WHERE provider_session_id IS NOT NULL;

-- ── Backfill ────────────────────────────────────────────────────────────────
-- Every session that exists today predates the feature, so connection_source's
-- 'platform' default is already right for all of them and needs no UPDATE.
--
-- did_hosting_did is a different matter. It has been a full_stack output column
-- (the daemon's own DID, step 3d) and is '' on every vta_only row. Its meaning
-- widens here to "the DID of the daemon at did_hosting_control_url", which is
-- true for both modes, and Factory.For now refuses a daemon reporting a DID
-- other than the one recorded. Populating it for existing vta_only rows is what
-- arms that check for sessions built before it existed — an empty value means
-- "no expectation on record" and accepts whatever the daemon claims.
--
-- The value comes from the platform stack, which is the only daemon any
-- existing vta_only session can have been wired to. Joined on the server URL
-- rather than assumed, so a row pointing at a rebuilt or absent stack is left
-- at '' rather than given a DID that was never its daemon's.
UPDATE setup_sessions AS consumer
   SET did_hosting_did = provider.did_hosting_did
  FROM setup_sessions AS provider
 WHERE consumer.mode = 'vta_only'
   AND consumer.did_hosting_did = ''
   AND consumer.did_hosting_server_url <> ''
   AND provider.mode = 'full_stack'
   AND provider.did_hosting_did <> ''
   AND provider.did_hosting_server_url = consumer.did_hosting_server_url;
