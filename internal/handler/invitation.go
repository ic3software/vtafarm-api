package handler

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/middleware"
	"github.com/ic3software/cipherportal-api/internal/model"
)

const defaultInvitationTTL = 24 * time.Hour

type InvitationHandler struct {
	db *gorm.DB
}

func NewInvitationHandler(db *gorm.DB) *InvitationHandler {
	return &InvitationHandler{db: db}
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

type registerViaInvitationRequest struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required,min=8"`
}

// Register — public: self-register using an invitation token.
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

	var req registerViaInvitationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not hash password"})
		return
	}

	user := model.User{
		Email:    req.Email,
		Password: string(hash),
	}
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
		if strings.Contains(createErr.Error(), "idx_users_email") {
			c.JSON(http.StatusConflict, gin.H{"error": "email already exists"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create user"})
		}
		return
	}

	now := time.Now()
	if err := h.db.Model(&inv).Update("used_at", now).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to mark invitation as used"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":        user.ID,
		"unique_id": user.UniqueId,
		"email":     user.Email,
	})
}
