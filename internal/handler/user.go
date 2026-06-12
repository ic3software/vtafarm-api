package handler

import (
	"crypto/rand"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

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
		ID        uint   `json:"id"`
		UniqueId  string `json:"unique_id"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	result := make([]userItem, len(users))
	for i, u := range users {
		result[i] = userItem{
			ID:        u.ID,
			UniqueId:  u.UniqueId,
			CreatedAt: u.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			UpdatedAt: u.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
	}
	c.JSON(http.StatusOK, result)
}
