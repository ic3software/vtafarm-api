package handler

import (
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/model"
)

// adminSessionsPageSize is fixed server-side — the admin sessions list always
// returns at most 20 rows per page regardless of what the client asks for.
const adminSessionsPageSize = 20

// AdminListSessions — GET /api/v1/admin/setup-sessions?page=N&mode=M (admin only).
// Lists ALL users' setup sessions ordered by id DESC (newest first), paginated
// at adminSessionsPageSize per page. An optional mode filter narrows the list
// (and total) to one setup mode. This is the read side of the session
// upgrade workflow: it exposes each session's per-component images so an admin
// can see what's deployed before upgrading.
func (h *SetupHandler) AdminListSessions(c *gin.Context) {
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page must be a positive integer"})
		return
	}

	mode := c.Query("mode")
	switch mode {
	case "", model.ModeVtaOnly, model.ModeFullStack:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid mode"})
		return
	}
	filtered := func(q *gorm.DB) *gorm.DB {
		if mode != "" {
			return q.Where("mode = ?", mode)
		}
		return q
	}

	var total int64
	if err := filtered(h.db.Model(&model.SetupSession{})).Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not count sessions"})
		return
	}

	// Per-mode totals for the filter buttons, independent of the active filter.
	var modeCounts []struct {
		Mode  string
		Count int64
	}
	if err := h.db.Model(&model.SetupSession{}).
		Select("mode, COUNT(*) AS count").Group("mode").
		Find(&modeCounts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not count sessions"})
		return
	}
	counts := map[string]int64{
		"all": 0, model.ModeVtaOnly: 0, model.ModeFullStack: 0,
	}
	for _, mc := range modeCounts {
		counts[mc.Mode] = mc.Count
		counts["all"] += mc.Count
	}

	var sessions []model.SetupSession
	if err := filtered(h.db.Order("id desc")).
		Limit(adminSessionsPageSize).
		Offset((page - 1) * adminSessionsPageSize).
		Find(&sessions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch sessions"})
		return
	}

	// Resolve owners' public ids for just the users on this page.
	userIDs := make([]uint, 0, len(sessions))
	seen := make(map[uint]bool, len(sessions))
	for _, s := range sessions {
		if !seen[s.UserID] {
			seen[s.UserID] = true
			userIDs = append(userIDs, s.UserID)
		}
	}
	userUniqueIds := make(map[uint]string, len(userIDs))
	if len(userIDs) > 0 {
		var users []model.User
		if err := h.db.Where("id IN ?", userIDs).Find(&users).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch session owners"})
			return
		}
		for _, u := range users {
			userUniqueIds[u.ID] = u.UniqueId
		}
	}

	// Which stack each vta_only row on this page connects to, and how many rows
	// connect to each full_stack on it. Support's first question about a broken
	// agent is whose infrastructure it is on, and the answer is otherwise a
	// URL-to-URL comparison across two queries.
	//
	// Batched over the page for the same reason the owner lookup above is: 20
	// rows must not become 20 round trips.
	providerNames := make(map[uint]string)
	connectionCounts := make(map[uint]int64)
	{
		providerIDs := make([]uint, 0, len(sessions))
		fullStackIDs := make([]uint, 0, len(sessions))
		for _, s := range sessions {
			if s.ProviderSessionID != nil {
				providerIDs = append(providerIDs, *s.ProviderSessionID)
			}
			if s.IsFullStack() {
				fullStackIDs = append(fullStackIDs, s.ID)
			}
		}
		if len(providerIDs) > 0 {
			var providers []model.SetupSession
			if err := h.db.Select("id, vta_name").Where("id IN ?", providerIDs).Find(&providers).Error; err == nil {
				for _, p := range providers {
					providerNames[p.ID] = p.VtaName
				}
			}
		}
		if len(fullStackIDs) > 0 {
			var counts []struct {
				ProviderSessionID uint
				Count             int64
			}
			if err := h.db.Model(&model.SetupSession{}).
				Select("provider_session_id, COUNT(*) AS count").
				Where("provider_session_id IN ?", fullStackIDs).
				Group("provider_session_id").
				Find(&counts).Error; err == nil {
				for _, c := range counts {
					connectionCounts[c.ProviderSessionID] = c.Count
				}
			}
		}
	}

	type sessionItem struct {
		// The numeric PK, for ordering only — never an address. vta_name is what
		// the routes take, so there is no separate identifier field here.
		ID           uint   `json:"id"`
		UserUniqueId string `json:"user_unique_id"`
		VtaName      string `json:"vta_name"`
		VtcName      string `json:"vtc_name,omitempty"`
		Mode         string `json:"mode"`
		// managed | custom | platform — the platform row is the farm's own
		// stack and the one deletion that needs an explicit confirm.
		DomainType string `json:"domain_type"`
		Status     string `json:"status"`
		ErrorMsg   string `json:"error_msg,omitempty"`
		Fqdn       string `json:"fqdn"`
		// Per-component images — empty for components the mode doesn't run.
		VtaImage      string `json:"vta_image,omitempty"`
		MediatorImage string `json:"mediator_image,omitempty"`
		DidsImage     string `json:"dids_image,omitempty"`
		VtcImage      string `json:"vtc_image,omitempty"`
		CreatedAt     string `json:"created_at"`
		// vta_only: where its mediator and DID host came from. Provider names
		// the stack when that is in_farm; ProviderGone says the stack was
		// deleted, which is why no name is available rather than a lookup having
		// failed.
		ConnectionSource string `json:"connection_source,omitempty"`
		Provider         string `json:"provider,omitempty"`
		ProviderGone     bool   `json:"provider_gone,omitempty"`
		// full_stack: whether it currently accepts connections, and how many it
		// already has. Both matter before deleting one.
		Shared          bool  `json:"shared,omitempty"`
		ConnectionCount int64 `json:"connection_count,omitempty"`
	}
	items := make([]sessionItem, len(sessions))
	for i, s := range sessions {
		items[i] = sessionItem{
			ID:            s.ID,
			UserUniqueId:  userUniqueIds[s.UserID],
			VtaName:       s.VtaName,
			VtcName:       s.VtcName,
			Mode:          s.Mode,
			DomainType:    s.DomainType,
			Status:        s.Status,
			ErrorMsg:      s.ErrorMsg,
			Fqdn:          s.FQDN(),
			VtaImage:      s.VtaImage,
			MediatorImage: s.MediatorImage,
			DidsImage:     s.DidsImage,
			VtcImage:      s.VtcImage,
			CreatedAt:     s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if s.IsFullStack() {
			items[i].Shared = s.IsShared()
			items[i].ConnectionCount = connectionCounts[s.ID]
			continue
		}
		items[i].ConnectionSource = s.ConnectionSource
		if s.ProviderSessionID != nil {
			items[i].Provider = providerNames[*s.ProviderSessionID]
		} else {
			items[i].ProviderGone = s.IsOrphaned()
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"items":     items,
		"total":     total,
		"page":      page,
		"page_size": adminSessionsPageSize,
		"counts":    counts,
	})
}

