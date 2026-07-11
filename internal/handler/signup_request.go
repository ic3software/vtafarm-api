package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/mailer"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
)

// signupRequestsPageSize is fixed server-side, matching the admin sessions list.
const signupRequestsPageSize = 20

type SignupRequestHandler struct {
	db        *gorm.DB
	mail      *mailer.Client // nil → approval still works, but no email is sent
	publicURL string         // frontend origin for building /register links
}

func NewSignupRequestHandler(db *gorm.DB, mail *mailer.Client, publicURL string) *SignupRequestHandler {
	return &SignupRequestHandler{db: db, mail: mail, publicURL: publicURL}
}

// Create — public: POST /api/v1/signup-requests. A visitor asks for an
// account by leaving their email. A repeat request for the same email answers
// 200 with status "already_requested" so the UI can tell the visitor their
// request is already on file.
func (h *SignupRequestHandler) Create(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required,email,max=254"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a valid email address is required"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))

	sr := model.SignupRequest{Email: email, Status: model.SignupRequestPending}
	if err := h.db.Create(&sr).Error; err != nil {
		if strings.Contains(err.Error(), "idx_signup_requests_email") {
			c.JSON(http.StatusOK, gin.H{"status": "already_requested"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not save request"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "received"})
}

// List — admin: GET /api/v1/admin/signup-requests?page=N&state=S. Requests
// newest first, paginated, with the issued invitation's state for approved
// ones. The optional state filter matches the UI's derived states: pending,
// invited (active link), expired (link expired unused), registered (link used).
func (h *SignupRequestHandler) List(c *gin.Context) {
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page must be a positive integer"})
		return
	}

	state := c.Query("state")
	switch state {
	case "", "pending", "invited", "expired", "registered":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid state"})
		return
	}
	filtered := func(q *gorm.DB) *gorm.DB {
		join := func() *gorm.DB {
			return q.Joins("JOIN invitation_links ON invitation_links.id = signup_requests.invitation_id")
		}
		switch state {
		case "pending":
			return q.Where("signup_requests.status = ?", model.SignupRequestPending)
		case "invited":
			return join().Where("invitation_links.used_at IS NULL AND invitation_links.expires_at > NOW()")
		case "expired":
			return join().Where("invitation_links.used_at IS NULL AND invitation_links.expires_at <= NOW()")
		case "registered":
			return join().Where("invitation_links.used_at IS NOT NULL")
		}
		return q
	}

	var total int64
	if err := filtered(h.db.Model(&model.SignupRequest{})).Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not count signup requests"})
		return
	}

	// Per-state totals for the filter buttons, independent of the active filter.
	var counts struct {
		All        int64 `json:"all"`
		Pending    int64 `json:"pending"`
		Invited    int64 `json:"invited"`
		Expired    int64 `json:"expired"`
		Registered int64 `json:"registered"`
	}
	if err := h.db.Raw(`SELECT
			COUNT(*) AS "all",
			COUNT(*) FILTER (WHERE signup_requests.status = ?) AS pending,
			COUNT(*) FILTER (WHERE invitation_links.id IS NOT NULL AND invitation_links.used_at IS NULL AND invitation_links.expires_at > NOW()) AS invited,
			COUNT(*) FILTER (WHERE invitation_links.id IS NOT NULL AND invitation_links.used_at IS NULL AND invitation_links.expires_at <= NOW()) AS expired,
			COUNT(*) FILTER (WHERE invitation_links.used_at IS NOT NULL) AS registered
		FROM signup_requests
		LEFT JOIN invitation_links ON invitation_links.id = signup_requests.invitation_id`,
		model.SignupRequestPending).Scan(&counts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not count signup requests"})
		return
	}

	var reqs []model.SignupRequest
	if err := filtered(h.db.Model(&model.SignupRequest{})).
		Select("signup_requests.*").
		Order("signup_requests.id desc").
		Limit(signupRequestsPageSize).
		Offset((page - 1) * signupRequestsPageSize).
		Find(&reqs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch signup requests"})
		return
	}

	invIDs := make([]uint, 0, len(reqs))
	for _, r := range reqs {
		if r.InvitationID != nil {
			invIDs = append(invIDs, *r.InvitationID)
		}
	}
	invByID := make(map[uint]model.InvitationLink, len(invIDs))
	if len(invIDs) > 0 {
		var invs []model.InvitationLink
		if err := h.db.Where("id IN ?", invIDs).Find(&invs).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch invitations"})
			return
		}
		for _, inv := range invs {
			invByID[inv.ID] = inv
		}
	}

	type item struct {
		ID              uint    `json:"id"`
		Email           string  `json:"email"`
		Status          string  `json:"status"`
		CreatedAt       string  `json:"created_at"`
		EmailSentAt     *string `json:"email_sent_at,omitempty"`
		InviteToken     string  `json:"invite_token,omitempty"`
		InviteExpiresAt string  `json:"invite_expires_at,omitempty"`
		InviteUsedAt    *string `json:"invite_used_at,omitempty"`
	}
	result := make([]item, len(reqs))
	for i, r := range reqs {
		it := item{
			ID:        r.ID,
			Email:     r.Email,
			Status:    r.Status,
			CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
		}
		if r.EmailSentAt != nil {
			s := r.EmailSentAt.UTC().Format(time.RFC3339)
			it.EmailSentAt = &s
		}
		if r.InvitationID != nil {
			if inv, ok := invByID[*r.InvitationID]; ok {
				it.InviteToken = inv.Token
				it.InviteExpiresAt = inv.ExpiresAt.UTC().Format(time.RFC3339)
				if inv.UsedAt != nil {
					s := inv.UsedAt.UTC().Format(time.RFC3339)
					it.InviteUsedAt = &s
				}
			}
		}
		result[i] = it
	}
	c.JSON(http.StatusOK, gin.H{
		"items":     result,
		"total":     total,
		"page":      page,
		"page_size": signupRequestsPageSize,
		"counts":    counts,
	})
}

