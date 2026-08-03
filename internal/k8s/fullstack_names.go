package k8s

import "fmt"

// full_stack K8s resource names. Keyed by sessionID, which is a globally
// unique PK shared across vta_only and full_stack rows in the same table, so
// these never collide with vta_only's "vta-*" names even for the same ID.
//
// CustomTLSSecret is the exception and is keyed by domain — see its comment.

// FSVtaName, FSMediatorName, FSDidsName, FSVtcName are the PVC/Deployment/
// Service/Ingress names for each component (FSVtcName is
// full_stack-only).
func FSVtaName(sessionID uint) string      { return fmt.Sprintf("fs-%d-vta", sessionID) }
func FSMediatorName(sessionID uint) string { return fmt.Sprintf("fs-%d-mediator", sessionID) }
func FSDidsName(sessionID uint) string     { return fmt.Sprintf("fs-%d-dids", sessionID) }
func FSVtcName(sessionID uint) string      { return fmt.Sprintf("fs-%d-vtc", sessionID) }

// CustomTLSSecret names both the Certificate and the Secret it writes —
// cert-manager takes the secretName from us, so keeping them identical means
// one name to look up when diagnosing issuance. Custom domains only; managed
// and platform sessions are served by the cluster wildcard and reach ACME never.
//
// Keyed on the DOMAIN, not the session, and that is the whole point. A
// certificate belongs to the four names, which belong to the domain — the
// session is just what happens to be running on them. Teardown keeps the Secret
// so a rebuild costs no ACME quota, and that only works if the next session
// asks for the same name: under the old fs-<sessionID>-tls the rebuild named a
// Secret that had never existed, cert-manager issued from scratch every time,
// and the kept Secret sat there unread.
//
// What that cost: CustomHosts is a pure function of (env, domain), so every
// session on one domain requests a byte-identical set of four names — exactly
// what Let's Encrypt's duplicate-certificate limit counts. Five per identical
// set, refilling one per ~34 hours, and not raisable by asking. Reuse takes a
// rebuild from one of those five to none of them.
//
// Safe as a key because setup_sessions_domain_unique makes a domain back at
// most one session — two sessions can never contend for this name.
func CustomTLSSecret(domainID uint) string { return fmt.Sprintf("custom-%d-tls", domainID) }

// Per-step setup Job (+ matching ConfigMap) names, in §6 order.
func FSJobVtaSetup(sessionID uint) string   { return fmt.Sprintf("fs-%d-vta-setup", sessionID) }
func FSJobMediatorP1(sessionID uint) string { return fmt.Sprintf("fs-%d-mediator-p1", sessionID) }
func FSJobMediatorReprov(sessionID uint) string {
	return fmt.Sprintf("fs-%d-mediator-reprov", sessionID)
}
func FSJobMediatorP2(sessionID uint) string    { return fmt.Sprintf("fs-%d-mediator-p2", sessionID) }
func FSJobDidsP1(sessionID uint) string        { return fmt.Sprintf("fs-%d-dids-p1", sessionID) }
func FSJobDidsProvision(sessionID uint) string { return fmt.Sprintf("fs-%d-dids-provision", sessionID) }
func FSJobDidsP2(sessionID uint) string        { return fmt.Sprintf("fs-%d-dids-p2", sessionID) }
func FSJobDidsInvite(sessionID uint) string    { return fmt.Sprintf("fs-%d-dids-invite", sessionID) }
func FSJobDidsLoadDid(sessionID uint) string   { return fmt.Sprintf("fs-%d-dids-load-did", sessionID) }
func FSJobDidsGrantFarm(sessionID uint) string {
	return fmt.Sprintf("fs-%d-dids-grant-farm", sessionID)
}
func FSJobVtaRegisterDids(sessionID uint) string {
	return fmt.Sprintf("fs-%d-vta-register-dids", sessionID)
}
func FSJobImportAdminDid(sessionID uint) string {
	return fmt.Sprintf("fs-%d-import-admin-did", sessionID)
}

// FSJobVtaACL is the post-provisioning ACL Job — granting a co-admin, revoking
// one, or just reading the ACL back
// (docs/platform-stack-admin-grant-design.md §6). Not a pipeline step, and one
// name for all three operations because they are mutually exclusive in time:
// each runs with the VTA scaled to 0, so two can never be in flight together,
// and reusing the name means the previous run's TTL'd Job is what
// DeleteComponentJob clears before the next.
func FSJobVtaACL(sessionID uint) string { return fmt.Sprintf("fs-%d-vta-acl", sessionID) }

// full_stack-only Jobs (design §8). FSJobVtcInvite is the reissue
// endpoint's `vtc admin invite` Job (POST /setup/:id/vtc/reissue-install),
// not a pipeline step — mirrors how FSJobDidsInvite doubles for reissue.
func FSJobVtcSetupKey(sessionID uint) string { return fmt.Sprintf("fs-%d-vtc-setup-key", sessionID) }
func FSJobVtcAclGrant(sessionID uint) string { return fmt.Sprintf("fs-%d-vtc-acl-grant", sessionID) }
func FSJobVtcSetup(sessionID uint) string    { return fmt.Sprintf("fs-%d-vtc-setup", sessionID) }
func FSJobVtcInvite(sessionID uint) string   { return fmt.Sprintf("fs-%d-vtc-invite", sessionID) }

// allFSJobNames lists every setup Job name for a session — used by teardown
// to best-effort delete each one (and its ConfigMap, where one exists).
func allFSJobNames(sessionID uint) []string {
	return []string{
		FSJobVtaSetup(sessionID),
		FSJobMediatorP1(sessionID),
		FSJobMediatorReprov(sessionID),
		FSJobMediatorP2(sessionID),
		FSJobDidsP1(sessionID),
		FSJobDidsProvision(sessionID),
		FSJobDidsP2(sessionID),
		FSJobDidsInvite(sessionID),
		FSJobDidsLoadDid(sessionID),
		FSJobDidsGrantFarm(sessionID),
		FSJobVtaRegisterDids(sessionID),
		FSJobImportAdminDid(sessionID),
		FSJobVtcSetupKey(sessionID),
		FSJobVtcAclGrant(sessionID),
		FSJobVtcSetup(sessionID),
		FSJobVtcInvite(sessionID),
	}
}