// AdminSessionLogs — GET /api/v1/admin/setup-sessions/:id/logs (admin only).
//
// The admin-cookie twin of GET /setup/:id/logs, differing only in the lookup:
// vta_name alone, with no user_id filter. It exists because the platform stack
// is owned by a passkey-less system account — nobody can ever hold that user's
// cookie, so without this route the farm's own stack is the one session whose
// provisioning nobody can watch.
func (h *SetupHandler) AdminSessionLogs(c *gin.Context) {
	publicID := c.Param("id")

	var session model.SetupSession
	if err := h.db.Where("vta_name = ?", publicID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	if h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s not configured"})
		return
	}
	if !session.IsFullStack() {
		// vta_only's log sources are keyed to statuses the admin panel has no
		// view for; there is no caller for it and no reason to duplicate it.
		c.JSON(http.StatusNotImplemented, gin.H{"error": "admin log streaming covers full_stack sessions only"})
		return
	}

	h.logsFullStack(c, &session)
}

// AdminDeleteSession — DELETE /api/v1/admin/setup-sessions/:id (admin only).
// Tears down any user's session, identified by vta_name alone — the one
// difference from the user-facing DELETE /setup/:id, which is scoped to the
// caller. Destructive and irreversible: the frontend gates it behind a
// type-the-session-id confirmation.
//
// Deleting the platform stack additionally requires {"confirm": "<label>"} in
// the body. That one is not left to the UI: it is the only deletion in the
// product that degrades every other user's service — every vta_only session
// loses its mediator and DID host — and it sits one mis-click away in an admin
// table.
func (h *SetupHandler) AdminDeleteSession(c *gin.Context) {
	publicID := c.Param("id")

	var session model.SetupSession
	if err := h.db.Where("vta_name = ?", publicID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	if session.DomainType == model.DomainPlatform {
		var body struct {
			Confirm string `json:"confirm"`
		}
		// A missing/!JSON body is the mis-click case, so bind errors are not
		// distinguished from a wrong value — both mean "not confirmed".
		_ = c.ShouldBindJSON(&body)
		if body.Confirm != session.VtaName {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "deleting the platform stack takes every vta_only session's mediator and DID host with it — " +
					`send {"confirm": "` + session.VtaName + `"} to proceed`,
			})
			return
		}
	}

	log.Printf("[admin] deleting %s session %d (%s), domain_type=%s, owned by user %d",
		session.Mode, session.ID, session.VtaName, session.DomainType, session.UserID)

	h.teardownSession(c, &session)
}
