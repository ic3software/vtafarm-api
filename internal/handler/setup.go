package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/capacity"
	"github.com/ic3software/vtafarm-api/internal/cloudflare"
	"github.com/ic3software/vtafarm-api/internal/didhosting"
	"github.com/ic3software/vtafarm-api/internal/ghcr"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/setup"
)

type SetupHandler struct {
	db            *gorm.DB
	cf            *cloudflare.Client
	appEnv        string
	ingressIP     string
	clusterDomain string
	// The mediator DID and DID-hosting URLs a vta_only session is wired to are
	// no longer configuration: they are read from the platform stack that
	// actually provides them (sharedInfra). What remains global is the client
	// keypair the factory authenticates with — vtafarm-api's own identity.
	didHosting *didhosting.Factory // nil when no keypair configured
	k8s        *k8s.Client
	orch       *setup.Orchestrator
	ghcr       *ghcr.Client // nil when not configured
	capacity   *CapacityService
	// maxStackConnections caps how many vta_only sessions may connect to one
	// shared full_stack; 0 disables the cap. Not a capacity model — the
	// consumer's own pod is what this cluster accounts for. It bounds what a
	// single share code can commit of somebody else's storage and message
	// volume, which matters because a provider cannot remove one connection.
	maxStackConnections int

	// full_stack mode
	mediatorGhcr *ghcr.Client // nil when not configured
	didsGhcr     *ghcr.Client // nil when not configured
	vtcGhcr      *ghcr.Client // nil when not configured
}

func NewSetupHandler(
	db *gorm.DB,
	cf *cloudflare.Client,
	appEnv, ingressIP, clusterDomain string,
	dhFactory *didhosting.Factory,
	k8sClient *k8s.Client,
	orch *setup.Orchestrator,
	ghcrClient *ghcr.Client,
	mediatorGhcrClient *ghcr.Client,
	didsGhcrClient *ghcr.Client,
	vtcGhcrClient *ghcr.Client,
	maxStackConnections int,
) *SetupHandler {
	return &SetupHandler{
		db:                  db,
		cf:                  cf,
		appEnv:              appEnv,
		ingressIP:           ingressIP,
		clusterDomain:       clusterDomain,
		didHosting:          dhFactory,
		k8s:                 k8sClient,
		orch:                orch,
		ghcr:                ghcrClient,
		capacity:            NewCapacityService(k8sClient),
		maxStackConnections: maxStackConnections,

		mediatorGhcr: mediatorGhcrClient,
		didsGhcr:     didsGhcrClient,
		vtcGhcr:      vtcGhcrClient,
	}
}

func (h *SetupHandler) cfRequired(c *gin.Context) bool {
	if h.cf == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "cloudflare not configured"})
		return false
	}
	return true
}

// POST /api/v1/setup/validate
func (h *SetupHandler) Validate(c *gin.Context) {
	if !h.cfRequired(c) {
		return
	}

	if err := h.cf.VerifyZone(c.Request.Context()); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "cloudflare connectivity failed: " + err.Error()})
		return
	}

	if h.ingressIP == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "cluster not configured: CLUSTER_INGRESS_IP not set"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"cloudflare": "ok"})
}

// GET /api/v1/setup/images?component=vta|mediator|dids|vtc
// component defaults to "vta" (vta_only's existing behavior, unchanged).
// mediator/dids are full_stack-only and vtc is full_stack-only —
// same GHCR-package-tags pattern as vta.
func (h *SetupHandler) Images(c *gin.Context) {
	type imageOption struct {
		Tag    string `json:"tag"`
		Image  string `json:"image"`
		Latest bool   `json:"latest,omitempty"`
	}

	component := c.DefaultQuery("component", "vta")
	var client *ghcr.Client
	switch component {
	case "vta":
		client = h.ghcr
	case "mediator":
		client = h.mediatorGhcr
	case "dids":
		client = h.didsGhcr
	case "vtc":
		client = h.vtcGhcr
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown component " + component + " (expected vta, mediator, dids, or vtc)"})
		return
	}

	if client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image source not configured for " + component})
		return
	}

	tags, err := client.ListTags(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch images: " + err.Error()})
		return
	}

	result := make([]imageOption, len(tags))
	for i, t := range tags {
		result[i] = imageOption{Tag: t.Tag, Image: t.Image, Latest: t.Latest}
	}
	c.JSON(http.StatusOK, result)
}

