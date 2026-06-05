package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/middleware"
	"github.com/ic3software/cipherportal-api/internal/model"
)

type AuthHandler struct {
	db        *gorm.DB
	jwtSecret string
}

func NewAuthHandler(db *gorm.DB, jwtSecret string) *AuthHandler {
	return &AuthHandler{db: db, jwtSecret: jwtSecret}
}

type loginRequest struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

func (h *AuthHandler) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Check admins table first.
	var admin model.Admin
	if h.db.Where("email = ?", req.Email).First(&admin).Error == nil {
		if bcrypt.CompareHashAndPassword([]byte(admin.Password), []byte(req.Password)) == nil {
			token, err := middleware.GenerateToken(admin.ID, model.RoleAdmin, h.jwtSecret)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate token"})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"token": token,
				"user": gin.H{
					"id":       admin.ID,
					"email":    admin.Email,
					"username": admin.Username,
					"role":     model.RoleAdmin,
				},
			})
			return
		}
	}

	// Check users table.
	var user model.User
	if h.db.Where("email = ?", req.Email).First(&user).Error == nil {
		if bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)) == nil {
			token, err := middleware.GenerateToken(user.ID, model.RoleUser, h.jwtSecret)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate token"})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"token": token,
				"user": gin.H{
					"id":       user.ID,
					"email":    user.Email,
					"username": user.Username,
					"role":     model.RoleUser,
				},
			})
			return
		}
	}

	c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
}
