package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/setup"
)

// The provider half of stack connections: a full_stack owner mints a share code
// and hands it to somebody, who pastes that one code when creating a vta_only
// agent. Design: docs/custom-stack-connection-design.md §4.
//
// The code is the whole handover. An earlier cut passed a JSON bundle carrying
// the stack name, the farm and the three DID/URL values a session is built
// from — but a globally unique code already identifies its stack, those three
// values were only ever compared and never used (the row is authoritative), and
// the confirmation the recipient sees has always been rendered from this
// server's own answer rather than from the pasted text. Everything except the
// code was doing no work, and one code is what a person can read down a phone.
//
// Dropping it also removes a hazard rather than only weight: with nothing
// pasted worth rendering, a UI *cannot* present the sender's claims as facts
// about a stack.

// displayShareCode is the grouped form handed to a person. Returns "" when the
// stack is not currently shareable, so a caller can drop the field rather than
// offer a code that would be refused the moment it was used.
func displayShareCode(s *model.SetupSession) string {
	if !s.IsShared() {
		return ""
	}
	return setup.GroupShareCode(*s.ShareCode)
}

// sharingResponse is the one shape every sharing action answers with, and the
// same fields GET /setup/:id carries for a full_stack.
//
// `connections_max` is the provider's half of a number the consumer's
// ValidateConnection has always returned: §6.3's cap is what bounds how many
// agents can arrive before the owner notices, and it is their storage and
// message volume being committed, so the count belongs on their page.
// Omitted when the cap is off, so a UI renders "3 connected" rather than
// "3 of 0".
func (h *SetupHandler) sharingResponse(s *model.SetupSession) gin.H {
	resp := gin.H{
		"shared":      s.IsShared(),
		"connections": h.listConnections(s.ID),
	}
	if code := displayShareCode(s); code != "" {
		resp["share_code"] = code
	}
	if h.maxStackConnections > 0 {
		resp["connections_max"] = h.maxStackConnections
	}
	return resp
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

// resolveShareCode turns a share code into the stack it opens, in two tiers.
//
// Everything decided before the code is verified must answer identically, or
// this becomes a directory of which stacks exist and which are shared — exactly
// what the code exists to prevent. Once the caller has proved they hold a
// current one, specificity costs nothing and every remaining refusal says
// precisely what is wrong.
//
// No check here reaches the network, and none can: a code names no host. Every
// value a session is built from comes off the row this finds, so there is
// nothing a caller could type that becomes a socket — see design §3.
func (h *SetupHandler) resolveShareCode(code string) (v sharedInfra, provider *model.SetupSession, reason, detail string) {
	// ── Tier 1: does this code open anything ────────────────────────────────
	//
	// Shape first, so a mistyped code is diagnosed as itself. The check
	// character makes that a local, certain answer rather than a guess, and
	// keeps a single hand-copied glyph out of the deliberately vague message
	// below — which is the one place a user has nothing to act on.
	if strings.TrimSpace(code) == "" {
		return v, nil, reasonBadBundle, "Enter the share code you were given."
	}
	if err := setup.ValidateShareCode(code); err != nil {
		return v, nil, reasonBadBundle,
			"That doesn't look like a share code — check it against what you were sent."
	}

	const invalid = "That code doesn't open anything here. The stack may have been deleted, or its owner may " +
		"have turned sharing off or issued a new code — ask them for a current one."

	// One lookup, keyed on the code alone: it is globally unique
	// (setup_sessions_share_code_unique), so it identifies its stack without a
	// name alongside it.
	//
	// This is also what makes tier 1 answer identically for every way a code can
	// fail — no such stack, never shared, sharing turned off, rotated, or simply
	// wrong. There is one query and one answer, so the endpoint cannot be used
	// to discover which stacks exist or which are shared. A database error lands
	// here too, for the same reason: its own reason would leak that a code
	// matched whenever the read happened to fail.
	var session model.SetupSession
	err := h.db.
		Where("share_code = ? AND mode = ?", setup.NormalizeShareCode(code), model.ModeFullStack).
		First(&session).Error
	if err != nil {
		return v, nil, reasonInvalidBundle, invalid
	}

	// ── Tier 2: the caller holds a current code ─────────────────────────────
	if v, reason, detail = providerInfra(&session); reason != "" {
		// providerInfra's sentences name "the platform stack", which is wrong
		// for a stack somebody shared. The condition is the same; the wording
		// is not.
		return sharedInfra{}, nil, reasonStackNotRunning,
			"That stack isn't ready right now. Ask its owner to check it, then try again."
	}

	// No staleness comparison is needed. Deleting and recreating a stack mints a
	// fresh code, so a code from before a rebuild simply fails to resolve above
	// rather than reaching a daemon that no longer exists. The three values the
	// old bundle carried were belt to this braces.

	if h.maxStackConnections > 0 {
		var connected int64
		h.db.Model(&model.SetupSession{}).Where("provider_session_id = ?", session.ID).Count(&connected)
		if connected >= int64(h.maxStackConnections) {
			return sharedInfra{}, nil, reasonStackAtConnLimit,
				"That stack has reached its limit of connected agents. Ask an admin to raise the limit, or use a different stack."
		}
	}

	return v, &session, "", ""
}

// POST /api/v1/setup/connection/validate
//
// Runs the same checks as create and creates nothing, so the create form can
// tell the user which stack a code opens before they fill the rest of it in.
//
// This is the only way that confirmation can exist. A share code carries no
// information — the stack's name, its mediator and its DID host are all facts
// this server holds and the sender does not transmit — so a UI has nothing to
// render from except this response. That is a property worth keeping: it makes
// presenting the sender's claims as facts about a stack structurally
// impossible, rather than merely something the client is asked not to do.
//
// Not authoritative: POST /setup re-runs everything. A stack can stop running,
// rotate its code or fill up in between, so this is a courtesy and create is the
// gate.
func (h *SetupHandler) ValidateConnection(c *gin.Context) {
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "reason": reasonBadBundle})
		return
	}

	infra, provider, reason, detail := h.resolveShareCode(req.Code)
	if reason != "" {
		c.JSON(connectionRefusalStatus(reason), gin.H{"error": detail, "reason": reason})
		return
	}

	resp := gin.H{
		"stack":                  provider.VtaName,
		"farm":                   h.clusterDomain,
		"mediator_did":           infra.MediatorDid,
		"did_hosting_server_url": infra.ServerURL,
	}
	if h.maxStackConnections > 0 {
		resp["connections_used"] = h.countConnections(provider.ID)
		resp["connections_max"] = h.maxStackConnections
	}
	c.JSON(http.StatusOK, resp)
}

// countConnections is listConnections' cheap form.
func (h *SetupHandler) countConnections(providerID uint) int64 {
	var n int64
	h.db.Model(&model.SetupSession{}).Where("provider_session_id = ?", providerID).Count(&n)
	return n
}

// connectionRefusalStatus maps a refusal to its HTTP status. The reason is what
// the frontend switches on; the status is for everything else in the chain.
func connectionRefusalStatus(reason string) int {
	switch reason {
	case reasonBadBundle:
		return http.StatusBadRequest
	case reasonInvalidBundle:
		return http.StatusForbidden
	case reasonStackNotRunning, reasonStackAtConnLimit:
		return http.StatusConflict
	default:
		// wrong_farm, stack_changed — well-formed, but not usable here.
		return http.StatusUnprocessableEntity
	}
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
		c.JSON(http.StatusOK, h.sharingResponse(session))
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
		c.JSON(http.StatusOK, h.sharingResponse(session))
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

	c.JSON(http.StatusOK, h.sharingResponse(session))
}
