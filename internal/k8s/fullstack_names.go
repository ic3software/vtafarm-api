package k8s

import "fmt"

// full_stack K8s resource names. All keyed by sessionID, which is a globally
// unique PK shared across vta_only and full_stack rows in the same table, so
// these never collide with vta_only's "vta-*" names even for the same ID.

// FSVtaName, FSMediatorName, FSDidsName, FSVtcName are the PVC/Deployment/
// Service/Ingress names for each component (FSVtcName is
// full_stack-only).
func FSVtaName(sessionID uint) string      { return fmt.Sprintf("fs-%d-vta", sessionID) }
func FSMediatorName(sessionID uint) string { return fmt.Sprintf("fs-%d-mediator", sessionID) }
func FSDidsName(sessionID uint) string     { return fmt.Sprintf("fs-%d-dids", sessionID) }
func FSVtcName(sessionID uint) string      { return fmt.Sprintf("fs-%d-vtc", sessionID) }

// FSTLSSecret names both the session's Certificate and the Secret it writes —
// cert-manager takes the secretName from us, so keeping them identical means
// one name to look up when diagnosing issuance. Custom domains only; managed
// and platform sessions are served by the cluster wildcard.
func FSTLSSecret(sessionID uint) string { return fmt.Sprintf("fs-%d-tls", sessionID) }

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
func FSJobVtaRegisterDids(sessionID uint) string {
	return fmt.Sprintf("fs-%d-vta-register-dids", sessionID)
}
func FSJobImportAdminDid(sessionID uint) string {
	return fmt.Sprintf("fs-%d-import-admin-did", sessionID)
}

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
		FSJobVtaRegisterDids(sessionID),
		FSJobImportAdminDid(sessionID),
		FSJobVtcSetupKey(sessionID),
		FSJobVtcAclGrant(sessionID),
		FSJobVtcSetup(sessionID),
		FSJobVtcInvite(sessionID),
	}
}
