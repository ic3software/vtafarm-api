package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/setup"
)

// This file holds the full_stack-mode branches of the setup handlers.
// setup.go's vta_only logic is left unchanged; each public handler there
// dispatches here on session.Mode == model.ModeFullStack.

// shellQuote single-quotes a value for safe interpolation into a `sh -c`
// command string (ReissueDidsEnroll only — the orchestrator has its own
// copy for its own Job commands; duplicated rather than exported to keep
// internal/setup and internal/handler decoupled).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// strVal dereferences a nullable model field, returning "" for nil — the
// full_stack output/image columns are *string (NULL until set; see
// model.SetupSession).
func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// createFullStack handles POST /api/v1/setup for mode=full_stack.
//
// On the managed zone it creates the four Cloudflare A-records up front, then
// persists the session and starts the orchestrator. On a custom domain it
// creates no DNS at all — the records are the user's, already verified, and we
// never touch their zone.
//
// domain is nil for a managed session; Create has already validated the names
// against whichever form applies.
func (h *SetupHandler) createFullStack(c *gin.Context, req createSetupRequest, domain *model.Domain) {
	if h.k8s == nil || h.orch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s/orchestrator not configured"})
		return
	}

	// vta_name has already been validated by Create; vtc_name is validated
	// here because it's full_stack's own input. It becomes the vtc-<name>
	// subdomain, so it gets the same treatment — required, DNS-safe, and
	// unique across all sessions (vta_only rows carry the column default '').
	// None of that applies on a custom domain, where the single label already
	// stands in for both names.
	if domain == nil {
		if req.VtcName == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "vtc_name is required"})
			return
		}
		if err := setup.ValidateName(req.VtcName); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid vtc_name: " + err.Error()})
			return
		}
		var vtcNameTaken int64
		h.db.Model(&model.SetupSession{}).
			Where("vtc_name = ? AND domain_type = ?", req.VtcName, model.DomainManaged).
			Count(&vtcNameTaken)
		if vtcNameTaken > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "vtc_name already in use"})
			return
		}
	}

	portable := true
	if req.Portable != nil {
		portable = *req.Portable
	}
	preRotationCount := 1
	if req.PreRotationCount != nil {
		preRotationCount = *req.PreRotationCount
	}

	if req.MediatorImage == "" || req.DidsImage == "" || req.VtcImage == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "full_stack requires mediator_image, dids_image and vtc_image (select from GET /setup/images?component=mediator|dids|vtc)",
		})
		return
	}

	userID := c.MustGet(middleware.ContextUserID).(uint)

	// Which zone, and which label shape. A custom domain uses the four fixed
	// labels — no user-chosen name reaches a hostname — and one free-form
	// label stands in for both names (design §4.3).
	zone := h.clusterDomain
	vtaName, vtcName := req.VtaName, req.VtcName
	vtaSub, mediatorSub, didsSub, vtcSub := setup.FullStackHosts(h.appEnv, req.VtaName, req.VtcName)
	if domain != nil {
		zone = domain.Domain
		vtaName, vtcName = req.Label, req.Label
		vtaSub, mediatorSub, didsSub, vtcSub = setup.FixedHosts(h.appEnv)
	}
	vtaFQDN := vtaSub + "." + zone
	mediatorFQDN := mediatorSub + "." + zone
	didsFQDN := didsSub + "." + zone
	vtcFQDN := vtcSub + "." + zone

	// All four managed records are created up front (design §3) — the rendered
	// recipes embed the final HTTPS URLs. Roll back everything created so
	// far on the first failure.
	//
	// A custom domain creates nothing: the user made those records themselves
	// and we verified them, so there is no DNS of ours to roll back — or, on
	// teardown, to delete.
	var created []string
	rollback := func() {
		for _, rec := range created {
			_ = h.cf.DeleteRecord(c.Request.Context(), rec)
		}
	}
	records := make(map[string]string, 4)
	if domain == nil {
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
	}

	domainType := model.DomainManaged
	var domainID *uint
	if domain != nil {
		domainType = model.DomainCustom
		domainID = &domain.ID
	}

	recordMediator, recordDids, recordVtc := records["mediator"], records["dids"], records["vtc"]
	session := model.SetupSession{
		UserID: userID,
		Mode:   model.ModeFullStack,
		Status: "dns_provision",
		// Explicit, not left to the column default — GORM omits a zero value
		// that carries a default tag, so the in-memory struct would read "" for
		// the rest of this request.
		DomainID:   domainID,
		DomainType: domainType,
		Domain:     zone,
		// VTA reuses Subdomain/CFRecordID — same fields vta_only uses.
		Subdomain:         vtaSub,
		CFRecordID:        records["vta"],
		MediatorSubdomain: mediatorSub,
		DidsSubdomain:     didsSub,
		VtcSubdomain:      vtcSub,
		CFRecordMediator:  &recordMediator,
		CFRecordDids:      &recordDids,
		CFRecordVtc:       &recordVtc,
		VtaName:           vtaName,
		VtcName:           vtcName,
		VtaImage:          req.VtaImage,
		MediatorImage:     req.MediatorImage,
		DidsImage:         req.DidsImage,
		VtcImage:          req.VtcImage,
		AdminDid:          req.AdminDid,
		Portable:          portable,
		PreRotationCount:  preRotationCount,
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
		// The pre-insert count checks race with concurrent creates; the DB
		// unique indexes are the real gate.
		if isUniqueViolation(createErr, "setup_sessions_vta_name_unique") {
			c.JSON(http.StatusConflict, gin.H{"error": "vta_name already in use"})
			return
		}
		if isUniqueViolation(createErr, "setup_sessions_vtc_name_unique") {
			c.JSON(http.StatusConflict, gin.H{"error": "vtc_name already in use"})
			return
		}
		if isUniqueViolation(createErr, "setup_sessions_domain_unique") {
			c.JSON(http.StatusConflict, gin.H{"error": "this domain is already in use by another session"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist session"})
		return
	}

	h.orch.Start(session.ID)

	c.JSON(http.StatusCreated, gin.H{
		"id":          session.UniqueId,
		"status":      session.Status,
		"domain_type": session.DomainType,
		"domain":      session.Domain,
		"urls": gin.H{
			"vta":      "https://" + vtaFQDN,
			"mediator": "https://" + mediatorFQDN,
			"dids":     "https://" + didsFQDN,
			"vtc":      "https://" + vtcFQDN,
		},
	})
}

// getFullStack implements GET /api/v1/setup/:id's full_stack response shape
// (design §12) — four URLs, the collected DIDs, the reveal-once admin keys and
// VTC install credentials, and the two ack flags.
func (h *SetupHandler) getFullStack(c *gin.Context, session *model.SetupSession) {
	urls := gin.H{
		"vta":      session.PublicURL(),
		"mediator": "https://" + session.MediatorFQDN(),
		"dids":     "https://" + session.DidsFQDN(),
		"vtc":      "https://" + session.VtcFQDN(),
	}
	collected := gin.H{
		"vta_did":               session.VtaDid,
		"mediator_did":          session.MediatorDid,
		"did_hosting_did":       session.DIDHostingDid,
		"mediator_admin_did":    session.MediatorAdminDid,
		"did_hosting_admin_did": session.DIDHostingAdminDid,
		"vtc_did":               session.VtcDid,
	}
	resp := gin.H{
		"id":          session.UniqueId,
		"mode":        session.Mode,
		"domain_type": session.DomainType,
		"domain":      session.Domain,
		"status":      session.Status,
		"urls":        urls,
		"collected":   collected,
		// Current images per component — the self-service upgrade UI shows
		// these as the running versions.
		"vta_image":      session.VtaImage,
		"mediator_image": session.MediatorImage,
		"dids_image":     session.DidsImage,
		"vtc_image":      session.VtcImage,
		"created_at":     session.CreatedAt,
		"updated_at":     session.UpdatedAt,
	}

	resp["dids_enroll_used"] = session.DidsEnrollUsed
	resp["vtc_install_used"] = session.VtcInstallUsed

	actionRequired := gin.H{}
	if session.DidsEnrollURL != "" && !session.DidsEnrollUsed {
		actionRequired["dids_admin_enroll_url"] = session.DidsEnrollURL
	}
	// Single-shot like the dids enroll URL — once acked, stop offering a dead
	// link; reissue-install mints a fresh pair.
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

// deleteFullStack implements the full_stack branch of DELETE
// /api/v1/setup/:id (design §13). orch.Cancel has already been called by
// the caller (setup.go's Delete). Each step is best-effort so one failure
// doesn't strand the rest of the teardown.
func (h *SetupHandler) deleteFullStack(c *gin.Context, session *model.SetupSession) {
	ctx := c.Request.Context()

	// A custom domain's records belong to the user: we never created them and
	// have no access to their zone. Removing the four CNAMEs is theirs to do,
	// and the UI has to say so — a record left aimed at our ingress is a
	// dangling-DNS liability.
	if h.cf != nil && session.OwnsDNS() {
		// Empty-skip covers a session torn down before every record was
		// created (createFullStack rolls back mid-loop failures itself).
		for label, rec := range map[string]string{"vta": session.CFRecordID, "mediator": strVal(session.CFRecordMediator), "dids": strVal(session.CFRecordDids), "vtc": strVal(session.CFRecordVtc)} {
			if rec == "" {
				continue
			}
			if err := h.cf.DeleteRecord(ctx, rec); err != nil {
				log.Printf("[setup] warn: failed to delete DNS record (%s) for session %d: %v", label, session.ID, err)
			}
		}
	}

	if h.k8s != nil {
		ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))

		if h.orch != nil {
			h.orch.TeardownMediatorVault(ctx, session.UserID, session.ID)
			h.orch.TeardownDidsVault(ctx, session.UserID, session.ID)
			h.orch.TeardownVtcVault(ctx, session.UserID, session.ID)
		}

		h.k8s.DeleteAllComponentJobs(ctx, ns, session.ID)
		h.k8s.DeleteComponentResources(ctx, ns, k8s.FSVtaName(session.ID))
		h.k8s.DeleteComponentResources(ctx, ns, k8s.FSMediatorName(session.ID))
		h.k8s.DeleteComponentResources(ctx, ns, k8s.FSDidsName(session.ID))
		h.k8s.DeleteComponentResources(ctx, ns, k8s.FSVtcName(session.ID))

		// The Certificate goes; **its Secret deliberately stays**. Recreating a
		// session on the same domain asks for the exact same four names, which
		// is what Let's Encrypt's "5 per identical set per 7 days" limit counts
		// — and that one cannot be raised. Leaving the Secret means a rebuild
		// finds a valid certificate and cert-manager reuses it, at no ACME cost.
		// Namespace deletion below collects it when the user's last session goes.
		if session.DomainType == model.DomainCustom {
			if err := h.k8s.DeleteSessionCert(ctx, ns, k8s.FSTLSSecret(session.ID)); err != nil {
				log.Printf("[setup] warn: failed to delete certificate for session %d: %v", session.ID, err)
			}
		}
	}

	if h.orch != nil {
		h.orch.TeardownVaultSeed(ctx, session.UserID, session.ID)
	}

	if err := h.db.Delete(session).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete session"})
		return
	}

	if h.k8s != nil {
		var remaining int64
		h.db.Model(&model.SetupSession{}).Where("user_id = ?", session.UserID).Count(&remaining)
		if remaining == 0 {
			_ = h.k8s.DeleteNamespace(ctx, fmt.Sprintf("%d", session.UserID))
			if h.orch != nil {
				h.orch.TeardownVaultUserAccess(ctx, session.UserID)
			}
		}
	}

	c.Status(http.StatusNoContent)
}

