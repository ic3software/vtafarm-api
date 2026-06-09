package handler

import (
	"crypto/rand"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/middleware"
	"github.com/ic3software/cipherportal-api/internal/model"
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

type createUserRequest struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required,min=8"`
}

func (h *UserHandler) Create(c *gin.Context) {
	var req createUserRequest
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

	c.JSON(http.StatusCreated, gin.H{
		"id":        user.ID,
		"unique_id": user.UniqueId,
		"email":     user.Email,
	})
}

type changeUserOwnPasswordRequest struct {
	CurrentPassword string `json:"current_password" binding:"required"`
	NewPassword     string `json:"new_password"     binding:"required,min=8"`
}

func (h *UserHandler) List(c *gin.Context) {
	var users []model.User
	if err := h.db.Order("id asc").Find(&users).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch users"})
		return
	}

	type userItem struct {
		ID        uint   `json:"id"`
		UniqueId  string `json:"unique_id"`
		Email     string `json:"email"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	result := make([]userItem, len(users))
	for i, u := range users {
		result[i] = userItem{
			ID:        u.ID,
			UniqueId:  u.UniqueId,
			Email:     u.Email,
			CreatedAt: u.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			UpdatedAt: u.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
	}
	c.JSON(http.StatusOK, result)
}

func (h *UserHandler) ChangeOwnPassword(c *gin.Context) {
	var req changeUserOwnPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userID, _ := c.Get(middleware.ContextUserID)

	var user model.User
	if err := h.db.First(&user, userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.CurrentPassword)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "current password is incorrect"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not hash password"})
		return
	}

	if err := h.db.Model(&user).Update("password", string(hash)).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not update password"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "password updated"})
}
