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
	dhFactory *didhosting.Factory,
	cfg *config.Config,
) *gin.Engine {
	r := gin.Default()

	// The Vite dev ports are a local convenience no deployment would configure.
	allowOrigins := append(
		[]string{"http://localhost:5173", "http://localhost:5174", "http://localhost:5175"},
		cfg.CORSAllowedOrigins...,
	)

	r.Use(cors.New(cors.Config{
		AllowOrigins:     allowOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Authorization", "Content-Type"},
		ExposeHeaders:    []string{"Content-Length", "Content-Disposition"},
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
		dhFactory, k8sClient, orch, ghcrClient,
		mediatorGhcrClient, didsGhcrClient, vtcGhcrClient,
		cfg.MaxStackConnections,
	)

	v1 := r.Group("/api/v1")

	// Monitor endpoints — polled by an external uptime service (UptimeRobot):
	// healthy → 200, anything wrong → 503. Gated by MONITOR_TOKEN (?token=)
	// since the poller can't log in; 404 until the token is configured.
	mh := handler.NewMonitorHandler(db, k8sClient, handler.MonitorConfig{
		Token:           cfg.Monitor.Token,
		CPUPercent:      cfg.Monitor.CPUPercent,
		MemPercent:      cfg.Monitor.MemPercent,
		StoragePercent:  cfg.Monitor.StoragePercent,
		RestartWindow:   time.Duration(cfg.Monitor.RestartWindowMin) * time.Minute,
		PendingGrace:    time.Duration(cfg.Monitor.PendingGraceMin) * time.Minute,
		ExtraNamespaces: cfg.Monitor.ExtraNamespaces,
		VaultAddr:       cfg.Vault.Addr,
		VaultSkipVerify: cfg.Vault.SkipVerify,
	})
	monitor := v1.Group("/monitor", mh.TokenRequired)
	{
		monitor.GET("/health", mh.Health)
		monitor.GET("/workloads", mh.Workloads)
		monitor.GET("/capacity", mh.Capacity)
	}

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
		adminAuth.POST("/admin/load-tests", sh.AdminCreateLoadTest)
		adminAuth.GET("/admin/load-tests", sh.AdminListLoadTests)
		adminAuth.POST("/admin/load-tests/:id/check", sh.AdminCheckLoadTest)
		adminAuth.DELETE("/admin/load-tests/:id", sh.AdminDeleteLoadTest)
		// Same teardown as the user-facing DELETE /setup/:id, but reaches any
		// user's session rather than only the caller's. Deleting the platform
		// stack additionally requires {"confirm": "<label>"} in the body.
		adminAuth.DELETE("/admin/setup-sessions/:id", sh.AdminDeleteSession)
		// Admin-cookie twin of GET /setup/:id/logs. The platform stack belongs
		// to a passkey-less system account, so its provisioning is otherwise
		// unwatchable — nobody can hold that user's cookie.
		adminAuth.GET("/admin/setup-sessions/:id/logs", sh.AdminSessionLogs)
		// Admin twins of the export routes below.
		adminAuth.GET("/admin/setup-sessions/:id/export/configs", sh.AdminExportConfigs)
		adminAuth.GET("/admin/setup-sessions/:id/export/logs", sh.AdminExportLogs)
		// Resumes a session parked at awaiting_admin_did. Same reason as the
		// logs route: the platform stack's owner has no passkey, so the
		// user-facing POST /setup/:id/admin can never be called for it — and
		// the admin DID can't be supplied up front, since `pnm setup` mints it
		// from a VTA DID the pipeline hasn't produced yet.
		adminAuth.POST("/admin/setup-sessions/:id/admin", sh.AdminProvisionAdmin)
		// The rest of the post-provisioning actions, same reasoning: without
		// them an admin can see the platform stack's single-use enrollment and
		// install links but never acknowledge or reissue one, which is most of
		// what finishing the stack consists of.
		adminAuth.POST("/admin/setup-sessions/:id/dids/reissue-enroll", sh.AdminReissueDidsEnroll)
		adminAuth.POST("/admin/setup-sessions/:id/dids/enroll-ack", sh.AdminAckDidsEnroll)
		adminAuth.POST("/admin/setup-sessions/:id/vtc/reissue-install", sh.AdminReissueVtcInstall)
		adminAuth.POST("/admin/setup-sessions/:id/vtc/install-ack", sh.AdminAckVtcInstall)
		// Admin twin of PUT /setup/:id/sharing, for support: a stack whose owner
		// has lost access to it can still be taken out of circulation.
		adminAuth.PUT("/admin/setup-sessions/:id/sharing", sh.AdminSetSharing)
		// The farm's own flagship stack at vta.{CLUSTER_DOMAIN} and friends —
		// the mediator and DID host vta_only sessions point at. Created whole
		// (domain + DNS + session) by one action; the only route that can mint
		// a domains row for our own zone.
		adminAuth.POST("/admin/platform-stack", sh.CreatePlatformStack)
		adminAuth.GET("/admin/platform-stack", sh.GetPlatformStack)
		// Co-admins on that stack's VTA — self-service, so a second admin can
		// add the did:key their own `pnm setup` minted instead of asking
		// whoever holds the credential to run `pnm acl create` for them.
		//
		// Adding only. Removal is `pnm acl delete` against the live VTA: no
		// downtime, and it avoids having to work out which ACL entry belongs to
		// whom, which this side cannot answer well once PNM has rotated keys.
		//
		// POST stops the VTA for about a minute and serialises against itself.
		// Narrowed to the platform stack even though the mechanism is
		// session-generic: granting unrestricted admin on a *customer's* stack
		// needs an approval step this doesn't have. See
		// docs/platform-stack-admin-grant-design.md §1 and §7.4.
		adminAuth.GET("/admin/platform-stack/admins", sh.ListPlatformStackAdmins)
		adminAuth.POST("/admin/platform-stack/admins", sh.GrantPlatformStackAdmin)
		// Cluster capacity overview: CPU/memory/storage totals per node plus
		// how many more sessions of each mode still fit.
		dashH := handler.NewDashboardHandler(k8sClient)
		adminAuth.GET("/admin/dashboard", dashH.Get)
		// Same handlers as their /setup/... twins — admins need the same facts
		// (image tags for upgrades, hostname shape for the platform stack page)
		// but sit behind a different cookie.
		adminAuth.GET("/admin/setup/images", sh.Images)
		adminAuth.GET("/admin/setup/domain-info", sh.DomainInfo)

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
		// Remaining per-mode cluster capacity — the create screen checks this to
		// show "Unavailable" and disable the button before submitting.
		userAuth.GET("/setup/availability", sh.Availability)
		// Hostname facts for this environment, so the create screen's hints
		// don't hardcode the production shape.
		userAuth.GET("/setup/domain-info", sh.DomainInfo)
		userAuth.GET("/setup", sh.List)
		userAuth.POST("/setup", sh.Create)
		userAuth.GET("/setup/:id", sh.Get)
		userAuth.DELETE("/setup/:id", sh.Delete)
		userAuth.GET("/setup/:id/logs", sh.Logs)
		// Zips read from the running pods: the rendered config.toml each
		// binary wrote to its PVC, and the pods' own logs — no Job logs.
		userAuth.GET("/setup/:id/export/configs", sh.ExportConfigs)
		userAuth.GET("/setup/:id/export/logs", sh.ExportLogs)
		// Self-service image upgrade/downgrade — a user can only ever change
		// their own session (looked up by unique_id AND user_id).
		userAuth.POST("/setup/:id/upgrade", uph.CreateForSession)
		userAuth.GET("/setup/:id/upgrade", uph.GetForSession)
		userAuth.POST("/setup/:id/admin", sh.ProvisionAdmin)
		userAuth.POST("/setup/:id/dids/reissue-enroll", sh.ReissueDidsEnroll)
		userAuth.POST("/setup/:id/dids/enroll-ack", sh.AckDidsEnroll)
		userAuth.POST("/setup/:id/vtc/reissue-install", sh.ReissueVtcInstall)
		userAuth.POST("/setup/:id/vtc/install-ack", sh.AckVtcInstall)
		// Mint, replace or clear the share code that lets someone else's
		// VTA-only agent connect to this full stack. The code is the only gate:
		// clearing it stops new connections and leaves existing ones running.
		userAuth.PUT("/setup/:id/sharing", sh.SetSharing)
		// Check a pasted bundle without creating anything, so the create form
		// can confirm which stack it names from values this server read rather
		// than from the pasted text itself. Rate-limited: it answers a yes/no
		// about a credential, even though 75 bits behind an authenticated route
		// is not brute-forceable.
		userAuth.POST("/setup/connection/validate",
			middleware.RateLimit(30, time.Minute), sh.ValidateConnection)
	}

	// Domains — a zone the user owns, verified on its own before any session
	// exists. That separation is the point: a session is only ever created
	// against DNS that is already live, so it starts provisioning immediately
	// and never parks half-built waiting for a record.
	//
	// Always on. The feature still needs its one-off cluster prerequisites (the
	// grey-cloud lb records, the ACME issuers) before a verification can pass —
	// but that shows up as a failing check with a reason, which is more useful
	// than a route that pretends not to exist.
	dh := handler.NewDomainHandler(db, cfg.AppEnv, cfg.ClusterDomain, cfg.ClusterIngressIP, k8sClient)
	domains := v1.Group("/domains",
		middleware.AuthRequired(cfg.JWTSecret, middleware.CookieUser),
		middleware.RequireRole(model.RoleUser),
	)
	{
		domains.GET("", dh.List)
		domains.POST("", dh.Create)
		// Both of these perform live DNS lookups, so they share one limit.
		// This is the per-IP backstop against bulk abuse; the real gate is
		// handler.VerifyCooldown, which is per-domain and survives restarts.
		resolveLimit := middleware.RateLimit(40, time.Minute)
		domains.GET("/:id", resolveLimit, dh.Get)
		domains.POST("/:id/verify", resolveLimit, dh.Verify)
		domains.DELETE("/:id", dh.Delete)
	}

	return r
}
