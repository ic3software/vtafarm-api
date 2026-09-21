package handler

import (
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/model"
)

// Granting a co-admin on the platform stack's VTA — phase 2 of
// docs/platform-stack-admin-grant-design.md. Runs through runVtaAclJob in
// admin_platform_stack_admins.go, so it takes the VTA down for the window.

// didKeyRe is the shape `pnm setup` mints: did:key with a base58btc multibase.
//
// Narrow on purpose. The ACL itself would take any DID string, but the value
// that belongs here is a PNM holder's key, and the VTA verifies a REST login's
// Data-Integrity proof with a did:key-only resolver
// (vti-common/src/auth/di_proof.rs). Admitting anything else mostly admits
// typos, which cost a maintenance window to discover and another to undo.
//
// It also keeps the value trivially safe to interpolate: shellQuote is what
// actually protects the Job command, but a DID matching this cannot contain a
// quote to begin with.
var didKeyRe = regexp.MustCompile(`^did:key:z[1-9A-HJ-NP-Za-km-z]{20,}$`)

// alreadyPresentMarker is echoed by the grant Job when the DID already had an
// entry. Not an error: `vta import-did` prompts "Overwrite?" through dialoguer
// in that case, and interact() on a pod's non-TTY stdin *errors*, so the Job
// probes with `vta acl get` first and skips the import. The outcome is the same
// one the caller asked for, so the grant still lands `granted` (§6).
const alreadyPresentMarker = "VTAFARM_ALREADY_PRESENT"

// GrantPlatformStackAdmin — POST /api/v1/admin/platform-stack/admins.
//
// Adds `did` to the platform stack's VTA ACL as an **unrestricted admin** — the
// same authority step_import_admin_did gave the stack's first admin (§2).
//
// Synchronous and slow (60–120s). The VTA ACL is authoritative; a successful
// operation also synchronizes the database snapshot returned by GET.
func (h *SetupHandler) GrantPlatformStackAdmin(c *gin.Context) {
	session := h.platformSession(c)
	if session == nil {
		return
	}

	var body struct {
		Did   string `json:"did"`
		Label string `json:"label"`
	}
	_ = c.ShouldBindJSON(&body)

	did := strings.TrimSpace(body.Did)
	if !didKeyRe.MatchString(did) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "did must be a did:key (the form `pnm setup` mints, and the only one the VTA's REST login can verify)",
		})
		return
	}
	label := strings.TrimSpace(body.Label)
	if len(label) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "label must be 64 characters or fewer"})
		return
	}
	if strings.ContainsAny(label, "\r\n\t") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "label must be a single line"})
		return
	}

	h.grantVtaAdmin(c, session, did, label, "platform admin")
}

// grantVtaAdmin performs the shared, session-scoped grant operation after the
// caller-specific route has established authority and validated its request.
func (h *SetupHandler) grantVtaAdmin(
	c *gin.Context,
	session *model.SetupSession,
	did, label string,
	actor string,
) {
	log.Printf("[vta-admins] granting super admin on session %d to %s (requested by %s)",
		session.ID, did, actor)

	logs, restartErr, runErr := h.runVtaAclJob(c.Request.Context(), session, grantCmd(did, label))
	if runErr != nil {
		respondAclJobError(c, session, runErr, restartErr)
		return
	}

	warnings := make([]string, 0, 2)
	if entries, parseErr := parseVtaAclList(logs); parseErr != nil {
		warnings = append(warnings, "The PNM was linked, but the ACL snapshot could not be parsed. Use Refresh live ACL to retry.")
	} else if syncErr := h.syncSessionAclSnapshot(session.ID, entries); syncErr != nil {
		log.Printf("[vta-admins] error: failed to sync ACL snapshot for session %d: %v", session.ID, syncErr)
		warnings = append(warnings, "The PNM was linked, but the ACL snapshot could not be saved. Use Refresh live ACL to retry.")
	}

	resp := gin.H{
		"did":    did,
		"status": "granted",
		// The caller asked for this DID to hold super admin; it already did.
		// Reported rather than swallowed so a UI can say "already an admin"
		// instead of implying it just changed something.
		"already_present": strings.Contains(logs, alreadyPresentMarker),
	}
	if restartErr != nil {
		warnings = append(warnings, restartWarning(session, restartErr))
	}
	if len(warnings) > 0 {
		resp["warning"] = strings.Join(warnings, " ")
	}
	c.JSON(http.StatusOK, resp)
}

// grantCmd probes before importing, because `vta import-did` prompts on an
// existing entry and a pod has no TTY to answer with (§6). A condition's exit
// status does not trigger `set -e`, so the probe stays a test rather than a
// failure.
//
// `set -e` ensures a failed import stops before the trailing ACL list and marker
// can make the shell script appear successful.
func grantCmd(did, label string) string {
	importCmd := "vta import-did --role admin --did " + shellQuote(did)
	if label != "" {
		importCmd += " --label " + shellQuote(label)
	}
	return "set -e\n" +
		"if vta acl get " + shellQuote(did) + " >/dev/null 2>&1; then\n" +
		"  echo " + alreadyPresentMarker + "\n" +
		"else\n" +
		"  " + importCmd + "\n" +
		"fi\n" +
		"echo " + aclListBeginMarker + "\n" +
		"vta acl list 2>&1\n" +
		"echo " + aclListEndMarker + "\n"
}
