package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/setup"
)

// This file holds full_stack's VTC-specific handlers (design
// docs/full-stack-setup-design.md §12). The rest of the full_stack handlers
// (create, get/delete/logs, dids enroll-ack/reissue) live in
// setup_fullstack.go.

// ReissueVtcInstall handles POST /api/v1/setup/:id/vtc/reissue-install
// (design §13, required — the setup-minted install token lives only 15
// minutes, so this is the expected way users actually claim the VTC admin).
// Remints a fresh install URL and claim code via `vtc admin invite`.
//
// Like ReissueDidsEnroll, the Job needs exclusive access to the component's
// local store, so the VTC Deployment is scaled to 0 first, restarted via
// defer so it comes back even if the Job fails.
func (h *SetupHandler) ReissueVtcInstall(c *gin.Context) {
	if s := h.userSession(c); s != nil {
		h.reissueVtcInstall(c, s)
	}
}

// AdminReissueVtcInstall — the admin-cookie twin, reaching any user's session.
func (h *SetupHandler) AdminReissueVtcInstall(c *gin.Context) {
	if s := h.adminSession(c); s != nil {
		h.reissueVtcInstall(c, s)
	}
}

func (h *SetupHandler) reissueVtcInstall(c *gin.Context, session *model.SetupSession) {
	if !session.IsFullStack() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reissue-install is only available for full_stack sessions"})
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
	if s := h.userSession(c); s != nil {
		h.ackVtcInstall(c, s)
	}
}

// AdminAckVtcInstall — the admin-cookie twin, reaching any user's session.
func (h *SetupHandler) AdminAckVtcInstall(c *gin.Context) {
	if s := h.adminSession(c); s != nil {
		h.ackVtcInstall(c, s)
	}
}

func (h *SetupHandler) ackVtcInstall(c *gin.Context, session *model.SetupSession) {
	if !session.IsFullStack() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "install-ack is only available for full_stack sessions"})
		return
	}
	if session.VtcInstallURL == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "no vtc install url has been issued yet"})
		return
	}

	if err := h.db.Model(session).Update("vtc_install_used", true).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update session"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"vtc_install_used": true})
}
