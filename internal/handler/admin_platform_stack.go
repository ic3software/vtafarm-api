package handler

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/capacity"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/setup"
)

// The platform stack: one full_stack session per environment running under our
// own zone's fixed labels (vta.firstperson.dev and friends), which is the
// mediator and DID host every vta_only session points at.
//
// It is created whole — domain row, DNS, session — by a single admin action,
// because it happens once per environment and splitting it would expose a
// two-step ceremony for no benefit (design §3.3.2).

const (
	// platformLabelDefault is used when the caller sends no label. The label
	// never reaches a hostname — the labels are fixed — and survives only in
	// did:webvh paths: did:webvh:<scid>:dids.firstperson.dev:firstperson-vta.
	platformLabelDefault = "firstperson"

	// systemAccountUniqueID identifies the users row that owns the platform
	// stack (design §3.3.6). It is not a login: the account has no passkey and
	// no email, and exists so setup_sessions.user_id — which is a FK to users
	// and derives the Kubernetes namespace — has something real to point at.
	// An admin id cannot be used: admins are a different table, so the id
	// would name whichever user happens to hold it.
	systemAccountUniqueID = "platform"
)

// systemAccount returns the platform stack's owning account, creating it on
// first use. Idempotent under concurrent calls: users.unique_id is uniquely
// indexed, so a losing racer re-reads the winner's row.
func (h *SetupHandler) systemAccount() (*model.User, error) {
	var user model.User
	err := h.db.Where("unique_id = ?", systemAccountUniqueID).First(&user).Error
	if err == nil {
		return &user, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	user = model.User{UniqueId: systemAccountUniqueID}
	if err := h.db.Create(&user).Error; err != nil {
		// Lost the race — the other caller's row is the one to use.
		if reErr := h.db.Where("unique_id = ?", systemAccountUniqueID).First(&user).Error; reErr == nil {
			return &user, nil
		}
		return nil, err
	}
	log.Printf("[platform] created system account (user id %d)", user.ID)
	return &user, nil
}

// platformDomain loads the platform domain row, or nil when none exists.
func (h *SetupHandler) platformDomain() (*model.Domain, error) {
	var d model.Domain
	err := h.db.Where("kind = ?", model.DomainKindPlatform).First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

type createPlatformStackRequest struct {
	// Label defaults to platformLabelDefault. DNS-safe because it still lands
	// in did:webvh paths and URLs, even though no hostname uses it.
	Label string `json:"label"`
	// There is deliberately no admin_did here. The stack follows exactly the
	// same sequence a user's session does — provision, park at
	// awaiting_admin_did, take the DID afterwards — because the admin DID is
	// minted locally by `pnm setup` from the VTA DID, which does not exist
	// until step_vta_setup has run. The only thing that differs from a user's
	// session is who may resume it: any admin, through
	// POST /admin/setup-sessions/:id/admin, rather than the one owning user.
	VtaImage      string `json:"vta_image"      binding:"required"`
	MediatorImage string `json:"mediator_image" binding:"required"`
	DidsImage     string `json:"dids_image"     binding:"required"`
	VtcImage      string `json:"vtc_image"      binding:"required"`

	Portable         *bool `json:"portable"`
	PreRotationCount *int  `json:"pre_rotation_count"`
}

// CreatePlatformStack — POST /api/v1/admin/platform-stack (admin only).
//
// Creates the domain row, the four proxied Cloudflare A records, and the
// full_stack session against them, then starts the orchestrator. beta_access
// does not apply (that gate is for users); cluster capacity does — the stack
// consumes the same resources as any other full stack, and an admin needs to
// know if it will not fit rather than have it silently over-commit.
//
// From here the session runs the ordinary full_stack pipeline and parks at
// awaiting_admin_did like any other. The one difference is who resumes it:
// every admin, not one owning user.
func (h *SetupHandler) CreatePlatformStack(c *gin.Context) {
	if !h.cfRequired(c) {
		return
	}
	if h.k8s == nil || h.orch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s/orchestrator not configured"})
		return
	}
	if h.ingressIP == "" || h.clusterDomain == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "cluster not configured: CLUSTER_INGRESS_IP and CLUSTER_DOMAIN must be set"})
		return
	}

	var req createPlatformStackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	label := req.Label
	if label == "" {
		label = platformLabelDefault
	}
	if err := setup.ValidateName(label); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid label: " + err.Error()})
		return
	}

	existing, err := h.platformDomain()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read platform domain"})
		return
	}
	if existing != nil {
		var inUse int64
		h.db.Model(&model.SetupSession{}).Where("domain_id = ?", existing.ID).Count(&inUse)
		if inUse > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "a platform stack already exists — delete it first (this takes every vta_only session's mediator and DID host with it)"})
			return
		}
	}

	if !h.capacityAllows(c, capacity.FullStack) {
		return
	}

	owner, err := h.systemAccount()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to provision the system account"})
		return
	}

	// The domain row survives session teardown, so a rebuild reuses it rather
	// than minting a second row for the same name (domains_domain_unique would
	// reject that anyway).
	domain := existing
	if domain == nil {
		now := time.Now()
		domain = &model.Domain{
			UserID: owner.ID,
			Domain: h.clusterDomain,
			Kind:   model.DomainKindPlatform,
			// Verified on insert: we control this zone, so there is nothing
			// for anyone to prove. No TXT token, and no ACME either — the
			// *.{CLUSTER_DOMAIN} wildcard already covers these names.
			VerifiedAt: &now,
		}
		if err := h.db.Create(domain).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist the platform domain"})
			return
		}
	}

	vtaSub, mediatorSub, didsSub, vtcSub := setup.FixedHosts(h.appEnv)
	vtaFQDN := vtaSub + "." + h.clusterDomain
	mediatorFQDN := mediatorSub + "." + h.clusterDomain
	didsFQDN := didsSub + "." + h.clusterDomain
	vtcFQDN := vtcSub + "." + h.clusterDomain

	var created []string
	rollback := func() {
		for _, rec := range created {
			_ = h.cf.DeleteRecord(c.Request.Context(), rec)
		}
	}
	records := make(map[string]string, 4)
	for _, host := range []struct{ label, fqdn string }{
		{"vta", vtaFQDN}, {"mediator", mediatorFQDN}, {"dids", didsFQDN}, {"vtc", vtcFQDN},
	} {
		rec, err := h.cf.CreateARecord(c.Request.Context(), host.fqdn, h.ingressIP)
		if err != nil {
			rollback()
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to create DNS record (" + host.label + "): " + err.Error()})
			return
		}
		created = append(created, rec)
		records[host.label] = rec
	}

	portable := true
	if req.Portable != nil {
		portable = *req.Portable
	}
	preRotationCount := 1
	if req.PreRotationCount != nil {
		preRotationCount = *req.PreRotationCount
	}

	recordMediator, recordDids, recordVtc := records["mediator"], records["dids"], records["vtc"]
	session := model.SetupSession{
		UserID:     owner.ID,
		Mode:       model.ModeFullStack,
		Status:     "dns_provision",
		DomainID:   &domain.ID,
		DomainType: model.DomainPlatform,
		Domain:     h.clusterDomain,
		// The VTA reuses Subdomain/CFRecordID, same as every other full_stack.
		Subdomain:         vtaSub,
		CFRecordID:        records["vta"],
		MediatorSubdomain: mediatorSub,
		DidsSubdomain:     didsSub,
		VtcSubdomain:      vtcSub,
		CFRecordMediator:  &recordMediator,
		CFRecordDids:      &recordDids,
		CFRecordVtc:       &recordVtc,
		// One label stands in for both names on a fixed-label domain: neither
		// reaches a hostname, and the did:webvh paths they do reach are already
		// distinct by their -vta / -mediator / -vtc suffixes (design §4.3).
		VtaName: label,
		VtcName: label,
		// The stack's own daemon is also the shared one every vta_only session
		// uploads to, so this value is what puts those rows in one namespace.
		DidHost:          didsFQDN,
		VtaImage:         req.VtaImage,
		MediatorImage:    req.MediatorImage,
		DidsImage:        req.DidsImage,
		VtcImage:         req.VtcImage,
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
		rollback()
		// Races with a concurrent create of the same stack; the partial unique
		// index on domain_id is the real gate.
		if strings.Contains(createErr.Error(), "setup_sessions_domain_unique") {
			c.JSON(http.StatusConflict, gin.H{"error": "a platform stack already exists"})
			return
		}
		log.Printf("[platform] failed to persist session: %v", createErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist session"})
		return
	}

	log.Printf("[platform] created stack: session %d (%s), label %q, owner user %d",
		session.ID, session.UniqueId, label, owner.ID)

	h.orch.Start(session.ID)

	c.JSON(http.StatusCreated, gin.H{
		"id":     session.UniqueId,
		"status": session.Status,
		"label":  label,
		"domain": h.clusterDomain,
		"urls":   platformURLs(&session),
	})
}

