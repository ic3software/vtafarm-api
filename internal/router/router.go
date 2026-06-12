package router

import (
	"log"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/webauthn"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/apidocs"
	"github.com/ic3software/vtafarm-api/internal/cloudflare"
	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/didhosting"
	"github.com/ic3software/vtafarm-api/internal/ghcr"
	"github.com/ic3software/vtafarm-api/internal/handler"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/passkey"
	"github.com/ic3software/vtafarm-api/internal/setup"
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
		AllowOrigins:     []string{"https://vtafarm.firstperson.dev", "http://localhost:5173", "http://localhost:5174", "http://localhost:5175"},
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

	// Auth — logout only (login is via passkey)
	ah := handler.NewAuthHandler(cfg.CookieSecure())
	v1.POST("/auth/admin/logout", ah.AdminLogout)
	v1.POST("/auth/user/logout", ah.UserLogout)

	// Passkey login — admin and user have separate endpoints for clear docs separation.
	// Both use the discoverable flow; the complete endpoint validates the decoded role.
	wa, err := webauthn.New(&webauthn.Config{
		RPDisplayName: cfg.WebAuthn.RPDisplayName,
		RPID:          cfg.WebAuthn.RPID,
		RPOrigins:     cfg.WebAuthn.RPOrigins,
	})
	if err != nil {
		log.Fatalf("webauthn init: %v", err)
	}
	passkeyStore := passkey.NewSessionStore()
	pkh := handler.NewPasskeyHandler(db, wa, passkeyStore, cfg.JWTSecret, cfg.CookieSecure())
	v1.POST("/auth/admin/passkey/begin", pkh.AdminLoginBegin)
	v1.POST("/auth/admin/passkey/complete", pkh.AdminLoginComplete)
	v1.POST("/auth/user/passkey/begin", pkh.UserLoginBegin)
	v1.POST("/auth/user/passkey/complete", pkh.UserLoginComplete)

	// Admin enrollment (public — no auth required)
	aeh := handler.NewAdminEnrollHandler(db, cfg.JWTSecret, cfg.CookieSecure())
	v1.GET("/admin/enroll/:token", aeh.Validate)
	v1.POST("/admin/enroll/:token", aeh.Enroll)

	// Admin routes — cookie: vtafarm_admin
	adminAuth := v1.Group("",
		middleware.AuthRequired(cfg.JWTSecret, middleware.CookieAdmin),
		middleware.RequireRole(model.RoleAdmin),
	)
	uh := handler.NewUserHandler(db)
	ih := handler.NewInvitationHandler(db, cfg.JWTSecret, cfg.CookieSecure())
	{
		adminH := handler.NewAdminHandler(db)
		adminAuth.GET("/admins", adminH.List)
		adminAuth.POST("/admins", adminH.Create)
		adminAuth.GET("/users", uh.List)
		adminAuth.POST("/admin/passkeys/register/begin", pkh.RegisterBegin)
		adminAuth.POST("/admin/passkeys/register/complete", pkh.RegisterComplete)
		adminAuth.GET("/admin/passkeys", pkh.List)
		adminAuth.DELETE("/admin/passkeys/:id", pkh.Delete)
		adminAuth.POST("/invitations", ih.Create)
		adminAuth.GET("/invitations", ih.List)
	}

	// Public invitation routes (no auth required)
	v1.GET("/invitations/:token", ih.Validate)
	v1.POST("/invitations/:token/register", ih.Register)

	// User routes — cookie: vtafarm_user
	userAuth := v1.Group("",
		middleware.AuthRequired(cfg.JWTSecret, middleware.CookieUser),
		middleware.RequireRole(model.RoleUser),
	)
	{
		userAuth.POST("/user/passkeys/register/begin", pkh.RegisterBegin)
		userAuth.POST("/user/passkeys/register/complete", pkh.RegisterComplete)
		userAuth.GET("/user/passkeys", pkh.List)
		userAuth.DELETE("/user/passkeys/:id", pkh.Delete)
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
