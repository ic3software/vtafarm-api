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
	case "", model.ModeVtaOnly, model.ModeFullStack, model.ModeFullStackWithVtc:
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
		"all": 0, model.ModeVtaOnly: 0, model.ModeFullStack: 0, model.ModeFullStackWithVtc: 0,
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

	type sessionItem struct {
		ID           uint   `json:"id"`
		UniqueId     string `json:"unique_id"`
		UserUniqueId string `json:"user_unique_id"`
		VtaName      string `json:"vta_name"`
		VtcName      string `json:"vtc_name,omitempty"`
		Mode         string `json:"mode"`
		Status       string `json:"status"`
		ErrorMsg     string `json:"error_msg,omitempty"`
		Fqdn         string `json:"fqdn"`
		// Per-component images — empty for components the mode doesn't run.
		VtaImage      string `json:"vta_image,omitempty"`
		MediatorImage string `json:"mediator_image,omitempty"`
		DidsImage     string `json:"dids_image,omitempty"`
		VtcImage      string `json:"vtc_image,omitempty"`
		CreatedAt     string `json:"created_at"`
	}
	items := make([]sessionItem, len(sessions))
	for i, s := range sessions {
		items[i] = sessionItem{
			ID:            s.ID,
			UniqueId:      s.UniqueId,
			UserUniqueId:  userUniqueIds[s.UserID],
			VtaName:       s.VtaName,
			VtcName:       s.VtcName,
			Mode:          s.Mode,
			Status:        s.Status,
			ErrorMsg:      s.ErrorMsg,
			Fqdn:          s.FQDN(),
			VtaImage:      s.VtaImage,
			MediatorImage: s.MediatorImage,
			DidsImage:     s.DidsImage,
			VtcImage:      s.VtcImage,
			CreatedAt:     s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
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

// AdminDeleteSession — DELETE /api/v1/admin/setup-sessions/:id (admin only).
// Tears down any user's session, identified by unique_id alone — the one
// difference from the user-facing DELETE /setup/:id, which is scoped to the
// caller. Destructive and irreversible: the frontend gates it behind a
// type-the-session-id confirmation.
func (h *SetupHandler) AdminDeleteSession(c *gin.Context) {
	publicID := c.Param("id")

	var session model.SetupSession
	if err := h.db.Where("unique_id = ?", publicID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	log.Printf("[admin] deleting session %d (%s) owned by user %d", session.ID, session.UniqueId, session.UserID)

	h.teardownSession(c, &session)
}
