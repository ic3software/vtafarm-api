package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/middleware"
)

const cookieMaxAge = 24 * 60 * 60

type AuthHandler struct {
	cookieDomain string
	cookieSecure bool
}

func NewAuthHandler(cookieDomain string, cookieSecure bool) *AuthHandler {
	return &AuthHandler{cookieDomain: cookieDomain, cookieSecure: cookieSecure}
}

func (h *AuthHandler) AdminLogout(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(middleware.CookieAdmin, "", -1, "/", h.cookieDomain, h.cookieSecure, true)
	c.Status(http.StatusNoContent)
}

func (h *AuthHandler) UserLogout(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(middleware.CookieUser, "", -1, "/", h.cookieDomain, h.cookieSecure, true)
	c.Status(http.StatusNoContent)
}