// logsFullStack implements GET /api/v1/setup/:id/logs for full_stack
// sessions — ?source= gains the values listed in design §12, defaulting to
// whichever step session.Status indicates is current.
func (h *SetupHandler) logsFullStack(c *gin.Context, session *model.SetupSession) {
	ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
	sid := session.ID

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	emit := func(line string) {
		fmt.Fprintf(c.Writer, "data: %s\n\n", line)
		c.Writer.Flush()
	}
	sseErr := func(err error) {
		if err != nil && c.Request.Context().Err() == nil {
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", err.Error())
			c.Writer.Flush()
		}
	}
	streamPod := func(component string) {
		selector := fmt.Sprintf("app=fs-%s,session-id=%d", component, sid)
		sseErr(h.k8s.StreamComponentPodLogs(c.Request.Context(), ns, selector, true, emit))
	}
	streamJob := func(jobName string) {
		sseErr(h.k8s.StreamJobLogs(c.Request.Context(), ns, jobName, emit))
	}

	source := c.Query("source")
	if source == "" {
		switch session.Status {
		case "step_vta_setup":
			source = "vta_setup"
		case "step_mediator_p1":
			source = "mediator_p1"
		case "step_mediator_reprov":
			source = "mediator_reprov"
		case "step_mediator_p2":
			source = "mediator_p2"
		case "step_dids_p1":
			source = "dids_p1"
		case "step_dids_provision":
			source = "dids_provision"
		case "step_dids_p2":
			source = "dids_p2"
		case "step_dids_invite":
			source = "dids_invite"
		case "step_dids_load_did":
			source = "dids_load_did"
		case "step_vta_register_dids":
			source = "vta_register_dids"
		case "step_import_admin_did":
			source = "import_admin_did"
		case "step_vtc_setup_key":
			source = "vtc_setup_key"
		case "step_vtc_acl_grant":
			source = "vtc_acl_grant"
		case "step_vtc_setup":
			source = "vtc_setup"
		case "deploy_vtc":
			source = "vtc"
		case "deploy_vta":
			source = "vta"
		case "running":
			// The VTC is the last component to come up, so its log is the
			// one worth tailing once the session is done.
			source = "vtc"
		case "deploy_mediator", "awaiting_admin_did":
			source = "mediator"
		case "deploy_dids":
			source = "dids"
		default:
			source = "vta_setup"
		}
	}

	switch source {
	case "vta_setup":
		streamJob(k8s.FSJobVtaSetup(sid))
	case "mediator_p1":
		streamJob(k8s.FSJobMediatorP1(sid))
	case "mediator_reprov":
		streamJob(k8s.FSJobMediatorReprov(sid))
	case "mediator_p2":
		streamJob(k8s.FSJobMediatorP2(sid))
	case "dids_p1":
		streamJob(k8s.FSJobDidsP1(sid))
	case "dids_provision":
		streamJob(k8s.FSJobDidsProvision(sid))
	case "dids_p2":
		streamJob(k8s.FSJobDidsP2(sid))
	case "dids_invite":
		streamJob(k8s.FSJobDidsInvite(sid))
	case "dids_load_did":
		streamJob(k8s.FSJobDidsLoadDid(sid))
	case "vta_register_dids":
		streamJob(k8s.FSJobVtaRegisterDids(sid))
	case "import_admin_did":
		streamJob(k8s.FSJobImportAdminDid(sid))
	case "vtc_setup_key":
		streamJob(k8s.FSJobVtcSetupKey(sid))
	case "vtc_acl_grant":
		streamJob(k8s.FSJobVtcAclGrant(sid))
	case "vtc_setup":
		streamJob(k8s.FSJobVtcSetup(sid))
	case "vta":
		streamPod("vta")
	case "mediator":
		streamPod("mediator")
	case "dids":
		streamPod("dids")
	case "vtc":
		streamPod("vtc")
	default:
		fmt.Fprintf(c.Writer, "event: error\ndata: unknown source %q\n\n", source)
		c.Writer.Flush()
	}

	fmt.Fprintf(c.Writer, "event: done\ndata: stream ended\n\n")
	c.Writer.Flush()
}

