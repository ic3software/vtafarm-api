package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/walletauth"
	"gorm.io/gorm"
)

type WalletLoginBindingHandler struct {
	db *gorm.DB
}

func NewWalletLoginBindingHandler(db *gorm.DB) *WalletLoginBindingHandler {
	return &WalletLoginBindingHandler{db: db}
}

func (h *WalletLoginBindingHandler) List(c *gin.Context) {
	userID := c.MustGet(middleware.ContextUserID).(uint)
	var bindings []model.WalletLoginBinding
	if err := h.db.WithContext(c.Request.Context()).Where("user_id = ? AND revoked_at IS NULL", userID).Order("created_at ASC").Find(&bindings).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch wallet login identities"})
		return
	}
	items := make([]gin.H, 0, len(bindings))
	for _, binding := range bindings {
		items = append(items, gin.H{"id": binding.ID, "login_did": binding.LoginDID, "created_at": binding.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")})
	}
	c.JSON(http.StatusOK, items)
}

func (h *WalletLoginBindingHandler) Revoke(c *gin.Context) {
	userID := c.MustGet(middleware.ContextUserID).(uint)
	bindingID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid wallet login identity"})
		return
	}
	if err := walletauth.RevokeBinding(c.Request.Context(), h.db, userID, bindingID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "wallet login identity not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not revoke wallet login identity"})
		return
	}
	c.Status(http.StatusNoContent)
}
