package handler

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/middleware"
	"github.com/ic3software/cipherportal-api/internal/model"
)

const defaultInvitationTTL = 24 * time.Hour

type InvitationHandler struct {
	db           *gorm.DB
	jwtSecret    string
	cookieDomain string
	cookieSecure bool
}

func NewInvitationHandler(db *gorm.DB, jwtSecret, cookieDomain string, cookieSecure bool) *InvitationHandler {
	return &InvitationHandler{db: db, jwtSecret: jwtSecret, cookieDomain: cookieDomain, cookieSecure: cookieSecure}
}

func generateInviteToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Create — admin creates an invitation link.
func (h *InvitationHandler) Create(c *gin.Context) {
	adminID, _ := c.Get(middleware.ContextUserID)

	token, err := generateInviteToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate token"})
		return
	}

	inv := model.InvitationLink{
		Token:     token,
		AdminID:   adminID.(uint),
		ExpiresAt: time.Now().Add(defaultInvitationTTL),
	}
	if err := h.db.Create(&inv).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create invitation"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":         inv.ID,
		"token":      inv.Token,
		"expires_at": inv.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// List — admin lists all invitation links.
func (h *InvitationHandler) List(c *gin.Context) {
	var invs []model.InvitationLink
	if err := h.db.Order("id desc").Find(&invs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch invitations"})
		return
	}

	type item struct {
		ID        uint    `json:"id"`
		Token     string  `json:"token"`
		AdminID   uint    `json:"admin_id"`
		ExpiresAt string  `json:"expires_at"`
		UsedAt    *string `json:"used_at"`
		CreatedAt string  `json:"created_at"`
	}
	result := make([]item, len(invs))
	for i, inv := range invs {
		it := item{
			ID:        inv.ID,
			Token:     inv.Token,
			AdminID:   inv.AdminID,
			ExpiresAt: inv.ExpiresAt.UTC().Format(time.RFC3339),
			CreatedAt: inv.CreatedAt.UTC().Format(time.RFC3339),
		}
		if inv.UsedAt != nil {
			s := inv.UsedAt.UTC().Format(time.RFC3339)
			it.UsedAt = &s
		}
		result[i] = it
	}
	c.JSON(http.StatusOK, result)
}

// Validate — public: check whether an invitation token is still valid.
func (h *InvitationHandler) Validate(c *gin.Context) {
	token := c.Param("token")

	var inv model.InvitationLink
	if err := h.db.Where("token = ?", token).First(&inv).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "invitation not found"})
		return
	}
	if inv.UsedAt != nil {
		c.JSON(http.StatusGone, gin.H{"error": "invitation already used"})
		return
	}
	if time.Now().After(inv.ExpiresAt) {
		c.JSON(http.StatusGone, gin.H{"error": "invitation expired"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"valid":      true,
		"expires_at": inv.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// Register — public: self-register using an invitation token. No request body needed.
// On success the user is automatically logged in via the cipher_user cookie
// so they can proceed to set up their passkey without a separate login step.
func (h *InvitationHandler) Register(c *gin.Context) {
	token := c.Param("token")

	var inv model.InvitationLink
	if err := h.db.Where("token = ?", token).First(&inv).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "invitation not found"})
		return
	}
	if inv.UsedAt != nil {
		c.JSON(http.StatusGone, gin.H{"error": "invitation already used"})
		return
	}
	if time.Now().After(inv.ExpiresAt) {
		c.JSON(http.StatusGone, gin.H{"error": "invitation expired"})
		return
	}

	var user model.User
	const maxAttempts = 5
	var createErr error
	for range maxAttempts {
		user.UniqueId = generateUniqueId()
		createErr = h.db.Create(&user).Error
		if createErr == nil {
			break
		}
		if !strings.Contains(createErr.Error(), "idx_users_unique_id") {
			break
		}
	}
	if createErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create user"})
		return
	}

	now := time.Now()
	if err := h.db.Model(&inv).Update("used_at", now).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to mark invitation as used"})
		return
	}

	// Auto-login: set cipher_user cookie so the user can immediately register their passkey.
	jwtToken, err := middleware.GenerateToken(user.ID, model.RoleUser, h.jwtSecret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate token"})
		return
	}
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(middleware.CookieUser, jwtToken, cookieMaxAge, "/", h.cookieDomain, h.cookieSecure, true)

	c.JSON(http.StatusCreated, gin.H{
		"id":        user.ID,
		"unique_id": user.UniqueId,
		"token":     jwtToken,
	})
}
