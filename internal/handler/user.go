package handler

import (
	"crypto/rand"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
)

const uniqueIdAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func generateUniqueId() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	result := make([]byte, 8)
	for i, byt := range b {
		result[i] = uniqueIdAlphabet[int(byt)%len(uniqueIdAlphabet)]
	}
	return string(result)
}

type UserHandler struct {
	db *gorm.DB
}

func NewUserHandler(db *gorm.DB) *UserHandler {
	return &UserHandler{db: db}
}

func (h *UserHandler) List(c *gin.Context) {
	var users []model.User
	if err := h.db.Order("id asc").Find(&users).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch users"})
		return
	}

	type userItem struct {
		ID         uint    `json:"id"`
		UniqueId   string  `json:"unique_id"`
		Email      *string `json:"email"` // null for pre-email and admin-invited accounts
		BetaAccess bool    `json:"beta_access"`
		// System is true for the account that owns the platform stack. It is
		// not a login — no passkey, no email — so the UI should not offer it
		// beta access, a recovery link, or anything else meant for a person.
		System    bool   `json:"system"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	result := make([]userItem, len(users))
	for i, u := range users {
		result[i] = userItem{
			ID:         u.ID,
			UniqueId:   u.UniqueId,
			Email:      u.Email,
			BetaAccess: u.BetaAccess,
			System:     u.UniqueId == systemAccountUniqueID,
			CreatedAt:  u.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			UpdatedAt:  u.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
	}
	c.JSON(http.StatusOK, result)
}

// Me — GET /api/v1/user/me. Lets the frontend know the caller's own
// beta_access without waiting for a re-login (the JWT itself doesn't carry
// it, since an admin can flip it at any time).
func (h *UserHandler) Me(c *gin.Context) {
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var user model.User
	if err := h.db.First(&user, userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":          user.UniqueId,
		"email":       user.Email,
		"beta_access": user.BetaAccess,
		"created_at":  user.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	})
}

type setBetaAccessRequest struct {
	BetaAccess bool `json:"beta_access"`
}

// SetBetaAccess — PUT /api/v1/admin/users/:id/beta-access (admin only). Grants
// or revokes a user's access to beta features (currently: full_stack setup mode).
func (h *UserHandler) SetBetaAccess(c *gin.Context) {
	publicID := c.Param("id")

	var req setBetaAccessRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var user model.User
	if err := h.db.Where("unique_id = ?", publicID).First(&user).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	if err := h.db.Model(&user).Update("beta_access", req.BetaAccess).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update user"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":          user.UniqueId,
		"beta_access": req.BetaAccess,
	})
}
