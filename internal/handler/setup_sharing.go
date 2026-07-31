package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/setup"
)

// The provider half of stack connections: a full_stack owner mints a share code
// and hands out the bundle a vta_only session pastes on create. Design:
// docs/custom-stack-connection-design.md §4.

// connectionBundleKind and connectionBundleVersion tag the bundle so that
// pasting the wrong thing — a DID, a URL, a JWT, a bundle from a future
// version — fails with a sentence rather than five steps later with a 500.
const (
	connectionBundleKind    = "vtafarm.stack-connection"
	connectionBundleVersion = 1
)

// connectionBundle is what a provider hands out and a consumer pastes.
//
// Only Stack and Code are load-bearing: the connection is resolved by looking
// Stack up in this farm's own database and checking Code against it, and the
// three values a session is actually built from come from that row. The rest is
// shown to the recipient so they can see whose stack they are joining before
// they commit, and compared on arrival so a bundle displaying values the stack
// no longer has is refused rather than silently connecting to different ones.
//
// That split is the reason this feature has no SSRF surface: nothing here ever
// becomes a URL this server connects to. See design §3.
type connectionBundle struct {
	Version int    `json:"v"`
	Kind    string `json:"kind"`
	Farm    string `json:"farm"`
	Stack   string `json:"stack"`
	Code    string `json:"code"`

	MediatorDid         string `json:"mediator_did"`
	DidHostingServerURL string `json:"did_hosting_server_url"`
	DidHostingDid       string `json:"did_hosting_did"`
}

// buildConnectionBundle renders a shared, ready stack as a bundle. Returns nil
// when the stack is not currently shareable, so a caller can drop the field
// rather than offer a bundle that would be refused on arrival.
func buildConnectionBundle(s *model.SetupSession, farm string) *connectionBundle {
	if !s.IsShared() {
		return nil
	}
	return &connectionBundle{
		Version:             connectionBundleVersion,
		Kind:                connectionBundleKind,
		Farm:                farm,
		Stack:               s.VtaName,
		Code:                setup.GroupShareCode(*s.ShareCode),
		MediatorDid:         s.MediatorDid,
		DidHostingServerURL: s.DidsURL(),
		DidHostingDid:       s.DIDHostingDid,
	}
}

// connectionSummary is one entry in a provider's dependent list: another user's
// agent connected to this stack.
//
// Name and status only. These sessions belong to other users, and the provider's
// legitimate interest is knowing what deleting their stack would break — not who
// owns it or how it is configured.
type connectionSummary struct {
	VtaName string `json:"vta_name"`
	Status  string `json:"status"`
}

// listConnections returns the sessions connected to a provider, oldest first.
//
// Not decoration: deleting this stack is allowed and breaks every one of them
// (design §7.2), so the list is the entire mitigation — it is what lets the UI
// name them in the delete confirmation.
func (h *SetupHandler) listConnections(providerID uint) []connectionSummary {
	var rows []model.SetupSession
	if err := h.db.
		Where("provider_session_id = ?", providerID).
		Order("created_at ASC").
		Find(&rows).Error; err != nil {
		// A failed read must not blank the list into "nothing depends on this",
		// which is the one answer that would mislead someone about to delete.
		// nil renders as absent rather than as an empty list.
		return nil
	}
	out := make([]connectionSummary, len(rows))
	for i, r := range rows {
		out[i] = connectionSummary{VtaName: r.VtaName, Status: r.Status}
	}
	return out
}

type sharingRequest struct {
	// enable mints a code, disable clears it, rotate replaces it. One field
	// rather than an enabled bool plus a rotate bool, which would make
	// {"enabled": false, "rotate": true} mean nothing in particular.
	Action string `json:"action" binding:"required,oneof=enable disable rotate"`
}

// PUT /api/v1/setup/:id/sharing
func (h *SetupHandler) SetSharing(c *gin.Context) {
	if s := h.userSession(c); s != nil {
		h.setSharing(c, s)
	}
}

// AdminSetSharing — the admin-cookie twin, reaching any user's session.
func (h *SetupHandler) AdminSetSharing(c *gin.Context) {
	if s := h.adminSession(c); s != nil {
		h.setSharing(c, s)
	}
}

func (h *SetupHandler) setSharing(c *gin.Context, session *model.SetupSession) {
	var req sharingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !session.IsFullStack() {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "only a full-stack session can be shared — a VTA-only agent runs no mediator or DID host of its own"})
		return
	}
	// The platform stack is reached by the default path, which sends no bundle
	// at all. Giving it a code would produce a second way to arrive at the same
	// place, and a share code that nobody needs but anybody could leak.
	if session.DomainType == model.DomainPlatform {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "the platform stack is already the default for every VTA-only agent and is not shared by code"})
		return
	}

	if req.Action == "disable" {
		if err := h.db.Model(session).Update("share_code", nil).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update sharing"})
			return
		}
		session.ShareCode = nil
		// Deliberately no teardown of existing connections. The code gates
		// joining, not membership: sessions already connected keep running, and
		// there is no way to remove one (design §7.4).
		c.JSON(http.StatusOK, gin.H{"shared": false, "connections": h.listConnections(session.ID)})
		return
	}

	// enable and rotate both need a stack that can actually serve a connection.
	// Checked before minting so that "sharing is on" never means "on, but any
	// bundle from it is refused".
	if session.Status != "running" {
		c.JSON(http.StatusConflict, gin.H{
			"error": "this stack is still being set up — it can be shared once it's running"})
		return
	}
	if session.MediatorDid == "" || session.DIDHostingDid == "" {
		c.JSON(http.StatusConflict, gin.H{
			"error": "this stack hasn't published its mediator and DID hosting identifiers yet — try again shortly"})
		return
	}

	// enable is idempotent: turning on something already on returns the current
	// code rather than silently invalidating every bundle already handed out.
	// Replacing one is what rotate is for, and it asks explicitly.
	if req.Action == "enable" && session.ShareCode != nil && *session.ShareCode != "" {
		c.JSON(http.StatusOK, gin.H{
			"shared":      true,
			"connection":  buildConnectionBundle(session, h.clusterDomain),
			"connections": h.listConnections(session.ID),
		})
		return
	}

	code, err := setup.NewShareCode()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate a share code"})
		return
	}
	stored := setup.NormalizeShareCode(code)
	if err := h.db.Model(session).Update("share_code", stored).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update sharing"})
		return
	}
	session.ShareCode = &stored

	c.JSON(http.StatusOK, gin.H{
		"shared":      true,
		"connection":  buildConnectionBundle(session, h.clusterDomain),
		"connections": h.listConnections(session.ID),
	})
}
