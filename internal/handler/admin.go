package handler

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/model"
)

type AdminHandler struct {
	db *gorm.DB
}

func NewAdminHandler(db *gorm.DB) *AdminHandler {
	return &AdminHandler{db: db}
}

func (h *AdminHandler) List(c *gin.Context) {
	var admins []model.Admin
	if err := h.db.Order("id asc").Find(&admins).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch admins"})
		return
	}

	type adminItem struct {
		ID        uint   `json:"id"`
		UniqueId  string `json:"unique_id"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	result := make([]adminItem, len(admins))
	for i, a := range admins {
		result[i] = adminItem{
			ID:        a.ID,
			UniqueId:  a.UniqueId,
			CreatedAt: a.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			UpdatedAt: a.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
	}
	c.JSON(http.StatusOK, result)
}

// Create — generate a 24h single-use enrollment token.
// The admin account is created later when the token is consumed via POST /admin/enroll/:token.
func (h *AdminHandler) Create(c *gin.Context) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate enrollment token"})
		return
	}
	enrollToken := hex.EncodeToString(b)
	expires := time.Now().Add(24 * time.Hour)
	tok := model.AdminEnrollmentToken{
		Token:     enrollToken,
		ExpiresAt: expires,
	}
	if err := h.db.Create(&tok).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create enrollment token"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"enrollment_token":   enrollToken,
		"enrollment_expires": expires.UTC().Format(time.RFC3339),
	})
}