type createSetupRequest struct {
	Mode string `json:"mode"      binding:"required,oneof=vta_only full_stack"`
	// Required on a managed session, globally unique, DNS-safe
	// (setup.ValidateName) — becomes the session's subdomains: vta-<name>
	// (plus mediator-<name> / dids-<name> for full_stack). Must be absent on a
	// custom domain, whose labels are fixed.
	VtaName string `json:"vta_name"`
	// DomainID attaches the session to one of the caller's verified domains.
	// Omitted → managed, today's behaviour.
	DomainID *uint `json:"domain_id"`
	// Label replaces vta_name/vtc_name on a fixed-label domain, where neither
	// reaches a hostname and their only surviving job is the did:webvh path.
	// Duplicates across users are fine there.
	Label    string `json:"label"`
	VtaImage string `json:"vta_image" binding:"required"`
	// Optional — if set, Phase 2 (import-did + Deployment) starts automatically after Phase 1.
	AdminDid string `json:"admin_did"`
	// Advanced — optional, defaults: portable=true, pre_rotation_count=1
	Portable         *bool `json:"portable"`
	PreRotationCount *int  `json:"pre_rotation_count"`
	// full_stack only — all three images are required for that mode. vtc_name
	// is globally unique and DNS-safe like vta_name (becomes the vtc-<name>
	// subdomain) and doubles as the VTA context id.
	MediatorImage string `json:"mediator_image"`
	DidsImage     string `json:"dids_image"`
	VtcImage      string `json:"vtc_image"`
	VtcName       string `json:"vtc_name"`
	// ShareCode points a vta_only session at a full_stack in this farm other
	// than the platform one. Omitted → the platform stack, unchanged.
	//
	// One code and nothing else: it is globally unique, so it identifies its
	// stack on its own, and every value the session is built from comes off that
	// row. There is deliberately nothing here naming a host.
	ShareCode string `json:"share_code"`
}

// POST /api/v1/setup
func (h *SetupHandler) Create(c *gin.Context) {
	if !h.cfRequired(c) {
		return
	}

	var req createSetupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if h.ingressIP == "" || h.clusterDomain == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "cluster not configured: CLUSTER_INGRESS_IP and CLUSTER_DOMAIN must be set"})
		return
	}

	userID := c.MustGet(middleware.ContextUserID).(uint)

	var user model.User
	if err := h.db.First(&user, userID).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
		return
	}

	// The domain is resolved first because it decides what the names mean: on
	// the managed zone vta_name/vtc_name *are* hostnames and must be globally
	// unique; on a custom domain the four labels are fixed, so the names reach
	// no hostname and collapse into one free-form label.
	domain := h.resolveCreateDomain(c, req, userID)
	if c.IsAborted() {
		return
	}

	if domain == nil {
		if req.Label != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "label applies only to a custom domain — a managed session is named by vta_name"})
			return
		}
		if req.VtaName == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "vta_name is required"})
			return
		}
		if err := setup.ValidateName(req.VtaName); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid vta_name: " + err.Error()})
			return
		}
		var nameTaken int64
		h.db.Model(&model.SetupSession{}).
			Where("vta_name = ? AND domain_type = ?", req.VtaName, model.DomainManaged).
			Count(&nameTaken)
		if nameTaken > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "vta_name already in use"})
			return
		}
	} else {
		if req.VtaName != "" || req.VtcName != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "vta_name and vtc_name don't apply to a custom domain — its hostnames are fixed; send label instead"})
			return
		}
		if req.Label == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "label is required"})
			return
		}
		// Still DNS-safe: it lands in did:webvh paths and URLs even though no
		// hostname carries it.
		if err := setup.ValidateName(req.Label); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid label: " + err.Error()})
			return
		}
		// One session per domain, checked here for the message; the partial
		// unique index on domain_id is the real gate.
		var inUse int64
		h.db.Model(&model.SetupSession{}).Where("domain_id = ?", domain.ID).Count(&inUse)
		if inUse > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "this domain is already in use by another session"})
			return
		}
	}

	if req.Mode == model.ModeFullStack {
		// A full_stack provisions its own mediator and DID host, so there is
		// nothing for a share code to point at. Refused rather than ignored:
		// silently dropping it would let someone believe their new stack was
		// wired to somebody else's.
		if req.ShareCode != "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "a full-stack session runs its own mediator and DID host — share_code applies only to a VTA-only agent"})
			return
		}
		if !user.BetaAccess {
			c.JSON(http.StatusForbidden, gin.H{"error": req.Mode + " mode is in beta — ask an admin to enable beta access for your account"})
			return
		}
		if !h.capacityAllows(c, capacity.FullStack) {
			return
		}
		h.createFullStack(c, req, domain)
		return
	}

	// The UI disables this, but the UI is not the gate: a vta_only agent whose
	// mediator and DID host don't exist can never deliver a message, and it
	// would consume cluster resources looking healthy while it did so.
	// It also yields the values the session is built from, so the gate and the
	// source are the same read — there is no window where the check passes and
	// the write then uses something else.
	//
	// Re-run in full even when the frontend already called
	// POST /setup/connection/validate: a stack can stop running, rotate its code
	// or fill up in between. Validate is a courtesy; this is the gate.
	infra, provider, reason, detail := h.resolveProvider(req.ShareCode)
	if reason != "" {
		// A refused code is the caller's problem and says which; a missing or
		// unready platform stack is the farm's, and has always been a 503.
		status := http.StatusServiceUnavailable
		if req.ShareCode != "" {
			status = connectionRefusalStatus(reason)
		}
		c.JSON(status, gin.H{"error": detail, "reason": reason})
		return
	}

	if !h.capacityAllows(c, capacity.VtaOnly) {
		return
	}

	session, status, err := h.createManagedVtaOnlySession(
		c.Request.Context(), userID, req, infra, provider, nil,
	)
	if err != nil {
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":        session.VtaName,
		"url":       session.PublicURL(),
		"status":    session.Status,
		"vta_image": req.VtaImage,
	})
}

