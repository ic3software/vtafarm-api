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

// This file holds the full_stack_with_vtc-specific handlers (design
// docs/full-stack-with-vtc-setup-design.md §13). The shared full_stack
// handlers (get/delete/logs, dids enroll-ack/reissue) live in
// setup_fullstack.go and branch on mode inline for the small VTC extensions.

// createFullStackWithVtc handles POST /api/v1/setup for
// mode=full_stack_with_vtc — createFullStack's shape with a fourth
// Cloudflare A-record (vtc) and the vtc_image/vtc_name inputs.
func (h *SetupHandler) createFullStackWithVtc(c *gin.Context, req createSetupRequest) {
	if h.k8s == nil || h.orch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s/orchestrator not configured"})
		return
	}

	if req.VtaName == "" {
		req.VtaName = "personal-vta"
	}
	if req.VtcName == "" {
		req.VtcName = "personal-vtc"
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
			"error": "full_stack_with_vtc requires mediator_image, dids_image and vtc_image (select from GET /setup/images?component=mediator|dids|vtc)",
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

	vtaSub, mediatorSub, didsSub, vtcSub := setup.FullStackWithVtcHosts(h.appEnv)
	vtaFQDN := vtaSub + "." + h.clusterDomain
	mediatorFQDN := mediatorSub + "." + h.clusterDomain
	didsFQDN := didsSub + "." + h.clusterDomain
	vtcFQDN := vtcSub + "." + h.clusterDomain

	// All four records are created up front (design §3) — the rendered
	// recipes embed the final HTTPS URLs. Roll back everything created so
	// far on the first failure.
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

	recordMediator, recordDids, recordVtc := records["mediator"], records["dids"], records["vtc"]
	session := model.SetupSession{
		UserID: userID,
		Mode:   model.ModeFullStackWithVtc,
		Status: "dns_provision",
		Domain: h.clusterDomain,
		// VTA reuses Subdomain/CFRecordID — same fields vta_only uses.
		Subdomain:         vtaSub,
		CFRecordID:        records["vta"],
		MediatorSubdomain: mediatorSub,
		DidsSubdomain:     didsSub,
		VtcSubdomain:      vtcSub,
		CFRecordMediator:  &recordMediator,
		CFRecordDids:      &recordDids,
		CFRecordVtc:       &recordVtc,
		VtaName:           req.VtaName,
		VtcName:           req.VtcName,
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
			"vtc":      "https://" + vtcFQDN,
		},
	})
}

// ReissueVtcInstall handles POST /api/v1/setup/:id/vtc/reissue-install
// (design §13, required — the setup-minted install token lives only 15
// minutes, so this is the expected way users actually claim the VTC admin).
// Remints a fresh install URL and claim code via `vtc admin invite`.
//
// Like ReissueDidsEnroll, the Job needs exclusive access to the component's
// local store, so the VTC Deployment is scaled to 0 first, restarted via
// defer so it comes back even if the Job fails.
func (h *SetupHandler) ReissueVtcInstall(c *gin.Context) {
	publicID := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("unique_id = ? AND user_id = ?", publicID, userID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if session.Mode != model.ModeFullStackWithVtc {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reissue-install is only available for full_stack_with_vtc sessions"})
		return
	}
	if session.VtcAdminDid == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "vtc not provisioned yet"})
		return
	}
	if h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s not configured"})
		return
	}

	ctx := c.Request.Context()
	ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
	vtcName := k8s.FSVtcName(session.ID)
	vtcSelector := fmt.Sprintf("app=fs-vtc,session-id=%d", session.ID)
	jobName := k8s.FSJobVtcInvite(session.ID)

	if err := h.k8s.ScaleComponentDeployment(ctx, ns, vtcName, 0); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to stop vtc: " + err.Error()})
		return
	}
	// Restart the VTC on the way out no matter how this handler returns — a
	// failed reissue shouldn't leave the service down.
	defer func() {
		restartCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := h.k8s.ScaleComponentDeployment(restartCtx, ns, vtcName, 1); err != nil {
			log.Printf("[setup] error: failed to restart vtc for session %d: %v", session.ID, err)
			return
		}
		if err := h.k8s.WaitForComponentDeploymentReady(restartCtx, ns, vtcName, 2*time.Minute); err != nil {
			log.Printf("[setup] warn: vtc not ready after reissue for session %d: %v", session.ID, err)
		}
	}()

	if err := h.k8s.WaitForComponentPodsGone(ctx, ns, vtcSelector, 2*time.Minute); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed waiting for vtc to stop: " + err.Error()})
		return
	}

	h.k8s.DeleteComponentJob(ctx, ns, jobName) // clear a previous (TTL'd) run if still present

	cmd := fmt.Sprintf("vtc admin invite --did %s", shellQuote(session.VtcAdminDid))
	if err := h.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          session.VtcImage,
		Command:        []string{"sh", "-c", cmd},
		WorkingDir:     "/work/vtc",
		ServiceAccount: k8s.VtaServiceAccount, // opens the VTC store, which reads its Vault key bundle
		PVCMounts:      []k8s.PVCMount{{Name: "vtc-data", ClaimName: k8s.FSVtcName(session.ID), MountPath: "/work/vtc"}},
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
	installURL, claimCode, err := setup.ParseVtcInviteOutput(logs)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "install URL / claim code not found in job output"})
		return
	}

	h.db.Model(&model.SetupSession{}).Where("id = ?", session.ID).Updates(map[string]any{
		"vtc_install_url":  installURL,
		"vtc_claim_code":   claimCode,
		"vtc_install_used": false,
	})
	c.JSON(http.StatusOK, gin.H{"install_url": installURL, "claim_code": claimCode})
}

// AckVtcInstall handles POST /api/v1/setup/:id/vtc/install-ack — the
// frontend calls this the instant the user opens the (one-shot) install URL.
// The VTC's own install-token state machine already refuses a second claim;
// this just lets the UI stop offering a dead link — reissue-install mints a
// fresh pair. Mirrors AckDidsEnroll.
func (h *SetupHandler) AckVtcInstall(c *gin.Context) {
	publicID := c.Param("id")
	userID := c.MustGet(middleware.ContextUserID).(uint)

	var session model.SetupSession
	if err := h.db.Where("unique_id = ? AND user_id = ?", publicID, userID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if session.Mode != model.ModeFullStackWithVtc {
		c.JSON(http.StatusBadRequest, gin.H{"error": "install-ack is only available for full_stack_with_vtc sessions"})
		return
	}
	if session.VtcInstallURL == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "no vtc install url has been issued yet"})
		return
	}

	if err := h.db.Model(&session).Update("vtc_install_used", true).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update session"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"vtc_install_used": true})
}
