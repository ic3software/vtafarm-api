package router

import (
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/apidocs"
	"github.com/ic3software/cipherportal-api/internal/cloudflare"
	"github.com/ic3software/cipherportal-api/internal/config"
	"github.com/ic3software/cipherportal-api/internal/didhosting"
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
	dhClient *didhosting.Client,
	cfg *config.Config,
) *gin.Engine {
	r := gin.Default()

	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"https://cipher.ic3.dev", "http://localhost:5173", "http://localhost:5174", "http://localhost:5175"},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Authorization", "Content-Type"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	r.GET("/health", handler.Health)

	if cfg.AppEnv != "production" {
		r.GET("/openapi.yaml", apidocs.ServeSpec)
		r.GET("/docs", apidocs.ServeUI)
	}

	sh := handler.NewSetupHandler(
		db, cfClient, cfg.AppEnv, cfg.ClusterIngressIP, cfg.ClusterDomain,
		cfg.MediatorDid, cfg.DidHosting.ServerUrl, dhClient, k8sClient, orch, ghcrClient,
	)

	v1 := r.Group("/api/v1")

	// Public auth
	ah := handler.NewAuthHandler(db, cfg.JWTSecret, cfg.CookieDomain, cfg.CookieSecure())
	v1.POST("/auth/admin/login", ah.AdminLogin)
	v1.POST("/auth/admin/logout", ah.AdminLogout)
	v1.POST("/auth/user/login", ah.UserLogin)
	v1.POST("/auth/user/logout", ah.UserLogout)

	// Admin routes — cookie: cipher_admin
	adminAuth := v1.Group("",
		middleware.AuthRequired(cfg.JWTSecret, middleware.CookieAdmin),
		middleware.RequireRole(model.RoleAdmin),
	)
	uh := handler.NewUserHandler(db)
	ih := handler.NewInvitationHandler(db)
	{
		adminH := handler.NewAdminHandler(db)
		adminAuth.GET("/admins", adminH.List)
		adminAuth.POST("/admins", adminH.Create)
		adminAuth.GET("/users", uh.List)
		adminAuth.POST("/users", uh.Create)
		adminAuth.PUT("/admin/password", adminH.ChangeOwnPassword)
		adminAuth.PUT("/users/:id/password", adminH.ChangeUserPassword)
		adminAuth.POST("/invitations", ih.Create)
		adminAuth.GET("/invitations", ih.List)
	}

	// Public invitation routes (no auth required)
	v1.GET("/invitations/:token", ih.Validate)
	v1.POST("/invitations/:token/register", ih.Register)

	// User routes — cookie: cipher_user
	userAuth := v1.Group("",
		middleware.AuthRequired(cfg.JWTSecret, middleware.CookieUser),
		middleware.RequireRole(model.RoleUser),
	)
	{
		userAuth.PUT("/user/password", uh.ChangeOwnPassword)
		userAuth.POST("/setup/validate", sh.Validate)
		userAuth.GET("/setup/images", sh.Images)
		userAuth.GET("/setup", sh.List)
		userAuth.POST("/setup", sh.Create)
		userAuth.GET("/setup/:id", sh.Get)
		userAuth.DELETE("/setup/:id", sh.Delete)
		userAuth.GET("/setup/:id/logs", sh.Logs)
		userAuth.POST("/setup/:id/admin", sh.ProvisionAdmin)
	}

	return r
}
