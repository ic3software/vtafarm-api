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

// createFullStack handles POST /api/v1/setup for mode=full_stack — creates
// three Cloudflare A-records (vta/mediator/dids) up front, persists the
// session, and starts the orchestrator's full_stack state machine.
func (h *SetupHandler) createFullStack(c *gin.Context, req createSetupRequest) {
	if h.k8s == nil || h.orch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s/orchestrator not configured"})
		return
	}

	if req.VtaName == "" {
		req.VtaName = "personal-vta"
	}
	portable := true
	if req.Portable != nil {
		portable = *req.Portable
	}
	preRotationCount := 1
	if req.PreRotationCount != nil {
		preRotationCount = *req.PreRotationCount
	}

	if req.MediatorImage == "" || req.DidsImage == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "full_stack requires mediator_image and dids_image (select from GET /setup/images?component=mediator|dids)",
		})
		return
	}

	userID := c.MustGet(middleware.ContextUserID).(uint)

	var existingName int64
	h.db.Model(&model.SetupSession{}).Where("user_id = ? AND vta_name = ?", userID, req.VtaName).Count(&existingName)
	if existingName > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "vta_name already in use"})
		return
	}

	vtaSub, mediatorSub, didsSub := setup.FullStackHosts(h.appEnv)
	vtaFQDN := vtaSub + "." + h.clusterDomain
	mediatorFQDN := mediatorSub + "." + h.clusterDomain
	didsFQDN := didsSub + "." + h.clusterDomain

	recordVta, err := h.cf.CreateARecord(c.Request.Context(), vtaFQDN, h.ingressIP)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to create DNS record (vta): " + err.Error()})
		return
	}
	recordMediator, err := h.cf.CreateARecord(c.Request.Context(), mediatorFQDN, h.ingressIP)
	if err != nil {
		_ = h.cf.DeleteRecord(c.Request.Context(), recordVta)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to create DNS record (mediator): " + err.Error()})
		return
	}
	recordDids, err := h.cf.CreateARecord(c.Request.Context(), didsFQDN, h.ingressIP)
	if err != nil {
		_ = h.cf.DeleteRecord(c.Request.Context(), recordVta)
		_ = h.cf.DeleteRecord(c.Request.Context(), recordMediator)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to create DNS record (dids): " + err.Error()})
		return
	}

	session := model.SetupSession{
		UserID: userID,
		Mode:   model.ModeFullStack,
		Status: "dns_provision",
		Domain: h.clusterDomain,
		// VTA reuses Subdomain/CFRecordID — same fields vta_only uses.
		Subdomain:         vtaSub,
		CFRecordID:        recordVta,
		MediatorSubdomain: mediatorSub,
		DidsSubdomain:     didsSub,
		CFRecordMediator:  &recordMediator,
		CFRecordDids:      &recordDids,
		VtaName:           req.VtaName,
		VtaImage:          req.VtaImage,
		MediatorImage:     req.MediatorImage,
		DidsImage:         req.DidsImage,
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
		_ = h.cf.DeleteRecord(c.Request.Context(), recordVta)
		_ = h.cf.DeleteRecord(c.Request.Context(), recordMediator)
		_ = h.cf.DeleteRecord(c.Request.Context(), recordDids)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist session"})
		return
	}

	h.orch.Start(session.ID)

	c.JSON(http.StatusCreated, gin.H{
		"id":     session.UniqueId,
		"status": session.Status,
		"urls": gin.H{
			"vta":      "https://" + vtaFQDN,
			"mediator": "https://" + mediatorFQDN,
			"dids":     "https://" + didsFQDN,
		},
	})
}

// getFullStack implements GET /api/v1/setup/:id's full_stack response shape
// (design §12), extended in place for full_stack_with_vtc (vtc design §13):
// a fourth URL, collected.vtc_did, the reveal-once install credentials, and
// vtc_install_used.
func (h *SetupHandler) getFullStack(c *gin.Context, session *model.SetupSession) {
	urls := gin.H{
		"vta":      session.PublicURL(),
		"mediator": "https://" + session.MediatorFQDN(),
		"dids":     "https://" + session.DidsFQDN(),
	}
	collected := gin.H{
		"vta_did":               session.VtaDid,
		"mediator_did":          session.MediatorDid,
		"did_hosting_did":       session.DIDHostingDid,
		"mediator_admin_did":    session.MediatorAdminDid,
		"did_hosting_admin_did": session.DIDHostingAdminDid,
	}
	resp := gin.H{
		"id":         session.UniqueId,
		"mode":       session.Mode,
		"status":     session.Status,
		"urls":       urls,
		"collected":  collected,
		"created_at": session.CreatedAt,
		"updated_at": session.UpdatedAt,
	}

	resp["dids_enroll_used"] = session.DidsEnrollUsed

	actionRequired := gin.H{}
	if session.DidsEnrollURL != "" && !session.DidsEnrollUsed {
		actionRequired["dids_admin_enroll_url"] = session.DidsEnrollURL
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

	if session.Mode == model.ModeFullStackWithVtc {
		urls["vtc"] = "https://" + session.VtcFQDN()
		collected["vtc_did"] = session.VtcDid
		resp["vtc_install_used"] = session.VtcInstallUsed
		// Single-shot like the dids enroll URL — once acked, stop offering a
		// dead link; reissue-install mints a fresh pair.
		if session.VtcInstallURL != "" && !session.VtcInstallUsed {
			actionRequired["install_url"] = session.VtcInstallURL
			actionRequired["claim_code"] = session.VtcClaimCode
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

	if h.cf != nil {
		// cf_record_vtc is nil outside full_stack_with_vtc, so the empty-skip
		// covers plain full_stack rows.
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
		}

		h.k8s.DeleteAllComponentJobs(ctx, ns, session.ID)
		h.k8s.DeleteComponentResources(ctx, ns, k8s.FSVtaName(session.ID))
		h.k8s.DeleteComponentResources(ctx, ns, k8s.FSMediatorName(session.ID))
		h.k8s.DeleteComponentResources(ctx, ns, k8s.FSDidsName(session.ID))

		if session.Mode == model.ModeFullStackWithVtc {
			if h.orch != nil {
				h.orch.TeardownVtcVault(ctx, session.UserID, session.ID)
			}
			h.k8s.DeleteComponentResources(ctx, ns, k8s.FSVtcName(session.ID))
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
			if session.Mode == model.ModeFullStackWithVtc {
				source = "vtc"
			} else {
				source = "vta"
			}
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
	if !session.IsFullStackFamily() {
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
	if !session.IsFullStackFamily() {
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