// Approve — admin: POST /api/v1/admin/signup-requests/approve {ids: [...]}.
// Issues a fresh invitation link for each request and emails it to the
// requester. Re-approving is allowed — it issues a new link and re-sends
// (e.g. after the previous one expired). A send failure does not roll back
// that approval; the result carries the link so the admin can deliver it
// manually. Requests are processed independently: one failure doesn't stop
// the rest.
func (h *SignupRequestHandler) Approve(c *gin.Context) {
	var req struct {
		IDs []uint `json:"ids" binding:"required,min=1,max=100"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids must be a non-empty array of at most 100 request ids"})
		return
	}
	adminID, _ := c.Get(middleware.ContextUserID)

	type result struct {
		ID         uint   `json:"id"`
		Email      string `json:"email,omitempty"`
		InviteURL  string `json:"invite_url,omitempty"`
		EmailSent  bool   `json:"email_sent"`
		EmailError string `json:"email_error,omitempty"`
		Error      string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(req.IDs))
	for _, id := range req.IDs {
		var sr model.SignupRequest
		if err := h.db.First(&sr, id).Error; err != nil {
			results = append(results, result{ID: id, Error: "not found"})
			continue
		}
		inviteURL, emailSent, emailErr, err := h.approveOne(c.Request.Context(), adminID.(uint), &sr)
		if err != nil {
			results = append(results, result{ID: id, Email: sr.Email, Error: "approve failed"})
			continue
		}
		results = append(results, result{
			ID: id, Email: sr.Email, InviteURL: inviteURL,
			EmailSent: emailSent, EmailError: emailErr,
		})
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}

// approveOne issues an invitation for one request, emails it, and marks the
// request approved. Any previous unused invitation is expired first, so only
// the newest link ever works. emailErr is a soft failure (approval stands,
// link needs manual delivery); err means the approval itself failed.
func (h *SignupRequestHandler) approveOne(ctx context.Context, adminID uint, sr *model.SignupRequest) (inviteURL string, emailSent bool, emailErr string, err error) {
	if sr.InvitationID != nil {
		if err := h.db.Model(&model.InvitationLink{}).
			Where("id = ? AND used_at IS NULL", *sr.InvitationID).
			Update("expires_at", time.Now()).Error; err != nil {
			return "", false, "", err
		}
	}

	token, err := generateInviteToken()
	if err != nil {
		return "", false, "", err
	}
	inv := model.InvitationLink{
		Token:     token,
		AdminID:   adminID,
		ExpiresAt: time.Now().Add(defaultInvitationTTL),
	}
	if err := h.db.Create(&inv).Error; err != nil {
		return "", false, "", err
	}

	inviteURL = fmt.Sprintf("%s/register/%s", h.publicURL, inv.Token)

	switch {
	case h.mail == nil:
		emailErr = "email sending is not configured"
	case h.publicURL == "":
		emailErr = "PUBLIC_BASE_URL is not configured"
	default:
		if _, sendErr := h.mail.Send(ctx, sr.Email,
			"Your VTA Farm invitation", inviteEmailHTML(inviteURL)); sendErr != nil {
			emailErr = sendErr.Error()
		} else {
			emailSent = true
		}
	}

	updates := map[string]any{
		"status":        model.SignupRequestApproved,
		"admin_id":      adminID,
		"invitation_id": inv.ID,
	}
	if emailSent {
		updates["email_sent_at"] = time.Now()
	}
	if err := h.db.Model(sr).Updates(updates).Error; err != nil {
		return inviteURL, emailSent, emailErr, err
	}
	return inviteURL, emailSent, emailErr, nil
}

func inviteEmailHTML(inviteURL string) string {
	return fmt.Sprintf(`<p>Your request for a VTA Farm account has been approved.</p>
<p><a href="%s">Click here to create your account</a> — or open this link:</p>
<p>%s</p>
<p>The link is valid for 24 hours and can be used once. If it expires, just reply to let us know and we'll send a new one.</p>`,
		inviteURL, inviteURL)
}