// ReissueDidsEnroll handles POST /api/v1/setup/:id/dids/reissue-enroll
// (design §12, optional endpoint) — regenerates the single-use dids admin
// enrollment URL by re-running `did-hosting-daemon invite`.
//
// The dids daemon holds an exclusive lock on its local (Fjall) store for as
// long as it's running, so the invite Job can't open the same store
// concurrently (fails with "store error: FjallError: Locked"). This scales
// the dids Deployment to 0, waits for its pod to actually terminate (the
// lock isn't released until then), runs the invite Job, then scales back to
// 1 — via defer, so the daemon comes back up even if the Job itself fails.
func (h *SetupHandler) ReissueDidsEnroll(c *gin.Context) {
	publicID := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("unique_id = ? AND user_id = ?", publicID, userID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if !session.IsFullStack() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reissue-enroll is only available for full-stack sessions"})
		return
	}
	if session.DIDHostingAdminDid == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "dids daemon not provisioned yet"})
		return
	}
	if h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s not configured"})
		return
	}

	ctx := c.Request.Context()
	ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
	didsName := k8s.FSDidsName(session.ID)
	didsSelector := fmt.Sprintf("app=fs-dids,session-id=%d", session.ID)
	jobName := k8s.FSJobDidsInvite(session.ID)

	if err := h.k8s.ScaleComponentDeployment(ctx, ns, didsName, 0); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to stop dids daemon: " + err.Error()})
		return
	}
	// Restart the daemon on the way out no matter how this handler returns —
	// a failed reissue shouldn't leave the dids service down.
	defer func() {
		restartCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := h.k8s.ScaleComponentDeployment(restartCtx, ns, didsName, 1); err != nil {
			log.Printf("[setup] error: failed to restart dids daemon for session %d: %v", session.ID, err)
			return
		}
		if err := h.k8s.WaitForComponentDeploymentReady(restartCtx, ns, didsName, 2*time.Minute); err != nil {
			log.Printf("[setup] warn: dids daemon not ready after reissue for session %d: %v", session.ID, err)
		}
	}()

	if err := h.k8s.WaitForComponentPodsGone(ctx, ns, didsSelector, 2*time.Minute); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed waiting for dids daemon to stop: " + err.Error()})
		return
	}

	h.k8s.DeleteComponentJob(ctx, ns, jobName) // clear a previous (TTL'd) run if still present

	cmd := fmt.Sprintf("did-hosting-daemon invite --role admin --did %s", shellQuote(session.DIDHostingAdminDid))
	if err := h.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          session.DidsImage,
		Command:        []string{"sh", "-c", cmd},
		WorkingDir:     "/work/dids",
		ServiceAccount: k8s.PodOperatorServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "dids-data", ClaimName: k8s.FSDidsName(session.ID), MountPath: "/work/dids"}},
	}); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to create reissue job: " + err.Error()})
		return
	}

	succeeded, failMsg, err := h.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "job watch error: " + err.Error()})
		return
	}
	if !succeeded {
		c.JSON(http.StatusBadGateway, gin.H{"error": "reissue job failed: " + failMsg})
		return
	}
	logs, err := h.k8s.JobLogs(ctx, ns, jobName)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read job logs: " + err.Error()})
		return
	}
	enrollURL, err := setup.ParseDidsEnrollURL(logs)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "enrollment URL not found in job output"})
		return
	}

	h.db.Model(&model.SetupSession{}).Where("id = ?", session.ID).Updates(map[string]any{
		"dids_enroll_url":  enrollURL,
		"dids_enroll_used": false,
	})
	c.JSON(http.StatusOK, gin.H{"dids_admin_enroll_url": enrollURL})
}

// AckDidsEnroll handles POST /api/v1/setup/:id/dids/enroll-ack — the
// frontend calls this the instant the user opens the (single-use) dids
// enrollment URL. The daemon itself already refuses a second use of the
// same invite; this just lets the UI know not to offer that same link again
// after a refresh — reissue-enroll is what mints a fresh one.
func (h *SetupHandler) AckDidsEnroll(c *gin.Context) {
	publicID := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("unique_id = ? AND user_id = ?", publicID, userID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if !session.IsFullStack() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enroll-ack is only available for full-stack sessions"})
		return
	}
	if session.DidsEnrollURL == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "no dids enrollment url has been issued yet"})
		return
	}

	if err := h.db.Model(&session).Update("dids_enroll_used", true).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update session"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"dids_enroll_used": true})
}
