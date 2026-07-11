package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
)

// recoveryTTL is deliberately short: a recovery link is full account takeover
// power, and the admin hands it to a person who is waiting for it right now.
const recoveryTTL = time.Hour

type RecoveryHandler struct {
	db           *gorm.DB
	jwtSecret    string
	cookieSecure bool
}

func NewRecoveryHandler(db *gorm.DB, jwtSecret string, cookieSecure bool) *RecoveryHandler {
	return &RecoveryHandler{db: db, jwtSecret: jwtSecret, cookieSecure: cookieSecure}
}

// Create — admin: POST /api/v1/admin/users/:id/recovery-link (:id is the
// user's unique_id, matching the beta-access route). Issues a 1-hour,
// single-use login link for that account. The admin verifies the requester's
// identity (e.g. against the account's email) and delivers the URL out of
// band — the system sends nothing. Any previous unused link for the same
// account is expired so only the newest one works.
func (h *RecoveryHandler) Create(c *gin.Context) {
	adminID, _ := c.Get(middleware.ContextUserID)

	var user model.User
	if err := h.db.Where("unique_id = ?", c.Param("id")).First(&user).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	if err := h.db.Model(&model.RecoveryLink{}).
		Where("user_id = ? AND used_at IS NULL AND expires_at > NOW()", user.ID).
		Update("expires_at", time.Now()).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not expire previous links"})
		return
	}

	token, err := generateInviteToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate token"})
		return
	}
	link := model.RecoveryLink{
		Token:     token,
		UserID:    user.ID,
		AdminID:   adminID.(uint),
		ExpiresAt: time.Now().Add(recoveryTTL),
	}
	if err := h.db.Create(&link).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create recovery link"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"token":      link.Token,
		"expires_at": link.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// Validate — public: GET /api/v1/recovery/:token. Same contract as the
// invitation validate endpoint so the frontend page can share its states.
func (h *RecoveryHandler) Validate(c *gin.Context) {
	var link model.RecoveryLink
	if err := h.db.Where("token = ?", c.Param("token")).First(&link).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "recovery link not found"})
		return
	}
	if link.UsedAt != nil {
		c.JSON(http.StatusGone, gin.H{"error": "recovery link already used"})
		return
	}
	if time.Now().After(link.ExpiresAt) {
		c.JSON(http.StatusGone, gin.H{"error": "recovery link expired"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"valid":      true,
		"expires_at": link.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// Consume — public: POST /api/v1/recovery/:token. Burns the link, revokes
// every passkey on the account, and logs the holder in (vtafarm_user cookie)
// so they can immediately register a fresh passkey. If the passkey ceremony
// then fails, the 24h cookie still lets them add one from Settings; a truly
// stranded user just needs the admin to issue another link.
func (h *RecoveryHandler) Consume(c *gin.Context) {
	var link model.RecoveryLink
	if err := h.db.Where("token = ?", c.Param("token")).First(&link).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "recovery link not found"})
		return
	}
	if link.UsedAt != nil {
		c.JSON(http.StatusGone, gin.H{"error": "recovery link already used"})
		return
	}
	if time.Now().After(link.ExpiresAt) {
		c.JSON(http.StatusGone, gin.H{"error": "recovery link expired"})
		return
	}

	var user model.User
	if err := h.db.First(&user, link.UserID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
		return
	}

	err := h.db.Transaction(func(tx *gorm.DB) error {
		// Compare-and-set guards against two concurrent consumes of one link.
		res := tx.Model(&model.RecoveryLink{}).
			Where("id = ? AND used_at IS NULL", link.ID).
			Update("used_at", time.Now())
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return tx.Where("user_id = ?", user.ID).Delete(&model.UserPasskey{}).Error
	})
	if err == gorm.ErrRecordNotFound {
		c.JSON(http.StatusGone, gin.H{"error": "recovery link already used"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not consume recovery link"})
		return
	}

	jwtToken, err := middleware.GenerateToken(user.ID, model.RoleUser, h.jwtSecret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate token"})
		return
	}
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(middleware.CookieUser, jwtToken, cookieMaxAge, "/", "", h.cookieSecure, true)

	c.JSON(http.StatusOK, gin.H{
		"id":        user.ID,
		"unique_id": user.UniqueId,
		"token":     jwtToken,
	})
}
