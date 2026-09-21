-- The VTA ACL is authoritative and vta_acl_entries stores its last complete
-- synchronized view. Grant rows contain temporary DIDs that become stale after
-- PNM key rotation, while vta_acl_snapshots.maintenance_started_at now provides
-- the cross-replica lock that pending grant rows previously supplied.
DROP TABLE IF EXISTS vta_admin_grants;
