package router

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/apidocs"
	"github.com/ic3software/cipherportal-api/internal/cloudflare"
	"github.com/ic3software/cipherportal-api/internal/ghcr"
	"github.com/ic3software/cipherportal-api/internal/handler"
	"github.com/ic3software/cipherportal-api/internal/k8s"
	"github.com/ic3software/cipherportal-api/internal/middleware"
	"github.com/ic3software/cipherportal-api/internal/model"
	"github.com/ic3software/cipherportal-api/internal/setup"
)

func Setup(
	db *gorm.DB,
	cfClient *cloudflare.Client,
	k8sClient *k8s.Client,
	orch *setup.Orchestrator,
	ghcrClient *ghcr.Client,
	appEnv, ingressIP, clusterDomain, didHostingBase, jwtSecret string,
) *gin.Engine {
	r := gin.Default()

	r.GET("/health", handler.Health)

	if appEnv != "production" {
		r.GET("/openapi.yaml", apidocs.ServeSpec)
		r.GET("/docs", apidocs.ServeUI)
	}

	sh := handler.NewSetupHandler(db, cfClient, appEnv, ingressIP, clusterDomain, didHostingBase, k8sClient, orch, ghcrClient)

	v1 := r.Group("/api/v1")

	// Public
	ah := handler.NewAuthHandler(db, jwtSecret)
	v1.POST("/auth/admin/login", ah.AdminLogin)
	v1.POST("/auth/user/login", ah.UserLogin)

	// Auth required
	auth := v1.Group("", middleware.AuthRequired(jwtSecret))
	{
		// Admin only
		uh := handler.NewUserHandler(db)
		auth.POST("/users", middleware.RequireRole(model.RoleAdmin), uh.Create)

		// User only
		userOnly := auth.Group("", middleware.RequireRole(model.RoleUser))
		userOnly.POST("/setup/validate", sh.Validate)
		userOnly.GET("/setup/images", sh.Images)
		userOnly.GET("/setup", sh.List)
		userOnly.POST("/setup", sh.Create)
		userOnly.GET("/setup/:id", sh.Get)
		userOnly.DELETE("/setup/:id", sh.Delete)
		userOnly.GET("/setup/:id/logs", sh.Logs)
		userOnly.POST("/setup/:id/admin", sh.ProvisionAdmin)
	}

	return r
}
