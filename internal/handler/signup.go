package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
)

type SignupHandler struct {
	db           *gorm.DB
	jwtSecret    string
	cookieSecure bool
}

func NewSignupHandler(db *gorm.DB, jwtSecret string, cookieSecure bool) *SignupHandler {
	return &SignupHandler{db: db, jwtSecret: jwtSecret, cookieSecure: cookieSecure}
}

// Signup — public: POST /api/v1/signup {email}. Self-service account creation
// from the home page. The email is a self-declared label (nothing is sent to
// it and ownership is never verified) — the passkey the frontend registers
// right after this call is what actually authenticates the account.
//
// Outcomes:
//   - new email → account created, auto-login cookie set (201)
//   - email on an account with no passkey yet → resume that account with a
//     fresh auto-login cookie (200), so an abandoned or failed passkey
//     ceremony never strands the email or leaks an extra account
//   - email on an account that has a passkey → 409; the account is taken for
//     good (there is no email-based recovery by design — a lost passkey means
//     the account is unreachable)
func (h *SignupHandler) Signup(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required,email,max=254"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a valid email address is required"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))

	var user model.User
	err := h.db.Where("email = ?", email).First(&user).Error
	switch {
	case err == nil:
		h.resumeOrReject(c, &user, http.StatusOK)
		return
	case !errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not look up account"})
		return
	}

	user = model.User{Email: &email}
	const maxAttempts = 5
	var createErr error
	for range maxAttempts {
		user.UniqueId = generateUniqueId()
		createErr = h.db.Create(&user).Error
		if createErr == nil {
			break
		}
		// Lost a race with a concurrent signup for the same email — the unique
		// index is the arbiter; fall back to the row that won.
		if strings.Contains(createErr.Error(), "idx_users_email") {
			if h.db.Where("email = ?", email).First(&user).Error == nil {
				h.resumeOrReject(c, &user, http.StatusOK)
				return
			}
			break
		}
		if !strings.Contains(createErr.Error(), "idx_users_unique_id") {
			break
		}
	}
	if createErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create account"})
		return
	}

	h.login(c, &user, http.StatusCreated)
}

// resumeOrReject re-opens an account still waiting for its first passkey, or
// rejects the email as taken once a passkey exists.
func (h *SignupHandler) resumeOrReject(c *gin.Context, user *model.User, resumeStatus int) {
	var passkeys int64
	if err := h.db.Model(&model.UserPasskey{}).Where("user_id = ?", user.ID).Count(&passkeys).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not look up account"})
		return
	}
	if passkeys > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "this email is already registered — sign in with your passkey instead"})
		return
	}
	h.login(c, user, resumeStatus)
}

// login sets the vtafarm_user cookie so the caller can immediately register a
// passkey, mirroring the invitation-based registration.
func (h *SignupHandler) login(c *gin.Context, user *model.User, status int) {
	jwtToken, err := middleware.GenerateToken(user.ID, model.RoleUser, h.jwtSecret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate token"})
		return
	}
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(middleware.CookieUser, jwtToken, cookieMaxAge, "/", "", h.cookieSecure, true)

	c.JSON(status, gin.H{
		"id":        user.ID,
		"unique_id": user.UniqueId,
		"token":     jwtToken,
	})
}
