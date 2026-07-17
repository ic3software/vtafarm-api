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
	"github.com/ic3software/vtafarm-api/internal/upgrade"
)

func Setup(
	db *gorm.DB,
	cfClient *cloudflare.Client,
	k8sClient *k8s.Client,
	orch *setup.Orchestrator,
	upgradeRunner *upgrade.Runner,
	ghcrClient *ghcr.Client,
	mediatorGhcrClient *ghcr.Client,
	didsGhcrClient *ghcr.Client,
	vtcGhcrClient *ghcr.Client,
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
		mediatorGhcrClient, didsGhcrClient, vtcGhcrClient,
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

	// Image upgrades — shared by the admin batch routes and the user
	// self-service routes below; background runner, DB-backed queue.
	uph := handler.NewUpgradeHandler(db, upgradeRunner, ghcrClient, mediatorGhcrClient, didsGhcrClient, vtcGhcrClient)

	// Admin routes — cookie: vtafarm_admin
	adminAuth := v1.Group("",
		middleware.AuthRequired(cfg.JWTSecret, middleware.CookieAdmin),
		middleware.RequireRole(model.RoleAdmin),
	)
	uh := handler.NewUserHandler(db)
	ih := handler.NewInvitationHandler(db, cfg.JWTSecret, cfg.CookieSecure())
	rh := handler.NewRecoveryHandler(db, cfg.JWTSecret, cfg.CookieSecure())
	{
		adminH := handler.NewAdminHandler(db)
		adminAuth.GET("/admin/admins", adminH.List)
		adminAuth.POST("/admin/admins", adminH.Create)
		adminAuth.GET("/admin/users", uh.List)
		adminAuth.PUT("/admin/users/:id/beta-access", uh.SetBetaAccess)
		// Lost-passkey recovery: issues a 1h single-use login link the admin
		// delivers out of band; consuming it is the public /recovery route.
		adminAuth.POST("/admin/users/:id/recovery-link", rh.Create)
		adminAuth.POST("/admin/passkeys/register/begin", pkh.RegisterBegin)
		adminAuth.POST("/admin/passkeys/register/complete", pkh.RegisterComplete)
		adminAuth.GET("/admin/passkeys", pkh.List)
		adminAuth.DELETE("/admin/passkeys/:id", pkh.Delete)
		adminAuth.POST("/admin/invitations", ih.Create)
		adminAuth.GET("/admin/invitations", ih.List)
		adminAuth.GET("/admin/setup-sessions", sh.AdminListSessions)
		// Same handler as the user-facing GET /setup/images — admins need the
		// tag list too (session upgrades), but sit behind a different cookie.
		adminAuth.GET("/admin/setup/images", sh.Images)

		// Batch image upgrades — background runner, DB-backed queue.
		adminAuth.POST("/admin/upgrades", uph.Create)
		adminAuth.GET("/admin/upgrades", uph.List)
		adminAuth.GET("/admin/upgrades/:id", uph.Get)
		adminAuth.POST("/admin/upgrades/:id/cancel", uph.Cancel)
		adminAuth.POST("/admin/upgrades/:id/resume", uph.Resume)
	}

	// Public invitation routes (no auth required)
	v1.GET("/invitations/:token", ih.Validate)
	v1.POST("/invitations/:token/register", ih.Register)

	// Public email signup — visitors create an account directly from the home
	// page (no admin approval, no email sent). Rate-limited: it both creates
	// accounts and issues login cookies.
	sgh := handler.NewSignupHandler(db, cfg.JWTSecret, cfg.CookieSecure())
	v1.POST("/signup", middleware.RateLimit(10, time.Minute), sgh.Signup)

	// Account recovery — an admin issues a short-lived login link for a user
	// who lost their passkey (adminAuth route above); the holder consumes it
	// here. Consuming revokes the account's passkeys and sets a login cookie.
	v1.GET("/recovery/:token", rh.Validate)
	v1.POST("/recovery/:token", middleware.RateLimit(10, time.Minute), rh.Consume)

	// User routes — cookie: vtafarm_user
	userAuth := v1.Group("",
		middleware.AuthRequired(cfg.JWTSecret, middleware.CookieUser),
		middleware.RequireRole(model.RoleUser),
	)
	{
		userAuth.GET("/user/me", uh.Me)
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
		// Self-service image upgrade/downgrade — a user can only ever change
		// their own session (looked up by unique_id AND user_id).
		userAuth.POST("/setup/:id/upgrade", uph.CreateForSession)
		userAuth.GET("/setup/:id/upgrade", uph.GetForSession)
		userAuth.POST("/setup/:id/admin", sh.ProvisionAdmin)
		userAuth.POST("/setup/:id/dids/reissue-enroll", sh.ReissueDidsEnroll)
		userAuth.POST("/setup/:id/dids/enroll-ack", sh.AckDidsEnroll)
		userAuth.POST("/setup/:id/vtc/reissue-install", sh.ReissueVtcInstall)
		userAuth.POST("/setup/:id/vtc/install-ack", sh.AckVtcInstall)
	}

	return r
}