// createManagedVtaOnlySession is the shared write path for an ordinary
// VTA-only create and an admin load-test member. Validation, provider lookup
// and capacity decisions stay with the caller; this function owns the atomic
// DNS + row creation and starts the ordinary orchestrator.
func (h *SetupHandler) createManagedVtaOnlySession(
	ctx context.Context,
	userID uint,
	req createSetupRequest,
	infra sharedInfra,
	provider *model.SetupSession,
	loadTestRunID *uint,
) (*model.SetupSession, int, error) {
	portable := true
	if req.Portable != nil {
		portable = *req.Portable
	}
	preRotationCount := 1
	if req.PreRotationCount != nil {
		preRotationCount = *req.PreRotationCount
	}

	// One path component, not two: the globally unique VTA name is also the
	// stable path served by the shared DID-hosting daemon.
	vtaDidURL := infra.ServerURL + "/" + setup.VtaDidPath(req.VtaName)
	subdomain := setup.VtaHost(h.appEnv, req.VtaName)
	fqdn := subdomain + "." + h.clusterDomain

	recordID, err := h.cf.CreateARecord(ctx, fqdn, h.ingressIP)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("failed to create DNS record: %w", err)
	}

	session := &model.SetupSession{
		UserID:               userID,
		Mode:                 model.ModeVtaOnly,
		Status:               "dns_provisioned",
		DomainType:           model.DomainManaged,
		Domain:               h.clusterDomain,
		Subdomain:            subdomain,
		CFRecordID:           recordID,
		VtaName:              req.VtaName,
		MediatorDid:          infra.MediatorDid,
		VtaDidUrl:            vtaDidURL,
		DidHostingServerURL:  infra.ServerURL,
		DidHostingControlURL: infra.ControlURL,
		DIDHostingDid:        infra.DaemonDid,
		ConnectionSource:     model.ConnectionPlatform,
		LoadTestRunID:        loadTestRunID,
		VtaImage:             req.VtaImage,
		AdminDid:             req.AdminDid,
		Portable:             portable,
		PreRotationCount:     preRotationCount,
	}
	if req.ShareCode != "" {
		if provider == nil {
			_ = h.cf.DeleteRecord(ctx, recordID)
			return nil, http.StatusInternalServerError, errors.New("provider session missing")
		}
		session.ConnectionSource = model.ConnectionInFarm
		session.ProviderSessionID = &provider.ID
	}

	if createErr := h.db.Create(session).Error; createErr != nil {
		_ = h.cf.DeleteRecord(ctx, recordID)
		if isUniqueViolation(createErr, "setup_sessions_vta_name_unique") ||
			isUniqueViolation(createErr, "setup_sessions_did_path_unique") {
			return nil, http.StatusConflict, errors.New("vta_name already in use")
		}
		return nil, http.StatusInternalServerError, errors.New("failed to persist session")
	}

	if h.orch != nil {
		h.orch.Start(session.ID)
	}
	return session, http.StatusCreated, nil
}

// resolveCreateDomain turns POST /setup's optional domain_id into the domain
// row the session will run under, or nil for a managed session. It aborts the
// request with the response itself on any problem — callers check
// c.IsAborted().
//
// vta_only is deliberately excluded: that mode points at a shared mediator and
// DID host, so a user's domain would cover only part of their footprint.
func (h *SetupHandler) resolveCreateDomain(c *gin.Context, req createSetupRequest, userID uint) *model.Domain {
	if req.DomainID == nil {
		return nil
	}
	if req.Mode != model.ModeFullStack {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "a custom domain requires full_stack — vta_only uses a shared mediator and DID host"})
		return nil
	}

	var d model.Domain
	// Scoped to the caller AND to kind=custom: the platform domain is reachable
	// only through POST /admin/platform-stack, and no user-facing route may
	// ever produce a session on our own zone.
	err := h.db.Where("id = ? AND user_id = ? AND kind = ?", *req.DomainID, userID, model.DomainKindCustom).First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "domain not found"})
		return nil
	}
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to read domain"})
		return nil
	}
	if !d.Verified() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "verify this domain before creating a session"})
		return nil
	}
	return &d
}

// Reasons a mode can't be created right now, beyond cluster capacity. Returned
// by GET /setup/availability so the create screen can say *why* rather than a
// bare "Unavailable", and echoed by POST /setup when it refuses.
const (
	reasonAtCapacity         = "at_capacity"
	reasonPlatformMissing    = "platform_stack_missing"
	reasonPlatformNotReady   = "platform_stack_not_ready"
	reasonSharedUnconfigured = "shared_infra_unconfigured"
	// reasonProviderUnknown means the lookup itself failed — a database error,
	// not a statement about the stack. The two callers must treat it
	// differently, which is why it is a reason rather than a bare error: see
	// resolveProvider.
	reasonProviderUnknown = "provider_lookup_failed"
)

// Reasons a pasted connection bundle is refused. Split across two tiers by how
// much the caller has proved — see resolveProvider.
const (
	reasonBadBundle = "bad_bundle"
	reasonWrongFarm = "wrong_farm"
	// reasonInvalidBundle is deliberately one reason for five situations: no
	// such stack, a stack that never shared, one that turned sharing off, a
	// rotated code, and a mangled code. Distinguishing them would turn this into
	// a way to discover which stacks exist and which are shared, and from the
	// holder's side they are the same fact — this bundle does not currently open
	// anything — with the same next step: ask for a current one.
	reasonInvalidBundle    = "invalid_bundle"
	reasonStackNotRunning  = "stack_not_running"
	reasonStackChanged     = "stack_changed"
	reasonStackAtConnLimit = "stack_at_connection_limit"
)

