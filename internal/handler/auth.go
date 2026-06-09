package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/middleware"
	"github.com/ic3software/cipherportal-api/internal/model"
)

const cookieMaxAge = 24 * 60 * 60 // 24 hours in seconds

type AuthHandler struct {
	db           *gorm.DB
	jwtSecret    string
	cookieDomain string
	cookieSecure bool
}

func NewAuthHandler(db *gorm.DB, jwtSecret, cookieDomain string, cookieSecure bool) *AuthHandler {
	return &AuthHandler{db: db, jwtSecret: jwtSecret, cookieDomain: cookieDomain, cookieSecure: cookieSecure}
}

type loginRequest struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

func (h *AuthHandler) AdminLogin(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var admin model.Admin
	if h.db.Where("email = ?", req.Email).First(&admin).Error != nil ||
		bcrypt.CompareHashAndPassword([]byte(admin.Password), []byte(req.Password)) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	token, err := middleware.GenerateToken(admin.ID, model.RoleAdmin, h.jwtSecret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate token"})
		return
	}

	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(middleware.CookieAdmin, token, cookieMaxAge, "/", h.cookieDomain, h.cookieSecure, true)

	// token is included for non-browser clients (curl, Postman, Scalar).
	// Browser frontend must rely on the httpOnly cookie and ignore this field.
	c.JSON(http.StatusOK, gin.H{
		"token": token,
		"user":  gin.H{"id": admin.ID, "email": admin.Email, "role": model.RoleAdmin},
	})
}

func (h *AuthHandler) AdminLogout(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(middleware.CookieAdmin, "", -1, "/", h.cookieDomain, h.cookieSecure, true)
	c.Status(http.StatusNoContent)
}

func (h *AuthHandler) UserLogin(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var user model.User
	if h.db.Where("email = ?", req.Email).First(&user).Error != nil ||
		bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	token, err := middleware.GenerateToken(user.ID, model.RoleUser, h.jwtSecret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate token"})
		return
	}

	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(middleware.CookieUser, token, cookieMaxAge, "/", h.cookieDomain, h.cookieSecure, true)

	// token is included for non-browser clients (curl, Postman, Scalar).
	// Browser frontend must rely on the httpOnly cookie and ignore this field.
	c.JSON(http.StatusOK, gin.H{
		"token": token,
		"user":  gin.H{"id": user.ID, "email": user.Email, "role": model.RoleUser},
	})
}

func (h *AuthHandler) UserLogout(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(middleware.CookieUser, "", -1, "/", h.cookieDomain, h.cookieSecure, true)
	c.Status(http.StatusNoContent)
}
