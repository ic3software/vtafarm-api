package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
)

type AdminEnrollHandler struct {
	db           *gorm.DB
	jwtSecret    string
	cookieSecure bool
}

func NewAdminEnrollHandler(db *gorm.DB, jwtSecret string, cookieSecure bool) *AdminEnrollHandler {
	return &AdminEnrollHandler{db: db, jwtSecret: jwtSecret, cookieSecure: cookieSecure}
}

// Validate — GET /api/v1/admin/enroll/:token
func (h *AdminEnrollHandler) Validate(c *gin.Context) {
	token := c.Param("token")
	var t model.AdminEnrollmentToken
	if err := h.db.Where("token = ?", token).First(&t).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "enrollment token not found"})
		return
	}
	if t.UsedAt != nil {
		c.JSON(http.StatusGone, gin.H{"error": "enrollment token already used"})
		return
	}
	if time.Now().After(t.ExpiresAt) {
		c.JSON(http.StatusGone, gin.H{"error": "enrollment token expired"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"valid": true, "expires_at": t.ExpiresAt.UTC().Format(time.RFC3339)})
}

// Enroll — POST /api/v1/admin/enroll/:token
// Consumes the enrollment token, creates the admin account, and sets the vtafarm_admin cookie
// so the admin can immediately register their passkey.
func (h *AdminEnrollHandler) Enroll(c *gin.Context) {
	token := c.Param("token")
	var t model.AdminEnrollmentToken
	if err := h.db.Where("token = ?", token).First(&t).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "enrollment token not found"})
		return
	}
	if t.UsedAt != nil {
		c.JSON(http.StatusGone, gin.H{"error": "enrollment token already used"})
		return
	}
	if time.Now().After(t.ExpiresAt) {
		c.JSON(http.StatusGone, gin.H{"error": "enrollment token expired"})
		return
	}

	now := time.Now()
	if err := h.db.Model(&t).Update("used_at", now).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not consume token"})
		return
	}

	var admin model.Admin
	const maxAttempts = 5
	var createErr error
	for range maxAttempts {
		admin.UniqueId = generateUniqueId()
		createErr = h.db.Create(&admin).Error
		if createErr == nil {
			break
		}
		if !strings.Contains(createErr.Error(), "idx_admins_unique_id") {
			break
		}
	}
	if createErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create admin account"})
		return
	}

	jwtToken, err := middleware.GenerateToken(admin.ID, model.RoleAdmin, h.jwtSecret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate token"})
		return
	}

	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(middleware.CookieAdmin, jwtToken, cookieMaxAge, "/", "", h.cookieSecure, true)

	c.JSON(http.StatusOK, gin.H{
		"id":        admin.ID,
		"unique_id": admin.UniqueId,
		"token":     jwtToken,
	})
}