// sharedInfra is what a vta_only session is wired to — read from the platform
// stack that actually provides it, never from configuration.
//
// These used to be MEDIATOR_DID / DID_HOSTING_SERVER_URL / DID_HOSTING_CONTROL_URL
// in the environment, pasted in by an admin from the platform stack page once
// the pipeline had minted them. Reading the row directly removes that copy step,
// and with it the whole class of "the stack is running but this server is still
// pointed at the last one" failure.
//
// ControlURL and ServerURL are the same value today because the daemon build
// answers both roles on one host; they are carried separately so a standalone
// DID-hosting service can split them without a schema change.
type sharedInfra struct {
	MediatorDid string
	ServerURL   string
	ControlURL  string
	// DaemonDid is the DID the daemon at ControlURL reports as its own, taken
	// from the provider's row rather than from the daemon itself. Snapshotted
	// onto the consumer so didhosting.Factory.For can refuse a host answering
	// with somebody else's DID — the token it would receive is signed with the
	// farm's admin key and replayable wherever that DID is enrolled.
	//
	// Not part of the readiness gate below. A provider that never recorded one
	// yields "", which means "no expectation on record" and behaves exactly as
	// this did before the field existed — deliberately, so a platform stack
	// built before the column was populated does not suddenly refuse to serve.
	DaemonDid string
}

// resolveProvider finds the stack a vta_only session will be wired to, and
// reports whether it is usable.
//
// Today that is always the platform stack (design §3.3) — a vta_only agent is
// only the VTA, pointed at a mediator and DID-hosting daemon it does not run
// itself, so creating one before those exist produces an agent that can never
// deliver a message. Naming this after the *role* rather than after the
// platform stack is what lets a bundle-named provider join later without a
// second, parallel path to the same values.
//
// reason is "" exactly when the returned sharedInfra is usable. The provider row
// is returned alongside it because callers need more than the three values —
// the connection has to be recorded against a row, not a URL.
//
// full_stack is unaffected: it provisions its own mediator and DID host.
func (h *SetupHandler) resolveProvider(shareCode string) (v sharedInfra, provider *model.SetupSession, reason, detail string) {
	if shareCode != "" {
		return h.resolveShareCode(shareCode)
	}

	const missing = "VTA-only agents need the platform stack — the shared mediator and DID hosting they connect to. " +
		"An admin has to create it before any VTA-only agent can be provisioned."
	const unknown = "Couldn't check the platform stack just now. Please try again."

	domain, err := h.platformDomain()
	if err != nil {
		return v, nil, reasonProviderUnknown, unknown
	}
	if domain == nil {
		return v, nil, reasonPlatformMissing, missing
	}

	var session model.SetupSession
	err = h.db.Where("domain_id = ?", domain.ID).First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// The domains row outlives its session: the name is still ours, but
		// nothing is running on it.
		return v, nil, reasonPlatformMissing, missing
	}
	if err != nil {
		return v, nil, reasonProviderUnknown, unknown
	}

	v, reason, detail = providerInfra(&session)
	if reason != "" {
		return sharedInfra{}, nil, reason, detail
	}
	return v, &session, "", ""
}

// providerInfra turns a candidate provider row into what a vta_only session
// wires itself to, or the reason it cannot be used.
//
// Split out from the lookup above because it is the half with all the
// judgement in it and none of the I/O, so it can be tested directly — and
// because a provider named by a share code has to be held to exactly the same
// readiness bar as the platform stack. Two copies of that bar would drift.
func providerInfra(s *model.SetupSession) (v sharedInfra, reason, detail string) {
	if s.Status != "running" {
		return v, reasonPlatformNotReady,
			"The platform stack — the shared mediator and DID hosting VTA-only agents connect to — is still being set up. " +
				"Try again once it's running."
	}

	// Running, yet its mediator DID is missing. This used to mean "an admin
	// hasn't copied the values into the environment yet" and was the normal
	// state for a while after provisioning; reading the row instead makes it a
	// narrow, transient one — a stack marked running whose 1b output never
	// landed. Kept rather than dropped because a session created here would
	// still carry an empty mediator DID and never deliver a message.
	//
	// The hostname is tested through its two components rather than through
	// DidsURL(). That builder always prefixes "https://", so its result is never
	// empty and a row with no dids hostname used to pass this check and yield
	// "https://." — a URL that resolves to nothing, is snapshotted onto the
	// session forever, and fails much later.
	//
	// DaemonDid is deliberately not tested — see sharedInfra.
	if s.MediatorDid == "" || s.DidsSubdomain == "" || s.Domain == "" {
		return sharedInfra{}, reasonSharedUnconfigured,
			"The platform stack is running but hasn't published its mediator DID yet. " +
				"Try again shortly; if it persists, an admin should check the stack."
	}

	return sharedInfra{
		MediatorDid: s.MediatorDid,
		// The stack's own daemon. Both roles on one host — see sharedInfra.
		ServerURL:  s.DidsURL(),
		ControlURL: s.DidsURL(),
		DaemonDid:  s.DIDHostingDid,
	}, "", ""
}

