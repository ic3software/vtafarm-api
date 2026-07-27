package handler

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/dnscheck"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/setup"
)

// The Domains resource: a user proves control of a zone they own, once, on its
// own page — and only then can create a session against it.
//
// Keeping this separate from session creation is what removes an entire state
// from the session state machine. The alternative parked a half-built session
// in `awaiting_dns` while the user edited DNS, which meant it held a name
// reservation, needed a capacity re-check when it finally started, and had to
// be garbage collected if abandoned. Here a session is only ever created
// against DNS that is already live.

type DomainHandler struct {
	db            *gorm.DB
	appEnv        string
	clusterDomain string
	ingressIP     string
	checker       *dnscheck.Checker
}

func NewDomainHandler(db *gorm.DB, appEnv, clusterDomain, ingressIP string) *DomainHandler {
	return &DomainHandler{
		db:            db,
		appEnv:        appEnv,
		clusterDomain: clusterDomain,
		ingressIP:     ingressIP,
		checker:       dnscheck.New(),
	}
}

// recordSpec is one row of the "create these records" table. expected_value is
// always the CNAME target — never an IP — because the user's records are
// effectively permanent (their DID-hosting hostname is baked into every
// did:webvh the session mints), so they must point at a name we control and
// can repoint later without anyone touching their DNS again.
type recordSpec struct {
	Component     string   `json:"component"`
	FQDN          string   `json:"fqdn"`
	ExpectedType  string   `json:"expected_type"`
	ExpectedValue string   `json:"expected_value"`
	Resolved      []string `json:"resolved"`
	CNAME         string   `json:"cname,omitempty"`
	OK            bool     `json:"ok"`
	Detail        string   `json:"detail,omitempty"`
}

type domainResponse struct {
	ID         uint       `json:"id"`
	Domain     string     `json:"domain"`
	Kind       string     `json:"kind"`
	Verified   bool       `json:"verified"`
	VerifiedAt *time.Time `json:"verified_at"`
	// InUseBy is the unique_id of the session running on this domain, if any.
	// A domain backs at most one — its four labels are fixed.
	InUseBy string `json:"in_use_by,omitempty"`
	// Target is what all four CNAMEs point at.
	Target string `json:"target"`
	// Checked distinguishes "we looked and it failed" from "we haven't looked".
	// The list endpoint never resolves anything, so its rows report false and
	// the UI shows a neutral "not checked yet" rather than a red cross.
	Checked bool                `json:"checked"`
	TXT     *dnscheck.TxtResult `json:"txt,omitempty"`
	Records []recordSpec        `json:"records"`
}

// componentOrder is the order everything in this codebase lists components in.
var componentOrder = []string{"vta", "mediator", "dids", "vtc"}

// isUniqueViolation reports whether err came from the named unique index. The
// driver doesn't surface constraint names structurally, so matching the text is
// the available option — which is why migration 000021 recreates the session
// name indexes under their original names rather than renaming them.
func isUniqueViolation(err error, index string) bool {
	return err != nil && strings.Contains(err.Error(), index)
}

// specs builds the four CNAME rows for a domain, with no resolution performed.
func (h *DomainHandler) specs(domain string) []recordSpec {
	target := setup.CNAMETarget(h.appEnv, h.clusterDomain)
	hosts := make([]string, 4)
	hosts[0], hosts[1], hosts[2], hosts[3] = setup.CustomHosts(h.appEnv, domain)

	out := make([]recordSpec, len(componentOrder))
	for i, component := range componentOrder {
		out[i] = recordSpec{
			Component:     component,
			FQDN:          hosts[i],
			ExpectedType:  "CNAME",
			ExpectedValue: target,
			Resolved:      []string{},
		}
	}
	return out
}

// inUseBy returns the unique_id of the session on this domain, or "".
func (h *DomainHandler) inUseBy(domainID uint) string {
	var s model.SetupSession
	if err := h.db.Where("domain_id = ?", domainID).First(&s).Error; err != nil {
		return ""
	}
	return s.UniqueId
}