// platformURLs is the four public URLs of a fixed-label stack.
func platformURLs(s *model.SetupSession) gin.H {
	return gin.H{
		"vta":      s.PublicURL(),
		"mediator": "https://" + s.MediatorFQDN(),
		"dids":     "https://" + s.DidsFQDN(),
		"vtc":      "https://" + s.VtcFQDN(),
	}
}

// GetPlatformStack — GET /api/v1/admin/platform-stack (admin only).
//
// Reports whether the stack exists and how far it has got, plus the values the
// admin has to copy into configuration once it is running (design §3.3.4):
// MEDIATOR_DID and the DID-hosting URLs can only be known after the pipeline
// mints them.
func (h *SetupHandler) GetPlatformStack(c *gin.Context) {
	domain, err := h.platformDomain()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read platform domain"})
		return
	}
	if domain == nil {
		c.JSON(http.StatusOK, gin.H{"exists": false})
		return
	}

	var session model.SetupSession
	err = h.db.Where("domain_id = ?", domain.ID).First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// The domain row outlives its session, so this is the state after a
		// teardown: the name is still ours, nothing is running on it.
		c.JSON(http.StatusOK, gin.H{"exists": false, "domain": domain.Domain})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read platform session"})
		return
	}

	resp := gin.H{
		"exists": true,
		"id":     session.UniqueId,
		"status": session.Status,
		"label":  session.VtaName,
		"domain": domain.Domain,
		"urls":   platformURLs(&session),
		// vta_did is what the admin feeds to `pnm setup` locally to mint the
		// admin DID this stack parks waiting for — so the page can't ask for
		// that DID without showing this one first.
		"collected": gin.H{
			"vta_did":               session.VtaDid,
			"mediator_did":          session.MediatorDid,
			"did_hosting_did":       session.DIDHostingDid,
			"mediator_admin_did":    session.MediatorAdminDid,
			"did_hosting_admin_did": session.DIDHostingAdminDid,
			"vtc_did":               session.VtcDid,
		},
		"images": gin.H{
			"vta":      session.VtaImage,
			"mediator": session.MediatorImage,
			"dids":     session.DidsImage,
			"vtc":      session.VtcImage,
		},
		// What to paste into configuration. Empty until the pipeline mints
		// them, which is why the admin page has to surface them rather than
		// the values being known upfront.
		"config_values": gin.H{
			"MEDIATOR_DID":            session.MediatorDid,
			"DID_HOSTING_SERVER_URL":  "https://" + session.DidsFQDN(),
			"DID_HOSTING_CONTROL_URL": "https://" + session.DidsFQDN(),
			"DID_HOSTING_DID":         session.DIDHostingDid,
		},
		"created_at": session.CreatedAt,
		"updated_at": session.UpdatedAt,
	}

	// The same post-provisioning outputs a user gets on GET /setup/:id. They
	// are not decoration: without the VTC install URL and its claim code nobody
	// can claim the platform community, and the two admin keys are shown for
	// offline backup and exist nowhere else the admin can reach.
	resp["dids_enroll_used"] = session.DidsEnrollUsed
	resp["vtc_install_used"] = session.VtcInstallUsed

	actionRequired := gin.H{}
	if session.DidsEnrollURL != "" && !session.DidsEnrollUsed {
		actionRequired["dids_admin_enroll_url"] = session.DidsEnrollURL
	}
	if session.VtcInstallURL != "" && !session.VtcInstallUsed {
		actionRequired["install_url"] = session.VtcInstallURL
		actionRequired["claim_code"] = session.VtcClaimCode
	}
	if session.MediatorAdminKey != "" || session.WebvhAdminKey != "" {
		actionRequired["reveal_keys_once"] = true
		if session.MediatorAdminKey != "" {
			resp["mediator_admin_key"] = session.MediatorAdminKey
		}
		if session.WebvhAdminKey != "" {
			resp["webvh_admin_key"] = session.WebvhAdminKey
		}
	}
	if len(actionRequired) > 0 {
		resp["action_required"] = actionRequired
	}

	if session.ErrorMsg != "" {
		resp["error_msg"] = session.ErrorMsg
	}
	c.JSON(http.StatusOK, resp)
}