// capacityAllows gates a create on remaining cluster capacity for mode. It
// fails open: if capacity can't be measured (no k8s client / stats read failed)
// it returns true rather than blocking. Only a measured "zero fit" writes 503
// and returns false.
func (h *SetupHandler) capacityAllows(c *gin.Context, mode capacity.Mode) bool {
	if h.capacity == nil {
		return true
	}
	fits, determinable := h.capacity.ModeFits(c.Request.Context(), mode)
	if !determinable {
		return true
	}
	if !fits {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "the cluster is at capacity — no resources are available to create a new agent right now. Please try again later or contact an admin."})
		return false
	}
	return true
}

// GET /api/v1/setup/availability — how many more sessions of each creatable
// mode still fit, so the create screen can show "Unavailable" and disable the
// button before the user submits. Fails open: when capacity can't be measured
// it reports every mode available with determinable=false, so a transient
// metrics/Longhorn outage never wrongly blocks the UI.
func (h *SetupHandler) Availability(c *gin.Context) {
	type modeAvail struct {
		Count int `json:"count"`
		// Available describes the DEFAULT path — for vta_only, the platform
		// stack. It is not the whole story for that mode any more, because a
		// caller carrying a connection bundle needs no platform stack at all.
		Available bool `json:"available"`
		// Why it's unavailable, and a sentence to show the user. Absent when
		// the mode is creatable.
		Reason string `json:"reason,omitempty"`
		Detail string `json:"detail,omitempty"`
		// CustomTargetAllowed says whether vta_only can be created against a
		// stack the caller names, which stays true when the platform stack is
		// missing and false only when the cluster itself is full. It is what
		// lets the UI disable one option rather than the whole mode: the
		// platform stack is a default, not a prerequisite.
		CustomTargetAllowed bool `json:"custom_target_allowed,omitempty"`
	}

	// Fail open on capacity, as before: a transient metrics/Longhorn outage
	// must never wrongly block creation.
	vtaOnly := modeAvail{Available: true}
	fullStack := modeAvail{Available: true}

	est, meta, determinable := h.capacity.Estimates(c.Request.Context())
	if determinable {
		vtaOnly.Count = est[capacity.VtaOnly.Name].Count
		vtaOnly.Available = vtaOnly.Count >= 1
		fullStack.Count = est[capacity.FullStack.Name].Count
		fullStack.Available = fullStack.Count >= 1
	}

	atCapacity := "The cluster is at capacity and can't provision a new agent right now. " +
		"Please try again later or contact an admin."
	if !vtaOnly.Available {
		vtaOnly.Reason, vtaOnly.Detail = reasonAtCapacity, atCapacity
	}
	if !fullStack.Available {
		fullStack.Reason, fullStack.Detail = reasonAtCapacity, atCapacity
	}

	// The shared mediator and DID host is a hard dependency of vta_only, not a
	// capacity question — so it overrides the fail-open above rather than
	// sitting alongside it. full_stack runs its own and is never gated on it.
	//
	// reasonProviderUnknown is the exception, and the two callers of
	// resolveProvider part company here: a database read that failed says
	// nothing about the stack, so reporting it as unavailable would blank the
	// create screen on a blip. It fails open, like capacity above. POST /setup
	// refuses on the same reason, because there it is the difference between
	// waiting and provisioning an agent with no mediator DID at all.
	//
	// It gates the DEFAULT path only. Connecting to a stack the caller names
	// needs no platform stack, so that option survives every reason below —
	// cluster capacity, decided above, is the only thing that can close it.
	vtaOnly.CustomTargetAllowed = vtaOnly.Available || vtaOnly.Reason != reasonAtCapacity
	if _, _, reason, detail := h.resolveProvider(""); reason != "" && reason != reasonProviderUnknown {
		vtaOnly.Available = false
		vtaOnly.Reason, vtaOnly.Detail = reason, detail
	}

	c.JSON(http.StatusOK, gin.H{
		"vta_only":          vtaOnly,
		"full_stack":        fullStack,
		"metrics_available": meta.MetricsAvailable,
		"storage_available": meta.StorageAvailable,
		"determinable":      determinable,
	})
}

// GET /api/v1/setup/domain-info — the environment's hostname facts, so the
// portal can render accurate hints instead of hardcoding the production shape
// (`vta-<name>.firstperson.dev`), which is wrong in development and will be
// wrong again for custom and platform domains.
//
//	managed_domain  the zone managed sessions live under (CLUSTER_DOMAIN)
//	env_prefix      "dev-" locally, "" in production — prefixed onto every label
//	target_ip       the ingress IP a custom domain must ultimately resolve to
//	target_host     the hostname a custom domain CNAMEs at
//
// Also served under /admin/setup/domain-info for the admin panel, which holds
// a different cookie and needs the same facts to name the platform stack's
// hostnames before it exists.
func (h *SetupHandler) DomainInfo(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"managed_domain": h.clusterDomain,
		"env_prefix":     setup.EnvPrefix(h.appEnv),
		"target_ip":      h.ingressIP,
		"target_host":    setup.CNAMETarget(h.appEnv, h.clusterDomain),
	})
}