// respond builds the payload without touching DNS.
func (h *DomainHandler) respond(d *model.Domain) domainResponse {
	return domainResponse{
		ID:         d.ID,
		Domain:     d.Domain,
		Kind:       d.Kind,
		Verified:   d.Verified(),
		VerifiedAt: d.VerifiedAt,
		InUseBy:    h.inUseBy(d.ID),
		Target:     setup.CNAMETarget(h.appEnv, h.clusterDomain),
		Records:    h.specs(d.Domain),
	}
}

// check resolves all five records and, when they all pass, promotes the domain
// to verified.
//
// An already-verified domain short-circuits without a single lookup. That is
// deliberate: control is checked once, at verification time, and never
// re-checked — otherwise the first time a user tidied their DNS we would break
// a running session, and the TXT record we told them they could delete would
// silently become load-bearing.
func (h *DomainHandler) check(c *gin.Context, d *model.Domain) domainResponse {
	resp := h.respond(d)
	if d.Verified() {
		resp.Checked = true
		return resp
	}

	ctx := c.Request.Context()
	fqdns := make([]string, len(resp.Records))
	for i, r := range resp.Records {
		fqdns[i] = r.FQDN
	}

	results := h.checker.CheckHosts(ctx, fqdns, h.ingressIP)
	allHostsOK := true
	for i, res := range results {
		resp.Records[i].Resolved = res.IPs
		if resp.Records[i].Resolved == nil {
			resp.Records[i].Resolved = []string{}
		}
		resp.Records[i].CNAME = res.CNAME
		resp.Records[i].OK = res.OK
		resp.Records[i].Detail = res.Detail
		if !res.OK {
			allHostsOK = false
		}
	}

	txt := h.checker.CheckTXT(ctx, setup.ChallengeName(d.Domain), d.Domain, d.VerifyToken)
	if txt.Found == nil {
		txt.Found = []string{}
	}
	resp.TXT = &txt
	resp.Checked = true

	if allHostsOK && txt.OK {
		now := time.Now()
		if err := h.db.Model(d).Update("verified_at", now).Error; err != nil {
			log.Printf("[domains] failed to persist verification of %s: %v", d.Domain, err)
		} else {
			d.VerifiedAt = &now
			resp.Verified = true
			resp.VerifiedAt = &now
			log.Printf("[domains] %s verified (domain id %d, user %d)", d.Domain, d.ID, d.UserID)
		}
	}
	return resp
}

// load fetches the caller's domain by path id. It writes the error response
// itself and returns nil when it does.
func (h *DomainHandler) load(c *gin.Context) *model.Domain {
	userID := c.MustGet(middleware.ContextUserID).(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "domain not found"})
		return nil
	}

	var d model.Domain
	// Scoped to the caller, so a wrong id is indistinguishable from someone
	// else's domain — the same treatment sessions get.
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&d).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "domain not found"})
		return nil
	}
	return &d
}

// List — GET /api/v1/domains.
//
// At most one row today (one custom domain per account), so this is a list for
// forward compatibility rather than because it ever paginates. No DNS lookups:
// the UI shows the records with a neutral "not checked" state and resolves them
// when the user opens the domain or presses Verify.
func (h *DomainHandler) List(c *gin.Context) {
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var domains []model.Domain
	if err := h.db.Where("user_id = ? AND kind = ?", userID, model.DomainKindCustom).
		Order("id asc").Find(&domains).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list domains"})
		return
	}

	out := make([]domainResponse, len(domains))
	for i := range domains {
		out[i] = h.respond(&domains[i])
	}
	c.JSON(http.StatusOK, out)
}

// Get — GET /api/v1/domains/:id.
//
// Resolves live, so the portal's background poll shows a ✓ appearing on its own
// once the user fixes their DNS in another tab. Cheap after success: a verified
// domain performs no lookups at all.
func (h *DomainHandler) Get(c *gin.Context) {
	d := h.load(c)
	if d == nil {
		return
	}
	c.JSON(http.StatusOK, h.check(c, d))
}

