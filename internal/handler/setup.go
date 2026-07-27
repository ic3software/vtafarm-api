package handler

import (
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
	db             *gorm.DB
	cf             *cloudflare.Client
	appEnv         string
	ingressIP      string
	clusterDomain  string
	mediatorDid    string
	didHostingBase string             // DID_HOSTING_SERVER_URL — public server URL used to build vta_did_url
	didHosting     *didhosting.Client // nil when not configured
	k8s            *k8s.Client
	orch           *setup.Orchestrator
	ghcr           *ghcr.Client // nil when not configured
	capacity       *CapacityService

	// full_stack mode
	mediatorGhcr *ghcr.Client // nil when not configured
	didsGhcr     *ghcr.Client // nil when not configured
	vtcGhcr      *ghcr.Client // nil when not configured
}

func NewSetupHandler(
	db *gorm.DB,
	cf *cloudflare.Client,
	appEnv, ingressIP, clusterDomain, mediatorDid, didHostingBase string,
	dhClient *didhosting.Client,
	k8sClient *k8s.Client,
	orch *setup.Orchestrator,
	ghcrClient *ghcr.Client,
	mediatorGhcrClient *ghcr.Client,
	didsGhcrClient *ghcr.Client,
	vtcGhcrClient *ghcr.Client,
) *SetupHandler {
	return &SetupHandler{
		db:             db,
		cf:             cf,
		appEnv:         appEnv,
		ingressIP:      ingressIP,
		clusterDomain:  clusterDomain,
		mediatorDid:    mediatorDid,
		didHostingBase: didHostingBase,
		didHosting:     dhClient,
		k8s:            k8sClient,
		orch:           orch,
		ghcr:           ghcrClient,
		capacity:       NewCapacityService(k8sClient),

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
	if ready, _, detail := h.sharedInfra(); !ready {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": detail})
		return
	}

	if !h.capacityAllows(c, capacity.VtaOnly) {
		return
	}

	portable := true
	if req.Portable != nil {
		portable = *req.Portable
	}
	preRotationCount := 1
	if req.PreRotationCount != nil {
		preRotationCount = *req.PreRotationCount
	}

	vtaDidUrl := h.didHostingBase + "/" + user.UniqueId + "/" + req.VtaName

	subdomain := setup.VtaHost(h.appEnv, req.VtaName)
	fqdn := subdomain + "." + h.clusterDomain

	recordID, err := h.cf.CreateARecord(c.Request.Context(), fqdn, h.ingressIP)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to create DNS record: " + err.Error()})
		return
	}

	session := model.SetupSession{
		UserID: userID,
		Mode:   req.Mode,
		Status: "dns_provisioned",
		// Set explicitly rather than left to the column default: the field
		// would otherwise read "" in memory for the rest of this request.
		DomainType:       model.DomainManaged,
		Domain:           h.clusterDomain,
		Subdomain:        subdomain,
		CFRecordID:       recordID,
		VtaName:          req.VtaName,
		MediatorDid:      h.mediatorDid,
		VtaDidUrl:        vtaDidUrl,
		VtaImage:         req.VtaImage,
		AdminDid:         req.AdminDid,
		Portable:         portable,
		PreRotationCount: preRotationCount,
	}
	const maxAttempts = 5
	var createErr error
	for range maxAttempts {
		session.UniqueId = generateUniqueId()
		createErr = h.db.Create(&session).Error
		if createErr == nil {
			break
		}
		if !strings.Contains(createErr.Error(), "setup_sessions_unique_id_unique") {
			break
		}
	}
	if createErr != nil {
		_ = h.cf.DeleteRecord(c.Request.Context(), recordID)
		// The pre-insert count check races with concurrent creates; the DB
		// unique index is the real gate.
		if strings.Contains(createErr.Error(), "setup_sessions_vta_name_unique") {
			c.JSON(http.StatusConflict, gin.H{"error": "vta_name already in use"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist session"})
		return
	}

	if h.orch != nil {
		h.orch.Start(session.ID)
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":        session.UniqueId,
		"url":       "https://" + fqdn,
		"status":    session.Status,
		"vta_image": req.VtaImage,
	})
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
)

// sharedInfra reports whether the mediator and DID host that every vta_only
// session points at are actually usable.
//
// That shared infrastructure IS the platform stack (design §3.3) — a vta_only
// agent is only the VTA, wired to a mediator and DID-hosting daemon it does not
// run itself. Creating one before those exist produces an agent that can never
// deliver a message.
//
// Two conditions, because the stack existing is not the same as it being
// reachable. Its DIDs are minted by the pipeline and only reach this API once an
// admin copies them into configuration (§3.3.4), so a session created in the
// window between "running" and "configured" would be written with an empty
// mediator DID. Reading those values straight from the platform session row
// would collapse both checks into one and remove the copy step entirely —
// deliberately parked by §3.3.4 until the platform stack has proven itself.
//
// full_stack is unaffected: it provisions its own mediator and DID host.
func (h *SetupHandler) sharedInfra() (ready bool, reason, detail string) {
	domain, err := h.platformDomain()
	if err != nil {
		// Can't tell — fail open rather than blocking every create on a
		// transient DB read, the same way capacity does.
		return true, "", ""
	}
	if domain == nil {
		return false, reasonPlatformMissing,
			"VTA-only agents need the platform stack — the shared mediator and DID hosting they connect to. " +
				"An admin has to create it before any VTA-only agent can be provisioned."
	}

	var session model.SetupSession
	err = h.db.Where("domain_id = ?", domain.ID).First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// The domains row outlives its session: the name is still ours, but
		// nothing is running on it.
		return false, reasonPlatformMissing,
			"VTA-only agents need the platform stack — the shared mediator and DID hosting they connect to. " +
				"An admin has to create it before any VTA-only agent can be provisioned."
	}
	if err != nil {
		return true, "", ""
	}
	if session.Status != "running" {
		return false, reasonPlatformNotReady,
			"The platform stack — the shared mediator and DID hosting VTA-only agents connect to — is still being set up. " +
				"Try again once it's running."
	}

	// Running, but its collected values haven't reached this API's environment
	// yet, so a session created now would carry an empty mediator DID.
	if h.mediatorDid == "" || h.didHostingBase == "" {
		return false, reasonSharedUnconfigured,
			"The platform stack is running, but this server hasn't been pointed at it yet " +
				"(MEDIATOR_DID / DID_HOSTING_SERVER_URL). An admin needs to copy its values into configuration."
	}

	return true, "", ""
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
		Count     int  `json:"count"`
		Available bool `json:"available"`
		// Why it's unavailable, and a sentence to show the user. Absent when
		// the mode is creatable.
		Reason string `json:"reason,omitempty"`
		Detail string `json:"detail,omitempty"`
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
	if ready, reason, detail := h.sharedInfra(); !ready {
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
	}

	result := make([]item, len(sessions))
	for i, s := range sessions {
		it := item{
			ID:          s.UniqueId,
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
		} else {
			it.URL = s.PublicURL()
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
	if err := h.db.Where("unique_id = ? AND user_id = ?", publicID, userID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	if session.IsFullStack() {
		h.getFullStack(c, &session)
		return
	}

	resp := gin.H{
		"id":          session.UniqueId,
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
	c.JSON(http.StatusOK, resp)
}

// DELETE /api/v1/setup/:id
func (h *SetupHandler) Delete(c *gin.Context) {
	publicID := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("unique_id = ? AND user_id = ?", publicID, userID).First(&session).Error; err != nil {
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

	if h.cf != nil && session.CFRecordID != "" {
		if err := h.cf.DeleteRecord(c.Request.Context(), session.CFRecordID); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to delete DNS record: " + err.Error()})
			return
		}
	}

	if h.didHosting != nil {
		if session.VtaDidUrl != "" {
			path := session.VtaDidUrl
			if u, err := url.Parse(path); err == nil {
				path = strings.TrimPrefix(u.Path, "/")
			}
			if err := h.didHosting.DeleteDid(c.Request.Context(), path); err != nil {
				log.Printf("[setup] warn: failed to delete DID from hosting for session %d: %v", session.ID, err)
			}
		}
		if session.VtaDid != "" {
			if err := h.didHosting.DeleteAcl(c.Request.Context(), session.VtaDid); err != nil {
				log.Printf("[setup] warn: failed to delete ACL entry for session %d: %v", session.ID, err)
			}
		}
	}

	if h.k8s != nil {
		ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
		h.k8s.DeleteSetupResources(c.Request.Context(), ns, session.ID)
		h.k8s.DeleteVtaResources(c.Request.Context(), ns, session.ID)
	}

	// Delete this session's master seed from Vault (best-effort).
	if h.orch != nil {
		h.orch.TeardownVaultSeed(c.Request.Context(), session.UserID, session.ID)
	}

	if err := h.db.Delete(session).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete session"})
		return
	}

	if h.k8s != nil {
		var remaining int64
		h.db.Model(&model.SetupSession{}).Where("user_id = ?", session.UserID).Count(&remaining)
		if remaining == 0 {
			_ = h.k8s.DeleteNamespace(c.Request.Context(), fmt.Sprintf("%d", session.UserID))
			// Last session for this user → remove their Vault policy + k8s role.
			if h.orch != nil {
				h.orch.TeardownVaultUserAccess(c.Request.Context(), session.UserID)
			}
		}
	}

	c.Status(http.StatusNoContent)
}

// GET /api/v1/setup/:id/logs
func (h *SetupHandler) Logs(c *gin.Context) {
	publicID := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("unique_id = ? AND user_id = ?", publicID, userID).First(&session).Error; err != nil {
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
func (h *SetupHandler) ProvisionAdmin(c *gin.Context) {
	publicID := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("unique_id = ? AND user_id = ?", publicID, userID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

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
