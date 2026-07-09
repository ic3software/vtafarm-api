-- Names drive globally-visible DNS hostnames (vta-<name> / mediator-<name> /
-- dids-<name>, and vtc-<name>), so they must be unique across ALL users'
-- sessions. These indexes back the handler-level checks against concurrent
-- creates. If this migration fails, existing rows share a name (e.g. the old
-- 'personal-vta' default) — de-duplicate them first.
CREATE UNIQUE INDEX setup_sessions_vta_name_unique ON setup_sessions (vta_name);

-- vtc_name only matters for sessions that actually run a VTC — rows from the
-- other modes all share the column default ('' since 000014, historically
-- 'personal-vtc') and never provision one, so the index must be partial:
-- a full-table unique index would collide on those equal placeholder values.
CREATE UNIQUE INDEX setup_sessions_vtc_name_unique ON setup_sessions (vtc_name)
    WHERE mode = 'full_stack_with_vtc';