// Create — POST /api/v1/domains.
func (h *DomainHandler) Create(c *gin.Context) {
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var req struct {
		Domain string `json:"domain" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	normalized, err := setup.NormalizeDomain(req.Domain)
	if err == nil {
		err = setup.ValidateDomain(normalized, h.clusterDomain)
	}
	if err != nil {
		switch {
		case errors.Is(err, setup.ErrDomainIsOurs):
			c.JSON(http.StatusBadRequest, gin.H{
				"error": h.clusterDomain + " is managed by VTA Farm and can't be attached — choose the managed option instead",
			})
		case errors.Is(err, setup.ErrDomainIsHostname):
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "attach the domain itself (aaa.com), not one of the hostnames we create under it",
			})
		case errors.Is(err, setup.ErrDomainNotPublic):
			c.JSON(http.StatusBadRequest, gin.H{"error": "this domain can't be issued a public TLS certificate"})
		case errors.Is(err, setup.ErrDomainTooLong):
			c.JSON(http.StatusBadRequest, gin.H{"error": "domain too long"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid domain"})
		}
		return
	}

	// One custom domain per account, and a domain exists once globally. Both
	// are indexed, so these checks are for the message; the index is the gate.
	var mine int64
	h.db.Model(&model.Domain{}).Where("user_id = ? AND kind = ?", userID, model.DomainKindCustom).Count(&mine)
	if mine > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "only one custom domain per account"})
		return
	}

	d := model.Domain{
		UserID:      userID,
		Domain:      normalized,
		Kind:        model.DomainKindCustom,
		VerifyToken: setup.MintVerifyToken(),
	}
	if err := h.db.Create(&d).Error; err != nil {
		if isUniqueViolation(err, "domains_domain_unique") {
			c.JSON(http.StatusConflict, gin.H{"error": "this domain is already attached to another account"})
			return
		}
		if isUniqueViolation(err, "domains_one_custom_per_user") {
			c.JSON(http.StatusConflict, gin.H{"error": "only one custom domain per account"})
			return
		}
		log.Printf("[domains] failed to persist %s: %v", normalized, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to attach domain"})
		return
	}

	log.Printf("[domains] attached %s (domain id %d, user %d)", d.Domain, d.ID, userID)

	resp := h.respond(&d)
	// The challenge is returned unresolved: the records don't exist yet, and
	// checking now would only teach the public resolvers an NXDOMAIN the user
	// then has to wait out (§6.6).
	resp.TXT = &dnscheck.TxtResult{
		Name:     setup.ChallengeName(d.Domain),
		Expected: d.VerifyToken,
		Found:    []string{},
	}
	c.JSON(http.StatusCreated, resp)
}

// Verify — POST /api/v1/domains/:id/verify.
//
// A failing check answers 200, not 4xx: it is an expected, retryable state the
// UI renders record by record, not a client error.
func (h *DomainHandler) Verify(c *gin.Context) {
	d := h.load(c)
	if d == nil {
		return
	}
	c.JSON(http.StatusOK, h.check(c, d))
}

// Delete — DELETE /api/v1/domains/:id.
//
// Always allowed while nothing runs on it: a pending domain holds no cluster
// resources and reserves nothing but its row.
func (h *DomainHandler) Delete(c *gin.Context) {
	d := h.load(c)
	if d == nil {
		return
	}

	if sessionID := h.inUseBy(d.ID); sessionID != "" {
		c.JSON(http.StatusConflict, gin.H{
			"error": "this domain is in use by session " + sessionID + " — delete that agent first",
		})
		return
	}

	if err := h.db.Delete(d).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete domain"})
		return
	}
	log.Printf("[domains] deleted %s (domain id %d, user %d)", d.Domain, d.ID, d.UserID)

	// The user's four CNAMEs are still pointing at us — we never created them
	// and cannot remove them. The UI has to tell them to, since a record left
	// aimed at our ingress is exactly the dangling-DNS setup the per-attach
	// token exists to defuse.
	c.Status(http.StatusNoContent)
}
