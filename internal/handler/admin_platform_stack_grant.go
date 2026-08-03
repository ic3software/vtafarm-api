package handler

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/middleware"
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
// Synchronous and slow (60–120s). The grant row is written `pending` before any
// Kubernetes work starts, so a client that times out at a proxy has not lost
// the operation: it still lands `granted` or `failed`, and GET shows it.
func (h *SetupHandler) GrantPlatformStackAdmin(c *gin.Context) {
	session := h.platformSession(c)
	if session == nil {
		return
	}

	var body struct {
		Did     string `json:"did"`
		Label   string `json:"label"`
		Confirm string `json:"confirm"`
	}
	_ = c.ShouldBindJSON(&body)
	if !requireStackConfirm(c, session, body.Confirm) {
		return
	}

	did := strings.TrimSpace(body.Did)
	if !didKeyRe.MatchString(did) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "did must be a did:key (the form `pnm setup` mints, and the only one the VTA's REST login can verify)",
		})
		return
	}
	// Required, not decorative. The DID in this request stops being the
	// holder's DID on their first connect — `POST /acl/swap` moves the entry
	// onto a freshly minted one — and of everything on that entry the label is
	// the only human-readable field that survives the move
	// (vta-service/src/operations/acl.rs `with_label(old.label.clone())`).
	//
	// Grant without one and the ACL ends up holding an unidentifiable did:key —
	// and since removal happens at a `pnm acl list` prompt, that label is what
	// the person deciding is reading. Cheaper to insist here.
	label := strings.TrimSpace(body.Label)
	if label == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "label is required — it is the only identifier that survives PNM's key rotation, " +
				"and without it this entry cannot be attributed to anyone later",
		})
		return
	}
	if len(label) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "label must be 64 characters or fewer"})
		return
	}

	// Not idempotent by design: a second grant of a live DID is a mistake worth
	// surfacing, not a no-op worth hiding, because it costs a window either
	// way. The partial unique index is the real gate; this is the readable
	// error in front of it.
	var existing model.VtaAdminGrant
	err := h.db.Where("session_id = ? AND did = ? AND status IN ?",
		session.ID, did, []string{model.GrantPending, model.GrantGranted}).First(&existing).Error
	if err == nil {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("this DID already has a %s grant on the platform stack", existing.Status),
		})
		return
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check existing grants"})
		return
	}

	// The cross-replica half of the mutual exclusion in runVtaAclJob. That lock
	// is in-process, so it only serialises callers hitting the same API pod;
	// this catches a second pod, because the `pending` row is written before any
	// Kubernetes work starts and is therefore visible to everyone.
	//
	// Bounded by aclJobStale so a request that died mid-window cannot wedge the
	// route permanently — past that deadline no Job of ours can still be alive.
	var inFlight int64
	if countErr := h.db.Model(&model.VtaAdminGrant{}).
		Where("session_id = ? AND status = ? AND created_at > ?",
			session.ID, model.GrantPending, time.Now().Add(-aclJobStale)).
		Count(&inFlight).Error; countErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check for an in-flight grant"})
		return
	}
	if inFlight > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": errAclJobBusy.Error()})
		return
	}

	grant := model.VtaAdminGrant{
		SessionID:   session.ID,
		Did:         did,
		Label:       label,
		Status:      model.GrantPending,
		RequestedBy: callingAdminID(c),
	}
	if createErr := h.db.Create(&grant).Error; createErr != nil {
		// Lost a race with a concurrent grant of the same DID — the partial
		// unique index caught what the read above could not.
		c.JSON(http.StatusConflict, gin.H{"error": "a grant for this DID was just created"})
		return
	}
	log.Printf("[platform-admins] granting super admin on session %d to %s (requested by admin %v)",
		session.ID, did, grant.RequestedBy)

	logs, restartErr, runErr := h.runVtaAclJob(c.Request.Context(), session, grantCmd(did, label))
	if runErr != nil {
		h.markGrant(&grant, model.GrantFailed, runErr.Error())
		respondAclJobError(c, runErr, restartErr)
		return
	}

	h.markGrant(&grant, model.GrantGranted, "")

	resp := gin.H{
		"did":    did,
		"status": grant.Status,
		// The caller asked for this DID to hold super admin; it already did.
		// Reported rather than swallowed so a UI can say "already an admin"
		// instead of implying it just changed something.
		"already_present": strings.Contains(logs, alreadyPresentMarker),
	}
	if restartErr != nil {
		resp["warning"] = restartWarning(restartErr)
	}
	c.JSON(http.StatusOK, resp)
}

// grantCmd probes before importing, because `vta import-did` prompts on an
// existing entry and a pod has no TTY to answer with (§6). A condition's exit
// status does not trigger `set -e`, so the probe stays a test rather than a
// failure.
//
// `set -e` is defensive rather than load-bearing now that the import is the last
// thing to run: with a trailing command it would be the difference between
// reporting a grant and reporting the truth, so it stays in front of the next
// person who appends a line.
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
		"fi\n"
}

// markGrant moves a grant row to its terminal state, keeping the in-memory copy
// in step so the caller can report from it.
func (h *SetupHandler) markGrant(grant *model.VtaAdminGrant, status, errMsg string) {
	now := time.Now()
	updates := map[string]any{"status": status, "error_msg": errMsg, "updated_at": now}
	if status == model.GrantGranted {
		updates["granted_at"] = now
		grant.GrantedAt = &now
	}
	if err := h.db.Model(&model.VtaAdminGrant{}).Where("id = ?", grant.ID).Updates(updates).Error; err != nil {
		log.Printf("[platform-admins] error: failed to mark grant %d as %s: %v", grant.ID, status, err)
	}
	grant.Status = status
	grant.ErrorMsg = errMsg
}

// callingAdminID reads the admin's id from the request. On an admin-cookie
// route the JWT's UserID claim is an admins.id — admins are their own table
// (see handler/admin_enroll.go, which mints the token from admin.ID).
func callingAdminID(c *gin.Context) *uint {
	v, ok := c.Get(middleware.ContextUserID)
	if !ok {
		return nil
	}
	id, ok := v.(uint)
	if !ok {
		return nil
	}
	return &id
}
