package router

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/cloudflare"
	"github.com/ic3software/cipherportal-api/internal/handler"
	"github.com/ic3software/cipherportal-api/internal/middleware"
	"github.com/ic3software/cipherportal-api/internal/model"
)

func Setup(db *gorm.DB, cfClient *cloudflare.Client, appEnv, ingressIP, jwtSecret string) *gin.Engine {
	r := gin.Default()

	r.GET("/health", handler.Health)

	// Public DID log hosting — no auth, served at the path did:webvh resolvers expect.
	sh := handler.NewSetupHandler(db, cfClient, appEnv, ingressIP)
	r.GET("/did/:subdomain/did.jsonl", sh.ServeDidLog)

	v1 := r.Group("/api/v1")

	// Public
	ah := handler.NewAuthHandler(db, jwtSecret)
	v1.POST("/auth/login", ah.Login)

	// Auth required
	auth := v1.Group("", middleware.AuthRequired(jwtSecret))
	{
		// Admin only
		uh := handler.NewUserHandler(db)
		auth.POST("/users", middleware.RequireRole(model.RoleAdmin), uh.Create)

		// User only
		userOnly := auth.Group("", middleware.RequireRole(model.RoleUser))
		userOnly.POST("/setup/validate", sh.Validate)
		userOnly.POST("/setup", sh.Create)
		userOnly.DELETE("/setup/:id", sh.Delete)
	}

	return r
}