// GET /api/v1/setup
func (h *SetupHandler) List(c *gin.Context) {
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var sessions []model.SetupSession
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&sessions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sessions"})
		return
	}

	type item struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Mode   string `json:"mode"`
		// Where the hostnames come from — managed | custom | platform.
		// Orthogonal to mode; `domain` is the zone they sit under.
		DomainType  string `json:"domain_type"`
		Domain      string `json:"domain"`
		URL         string `json:"url,omitempty"`
		URLs        gin.H  `json:"urls,omitempty"` // full_stack only
		VtaName     string `json:"vta_name"`
		VtaImage    string `json:"vta_image,omitempty"`
		MediatorDid string `json:"mediator_did"`
		VtaDidUrl   string `json:"vta_did_url"`
		VtaDid      string `json:"vta_did,omitempty"`
		ErrorMsg    string `json:"error_msg,omitempty"`
		CreatedAt   any    `json:"created_at"`
		UpdatedAt   any    `json:"updated_at"`
		// vta_only: where its mediator and DID host came from, and whether that
		// stack still exists. On the list so an orphaned agent can be marked
		// without opening it — its badge still reads `running`, because it is,
		// so nothing else on the row would give it away.
		ConnectionSource string `json:"connection_source,omitempty"`
		ProviderGone     bool   `json:"provider_gone,omitempty"`
		// full_stack: how many other people's agents depend on this stack.
		ConnectionCount int64 `json:"connection_count,omitempty"`
	}

	result := make([]item, len(sessions))
	for i, s := range sessions {
		it := item{
			ID:          s.VtaName,
			Status:      s.Status,
			Mode:        s.Mode,
			DomainType:  s.DomainType,
			Domain:      s.Domain,
			VtaName:     s.VtaName,
			VtaImage:    s.VtaImage,
			MediatorDid: s.MediatorDid,
			VtaDidUrl:   s.VtaDidUrl,
			VtaDid:      s.VtaDid,
			ErrorMsg:    s.ErrorMsg,
			CreatedAt:   s.CreatedAt,
			UpdatedAt:   s.UpdatedAt,
		}
		if s.IsFullStack() {
			it.URLs = gin.H{
				"vta":      s.PublicURL(),
				"mediator": "https://" + s.MediatorFQDN(),
				"dids":     "https://" + s.DidsFQDN(),
				"vtc":      "https://" + s.VtcFQDN(),
			}
			it.ConnectionCount = h.countConnections(s.ID)
		} else {
			it.URL = s.PublicURL()
			it.ConnectionSource = s.ConnectionSource
			it.ProviderGone = s.IsOrphaned()
		}
		result[i] = it
	}
	c.JSON(http.StatusOK, result)
}

