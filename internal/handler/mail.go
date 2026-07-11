package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/mailer"
)

type MailHandler struct {
	mail *mailer.Client // nil when RESEND_API_KEY / RESEND_FROM are not configured
}

func NewMailHandler(mail *mailer.Client) *MailHandler {
	return &MailHandler{mail: mail}
}

// SendTest — POST /api/v1/admin/test-email (admin only).
// Sends a fixed test message so an admin can verify the Resend configuration
// (API key, verified sender domain) end to end.
func (h *MailHandler) SendTest(c *gin.Context) {
	if h.mail == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email sending is not configured (set RESEND_API_KEY and RESEND_FROM)"})
		return
	}

	var req struct {
		To string `json:"to" binding:"required,email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a valid \"to\" email address is required"})
		return
	}

	id, err := h.mail.Send(c.Request.Context(), req.To,
		"VTA Farm test email",
		"<p>This is a test email from vtafarm-api — Resend is configured correctly.</p>")
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id})
}