// GET /api/v1/setup/:id
func (h *SetupHandler) Get(c *gin.Context) {
	publicID := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("vta_name = ? AND user_id = ?", publicID, userID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	if session.IsFullStack() {
		h.getFullStack(c, &session)
		return
	}

	resp := gin.H{
		"id":          session.VtaName,
		"status":      session.Status,
		"mode":        session.Mode,
		"domain_type": session.DomainType,
		"domain":      session.Domain,
		"url":         session.PublicURL(),
		"vta_image":   session.VtaImage,
		"vta_did":     session.VtaDid,
		"created_at":  session.CreatedAt,
		"updated_at":  session.UpdatedAt,
	}
	if session.ErrorMsg != "" {
		resp["error_msg"] = session.ErrorMsg
	}
	h.describeConnection(resp, &session)
	c.JSON(http.StatusOK, resp)
}

// describeConnection adds which stack a vta_only session is wired to.
//
// The first question when an agent misbehaves is whose infrastructure it is
// on, and until now the answer was a bare mediator DID. `provider` names the
// stack; its absence on an in_farm session is not missing data but the fact
// that the stack was deleted — see model.IsOrphaned.
func (h *SetupHandler) describeConnection(resp gin.H, s *model.SetupSession) {
	if s.IsFullStack() {
		return
	}
	resp["connection_source"] = s.ConnectionSource
	if s.ConnectionSource != model.ConnectionInFarm {
		return
	}
	if s.ProviderSessionID == nil {
		// The agent keeps running — nothing in a provider teardown touches the
		// consumer's namespace — but its DID no longer resolves and its
		// mediator is gone. Reported as a distinct fact rather than as a status,
		// because nothing about this session's own pipeline failed.
		resp["provider_gone"] = true
		return
	}
	var provider model.SetupSession
	if err := h.db.Select("vta_name").First(&provider, *s.ProviderSessionID).Error; err == nil {
		resp["provider"] = provider.VtaName
	}
}

// DELETE /api/v1/setup/:id
func (h *SetupHandler) Delete(c *gin.Context) {
	publicID := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("vta_name = ? AND user_id = ?", publicID, userID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	h.teardownSession(c, &session)
}

// teardownSession destroys everything a session owns — orchestrator goroutine,
// DNS records, hosted DID + ACL, K8s resources, Vault seed, the row itself —
// and, when it was the user's last session, their namespace and Vault access
// too. It writes the response (204, or an error status) itself.
//
// Ownership is the caller's business: Delete scopes the lookup to the calling
// user, AdminDeleteSession accepts any session. Everything after that point is
// identical, so both funnel through here.
func (h *SetupHandler) teardownSession(c *gin.Context, session *model.SetupSession) {
	if h.orch != nil {
		h.orch.Cancel(session.ID)
	}

	if session.IsFullStack() {
		h.deleteFullStack(c, session)
		return
	}
	if status, err := h.teardownVtaOnlySession(c.Request.Context(), session); err != nil {
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// teardownVtaOnlySession removes one VTA-only session without writing an HTTP
// response. The user/admin delete handlers and load-test batch cleanup share it
// so all three remove the same DNS, DID-hosting, Kubernetes and Vault state.
func (h *SetupHandler) teardownVtaOnlySession(ctx context.Context, session *model.SetupSession) (int, error) {
	if h.cf != nil && session.CFRecordID != "" {
		if err := h.cf.DeleteRecord(ctx, session.CFRecordID); err != nil {
			return http.StatusBadGateway, fmt.Errorf("failed to delete DNS record: %w", err)
		}
	}

	// Through the control URL this session was provisioned against, not the
	// current one: a platform stack rebuilt since then is a different daemon,
	// and deleting from it would leave this session's DID log behind on the old
	// one while removing somebody else's.
	if h.didHosting != nil && (session.VtaDidUrl != "" || session.VtaDid != "") {
		dh, err := h.didHosting.For(session.DidHostingControlURL, session.DIDHostingDid)
		if err != nil {
			log.Printf("[setup] warn: no DID hosting client for session %d (%q): %v",
				session.ID, session.DidHostingControlURL, err)
		} else {
			if session.VtaDidUrl != "" {
				path := session.VtaDidUrl
				if u, err := url.Parse(path); err == nil {
					path = strings.TrimPrefix(u.Path, "/")
				}
				if err := dh.DeleteDid(ctx, path); err != nil {
					log.Printf("[setup] warn: failed to delete DID from hosting for session %d: %v", session.ID, err)
				}
			}
			if session.VtaDid != "" {
				if err := dh.DeleteAcl(ctx, session.VtaDid); err != nil {
					log.Printf("[setup] warn: failed to delete ACL entry for session %d: %v", session.ID, err)
				}
			}
		}
	}

	if h.k8s != nil {
		ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
		h.k8s.DeleteSetupResources(ctx, ns, session.ID)
		h.k8s.DeleteVtaResources(ctx, ns, session.ID)
	}

	// Delete this session's master seed from Vault (best-effort).
	if h.orch != nil {
		h.orch.TeardownVaultSeed(ctx, session.UserID, session.ID)
	}

	if err := h.db.Delete(session).Error; err != nil {
		return http.StatusInternalServerError, errors.New("failed to delete session")
	}

	if h.k8s != nil {
		var remaining int64
		h.db.Model(&model.SetupSession{}).Where("user_id = ?", session.UserID).Count(&remaining)
		if remaining == 0 {
			_ = h.k8s.DeleteNamespace(ctx, fmt.Sprintf("%d", session.UserID))
			// Last session for this user → remove their Vault policy + k8s role.
			if h.orch != nil {
				h.orch.TeardownVaultUserAccess(ctx, session.UserID)
			}
		}
	}
	return http.StatusNoContent, nil
}

// GET /api/v1/setup/:id/logs
func (h *SetupHandler) Logs(c *gin.Context) {
	publicID := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("vta_name = ? AND user_id = ?", publicID, userID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	if h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s not configured"})
		return
	}

	if session.IsFullStack() {
		h.logsFullStack(c, &session)
		return
	}

	if session.Status == "dns_provisioned" {
		c.JSON(http.StatusConflict, gin.H{"error": "setup not started yet"})
		return
	}

	ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	sseError := func(err error) {
		fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", err.Error())
		c.Writer.Flush()
	}

	if source := c.Query("source"); source != "" {
		switch source {
		case "setup":
			sawMarker := false
			if err := h.k8s.StreamJobLogs(c.Request.Context(), ns, k8s.SetupJobName(session.ID), func(line string) {
				if line == "---DID_LOG_START---" {
					sawMarker = true
				}
				if !sawMarker {
					fmt.Fprintf(c.Writer, "data: %s\n\n", line)
					c.Writer.Flush()
				}
			}); err != nil && c.Request.Context().Err() == nil {
				sseError(err)
			}
		case "provision":
			if err := h.k8s.StreamJobLogs(c.Request.Context(), ns, k8s.ProvisionJobName(session.ID), func(line string) {
				fmt.Fprintf(c.Writer, "data: %s\n\n", line)
				c.Writer.Flush()
			}); err != nil && c.Request.Context().Err() == nil {
				sseError(err)
			}
		case "vta":
			if err := h.k8s.StreamVtaPodLogs(c.Request.Context(), ns, session.ID, true, func(line string) {
				fmt.Fprintf(c.Writer, "data: %s\n\n", line)
				c.Writer.Flush()
			}); err != nil && c.Request.Context().Err() == nil {
				sseError(err)
			}
		default:
			fmt.Fprintf(c.Writer, "event: error\ndata: unknown source %q\n\n", source)
			c.Writer.Flush()
		}
		fmt.Fprintf(c.Writer, "event: done\ndata: stream ended\n\n")
		c.Writer.Flush()
		return
	}

	switch session.Status {
	case "vta_setup_running":
		sawMarker := false
		if err := h.k8s.StreamJobLogs(c.Request.Context(), ns, k8s.SetupJobName(session.ID), func(line string) {
			if line == "---DID_LOG_START---" {
				sawMarker = true
			}
			if !sawMarker {
				fmt.Fprintf(c.Writer, "data: %s\n\n", line)
				c.Writer.Flush()
			}
		}); err != nil && c.Request.Context().Err() == nil {
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", err.Error())
			c.Writer.Flush()
		}
	case "provisioning":
		if err := h.k8s.StreamJobLogs(c.Request.Context(), ns, k8s.ProvisionJobName(session.ID), func(line string) {
			fmt.Fprintf(c.Writer, "data: %s\n\n", line)
			c.Writer.Flush()
		}); err != nil && c.Request.Context().Err() == nil {
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", err.Error())
			c.Writer.Flush()
		}
	case "vta_starting", "running", "complete":
		if err := h.k8s.StreamVtaPodLogs(c.Request.Context(), ns, session.ID, false, func(line string) {
			fmt.Fprintf(c.Writer, "data: %s\n\n", line)
			c.Writer.Flush()
		}); err != nil && c.Request.Context().Err() == nil {
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", err.Error())
			c.Writer.Flush()
		}
	case "vta_setup_complete":
		logs, err := h.k8s.JobLogs(c.Request.Context(), ns, k8s.SetupJobName(session.ID))
		if err != nil {
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", err.Error())
		} else {
			for _, line := range splitLines(logs) {
				if line == "---DID_LOG_START---" {
					break
				}
				fmt.Fprintf(c.Writer, "data: %s\n\n", line)
			}
		}
		c.Writer.Flush()
	default: // failed — replay import job if it exists, else setup job
		jobName := k8s.ProvisionJobName(session.ID)
		logs, err := h.k8s.JobLogs(c.Request.Context(), ns, jobName)
		if err != nil {
			logs, err = h.k8s.JobLogs(c.Request.Context(), ns, k8s.SetupJobName(session.ID))
		}
		if err != nil {
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", err.Error())
		} else {
			for _, line := range splitLines(logs) {
				fmt.Fprintf(c.Writer, "data: %s\n\n", line)
			}
		}
		c.Writer.Flush()
	}

	fmt.Fprintf(c.Writer, "event: done\ndata: stream ended\n\n")
	c.Writer.Flush()
}

// POST /api/v1/setup/:id/admin
// userSession loads the session named by :id, scoped to the calling user, and
// adminSession loads it by vta_name alone.
//
// Every session action exists in both cookie families: the user-facing route
// owns the caller's session, and the admin twin reaches any of them. The twins
// are not a convenience — the platform stack is owned by a passkey-less system
// account, so a route that filters by user_id can never be called for it.
// Splitting the lookup from the action keeps one implementation of each action;
// a divergent second copy is how the two drift.
//
// Both write the 404 themselves and return nil when they do.
func (h *SetupHandler) userSession(c *gin.Context) *model.SetupSession {
	userID := c.MustGet(middleware.ContextUserID).(uint)
	return h.findSession(c, h.db.Where("vta_name = ? AND user_id = ?", c.Param("id"), userID))
}

func (h *SetupHandler) adminSession(c *gin.Context) *model.SetupSession {
	return h.findSession(c, h.db.Where("vta_name = ?", c.Param("id")))
}

func (h *SetupHandler) findSession(c *gin.Context, q *gorm.DB) *model.SetupSession {
	var session model.SetupSession
	if err := q.First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return nil
	}
	return &session
}

func (h *SetupHandler) ProvisionAdmin(c *gin.Context) {
	if s := h.userSession(c); s != nil {
		h.provisionAdmin(c, s)
	}
}

// AdminProvisionAdmin — POST /api/v1/admin/setup-sessions/:id/admin (admin only).
//
// An earlier attempt to dodge needing this — requiring admin_did at create time
// — was impossible to satisfy: `pnm setup` mints that DID locally from the VTA
// DID, which does not exist until the pipeline has already run and parked.
func (h *SetupHandler) AdminProvisionAdmin(c *gin.Context) {
	if s := h.adminSession(c); s != nil {
		h.provisionAdmin(c, s)
	}
}

// provisionAdmin resumes a session parked waiting for its PNM admin DID.
// Ownership is the caller's business — both routes above funnel through here so
// the state machine has one entry point.
func (h *SetupHandler) provisionAdmin(c *gin.Context, session *model.SetupSession) {
	readyStatus := "vta_setup_complete"
	if session.IsFullStack() {
		readyStatus = "awaiting_admin_did"
	}
	if session.Status != readyStatus {
		c.JSON(http.StatusConflict, gin.H{"error": "session must be in " + readyStatus + " status"})
		return
	}

	if h.orch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s not configured"})
		return
	}

	var req struct {
		AdminDid string `json:"admin_did" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	h.orch.Provision(session.ID, req.AdminDid)

	c.JSON(http.StatusAccepted, gin.H{"status": "provisioning"})
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
