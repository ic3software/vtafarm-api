package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	corev1 "k8s.io/api/core/v1"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
)

// Co-admins on the platform stack's VTA.
// Design: docs/platform-stack-admin-grant-design.md.
//
// The point of the feature is self-service: a second admin pastes the `did:key`
// their own `pnm setup` minted, instead of asking whoever holds the stack's
// credential to run `pnm acl create` for them.
//
// **Adding only.** Removing an admin is deliberately not here — an operator who
// needs to do it runs `pnm acl delete` against the live VTA, which costs no
// downtime and needs nothing from this side. What that buys is not just less
// code: it removes the need to work out *which* ACL entry belongs to whom, a
// question this side cannot answer well (see the note on rotation below).
//
// The VTA's admin ACL is a fjall store on a ReadWriteOnce PVC, and the running
// VTA holds a store-level lock on it, so writing it means stopping the VTA,
// running a Job against the volume, and starting it again (§3).
// `reissueDidsEnroll` does the same dance against the dids daemon and is the
// template this follows, down to the unconditional deferred restart.
//
// The owner-facing refresh route uses the same maintenance window to run
// `vta acl list` and replace the database snapshot shown in the portal.

// aclJobTimeouts. The window is dominated by waiting for the old pod's lock to
// be released and by the new pod's readiness probe, not by the CLI itself.
const (
	aclPodsGoneTimeout = 2 * time.Minute
	aclRestartTimeout  = 3 * time.Minute
	aclReadyTimeout    = 2 * time.Minute

	// aclJobStale bounds how long a `pending` row can block the next attempt.
	// Matches ComponentJobSpec's default ActiveDeadlineSeconds (600s) plus the
	// restart budget: past that, no Job of ours can still be running, so a row
	// still sitting at `pending` belongs to a request that died without
	// finishing — most likely a replica that was killed mid-window.
	aclJobStale = 15 * time.Minute
)

// aclJobLockSet serialises maintenance windows per session.
//
// Two concurrent grants would be genuinely destructive, not merely racy: both
// scale the VTA to 0, both then `DeleteComponentJob` and `CreateComponentJob`
// under the *same* name (there is one per session), so each can delete the
// other's running Job — and the first `defer` to fire scales the VTA back up
// while the other is still writing to the fjall store it holds a lock on.
//
// Different sessions have different Deployments, Jobs and PVCs, so they can be
// updated in parallel. Keeping only the set of held ids also avoids retaining a
// mutex forever for every session that has ever received a grant.
type aclJobLockSet struct {
	mu   sync.Mutex
	held map[uint]struct{}
}

func (l *aclJobLockSet) TryLock(sessionID uint) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.held[sessionID]; exists {
		return false
	}
	l.held[sessionID] = struct{}{}
	return true
}

func (l *aclJobLockSet) Unlock(sessionID uint) {
	l.mu.Lock()
	delete(l.held, sessionID)
	l.mu.Unlock()
}

var aclJobLocks = aclJobLockSet{held: make(map[uint]struct{})}

// errAclJobBusy is a sentinel so the handler answers 409 (come back in a
// minute) rather than 502 (something broke). Nothing is wrong when this fires.
var errAclJobBusy = errors.New(
	"another ACL operation is running for this VTA — that takes about a minute; try again after it finishes")

// platformSession loads the platform stack's session, writing the response and
// returning nil when there isn't one. Same two-step lookup GetPlatformStack
// does — the domain row outlives its session, so "no domain" and "domain but no
// session" are different states and both mean there is no stack to administer.
func (h *SetupHandler) platformSession(c *gin.Context) *model.SetupSession {
	domain, err := h.platformDomain()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read platform domain"})
		return nil
	}
	if domain == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no platform stack exists"})
		return nil
	}

	var session model.SetupSession
	err = h.db.Where("domain_id = ?", domain.ID).First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no platform stack exists"})
		return nil
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read platform session"})
		return nil
	}
	return &session
}

// ListPlatformStackAdmins — GET /api/v1/admin/platform-stack/admins.
//
// Serves stored state only: never stops the VTA, never blocks.
//
// This is a history of what was added from here, **not** the VTA's current
// admin list — and the two genuinely differ. A granted DID stops being the
// holder's DID on their first connect (PNM rotates and `POST /acl/swap` moves
// the entry), and admins added out of band never appear here at all. For who
// can act on the VTA right now, `pnm acl list`.
func (h *SetupHandler) ListPlatformStackAdmins(c *gin.Context) {
	session := h.platformSession(c)
	if session == nil {
		return
	}

	var grants []model.VtaAdminGrant
	if err := h.db.Where("session_id = ?", session.ID).
		Order("created_at DESC").Find(&grants).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read grants"})
		return
	}
	if grants == nil {
		grants = []model.VtaAdminGrant{}
	}

	c.JSON(http.StatusOK, gin.H{
		"id":     session.VtaName,
		"label":  session.VtaName,
		"grants": grants,
	})
}

// requireStackConfirm gates the one route that takes the VTA down, mirroring
// the guard on DELETE /admin/setup-sessions/:id: the caller must name the stack.
//
// Enforced here rather than in the UI. Adding an admin is both an irreversible
// privilege grant and a minute of downtime on the flagship stack — neither is
// something a stray click should be able to cause.
//
// Takes the already-bound value rather than reading the body itself, because
// gin's ShouldBindJSON consumes it: the grant route carries `did` and `label`
// alongside `confirm` and has to bind all three in one pass. A missing or
// malformed body leaves Confirm at "", which fails here exactly as a wrong
// value does — both mean "not confirmed".
func requireStackConfirm(c *gin.Context, session *model.SetupSession, confirm string) bool {
	if confirm != session.VtaName {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "this stops the platform stack's VTA for about a minute — " +
				`send {"confirm": "` + session.VtaName + `"} to proceed`,
		})
		return false
	}
	return true
}

// aclRestartError marks a failure of the deferred scale-back-up, so callers can
// tell "the grant failed" from "the grant may have worked but the VTA is still
// down" — §7.5. It has to reach either the platform operator or session owner.
type aclRestartError struct{ err error }

func (e *aclRestartError) Error() string { return e.err.Error() }
func (e *aclRestartError) Unwrap() error { return e.err }

// runVtaAclJob stops the VTA, runs `cmd` against its PVC, and starts it again.
//
// Returns the Job's logs, a non-nil restart error if bringing the VTA back up
// failed, and the operation error. The restart error is returned separately
// from the operation error on purpose: the two are independent, and an
// operation that succeeded while the restart failed is the case most in need of
// being reported accurately.
//
// The Job runs with Env: fsNoColorEnv() like every other full_stack Job — some
// of these binaries colorize output even when stdout is not a TTY, and the ANSI
// escapes corrupt captured values (§6).
func (h *SetupHandler) runVtaAclJob(
	ctx context.Context, session *model.SetupSession, cmd string,
) (logs string, restartErr error, err error) {
	if h.k8s == nil {
		return "", nil, fmt.Errorf("k8s not configured")
	}
	if session.VtaImage == "" {
		return "", nil, fmt.Errorf("stack has no VTA image recorded — it has not been provisioned")
	}

	// Held for the whole window, taken before anything is scaled. TryLock
	// rather than Lock: a caller who waits would sit through the other window
	// and then start their own, so the honest answer is to refuse now and let
	// them retry once — a queue of these is a queue of outages.
	if !aclJobLocks.TryLock(session.ID) {
		return "", nil, errAclJobBusy
	}
	defer aclJobLocks.Unlock(session.ID)

	// The in-process lock above protects callers handled by this replica. This
	// row closes the same race across replicas and doubles as snapshot metadata.
	lockStartedAt, ok, lockErr := h.acquireVtaAclMaintenance(session.ID)
	if lockErr != nil {
		return "", nil, fmt.Errorf("failed to lock ACL maintenance: %w", lockErr)
	}
	if !ok {
		return "", nil, errAclJobBusy
	}
	defer h.releaseVtaAclMaintenance(session.ID, lockStartedAt)

	ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
	target := vtaAclTargetFor(session)

	if scaleErr := h.k8s.ScaleComponentDeployment(ctx, ns, target.deployment, 0); scaleErr != nil {
		return "", nil, fmt.Errorf("failed to stop the VTA: %w", scaleErr)
	}

	// Bring the VTA back however this returns. Its own context, because the
	// request's may already be cancelled by the time we get here — a client
	// hanging up must not be what leaves the stack down.
	defer func() {
		restartCtx, cancel := context.WithTimeout(context.Background(), aclRestartTimeout)
		defer cancel()
		if e := h.k8s.ScaleComponentDeployment(restartCtx, ns, target.deployment, 1); e != nil {
			log.Printf("[vta-admins] error: failed to restart VTA for session %d: %v", session.ID, e)
			restartErr = &aclRestartError{e}
			return
		}
		if e := h.k8s.WaitForComponentDeploymentReady(restartCtx, ns, target.deployment, aclReadyTimeout); e != nil {
			log.Printf("[vta-admins] warn: VTA not ready after ACL job for session %d: %v", session.ID, e)
			restartErr = &aclRestartError{e}
		}
	}()

	// Scaling to 0 only stops scheduling — the outgoing pod keeps the fjall
	// lock until it actually terminates, so the Job cannot start before this.
	if waitErr := h.k8s.WaitForComponentPodsGone(ctx, ns, target.selector, aclPodsGoneTimeout); waitErr != nil {
		return "", nil, fmt.Errorf("failed waiting for the VTA to stop: %w", waitErr)
	}

	h.k8s.DeleteComponentJob(ctx, ns, target.job) // clear a previous (TTL'd) run

	if createErr := h.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           target.job,
		Image:          session.VtaImage,
		Command:        []string{"sh", "-c", cmd},
		WorkingDir:     "/work/vta",
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      []k8s.PVCMount{{Name: "vta-data", ClaimName: target.pvc, MountPath: "/work/vta"}},
		Env:            noColorEnv(),
	}); createErr != nil {
		return "", nil, fmt.Errorf("failed to create the ACL job: %w", createErr)
	}

	succeeded, failMsg, watchErr := h.k8s.WaitForJob(ctx, ns, target.job)
	if watchErr != nil {
		return "", nil, fmt.Errorf("job watch error: %w", watchErr)
	}
	if !succeeded {
		return "", nil, fmt.Errorf("ACL job failed: %s", failMsg)
	}

	out, logErr := h.k8s.JobLogs(ctx, ns, target.job)
	if logErr != nil {
		return "", nil, fmt.Errorf("failed to read job logs: %w", logErr)
	}
	return out, nil, nil
}

func (h *SetupHandler) acquireVtaAclMaintenance(sessionID uint) (time.Time, bool, error) {
	now := time.Now()
	result := h.db.Exec(`
		INSERT INTO vta_acl_snapshots (session_id, synced_at, entry_count, maintenance_started_at)
		VALUES (?, NULL, 0, ?)
		ON CONFLICT (session_id) DO UPDATE
		SET maintenance_started_at = EXCLUDED.maintenance_started_at
		WHERE vta_acl_snapshots.maintenance_started_at IS NULL
		   OR vta_acl_snapshots.maintenance_started_at <= ?`,
		sessionID, now, now.Add(-aclJobStale))
	return now, result.RowsAffected == 1, result.Error
}

func (h *SetupHandler) releaseVtaAclMaintenance(sessionID uint, startedAt time.Time) {
	if err := h.db.Model(&model.VtaAclSnapshot{}).
		Where("session_id = ? AND maintenance_started_at = ?", sessionID, startedAt).
		Update("maintenance_started_at", nil).Error; err != nil {
		log.Printf("[vta-admins] error: failed to release ACL maintenance lock for session %d: %v", sessionID, err)
	}
}

type vtaAclTarget struct {
	deployment string
	selector   string
	job        string
	pvc        string
}

func vtaAclTargetFor(session *model.SetupSession) vtaAclTarget {
	if session.IsFullStack() {
		return vtaAclTarget{
			deployment: k8s.FSVtaName(session.ID),
			selector:   fmt.Sprintf("app=fs-vta,session-id=%d", session.ID),
			job:        k8s.FSJobVtaACL(session.ID),
			pvc:        k8s.FSVtaName(session.ID),
		}
	}
	return vtaAclTarget{
		deployment: k8s.VtaDeploymentName(session.ID),
		selector:   fmt.Sprintf("app=vta,session-id=%d", session.ID),
		job:        k8s.VtaACLJobName(session.ID),
		pvc:        k8s.VtaPVCName(session.ID),
	}
}

// respondAclJobError writes the failure, folding in a failed restart when there
// was one. Both are reported: a caller told only that the grant failed would
// have no reason to check whether the VTA came back.
func respondAclJobError(c *gin.Context, session *model.SetupSession, err, restartErr error) {
	body := gin.H{"error": err.Error()}
	if restartErr != nil {
		body["error"] = err.Error() + " — " + restartWarning(session, restartErr)
	}
	// Busy is not a failure: nothing broke, nothing was scaled, and the caller
	// should simply come back. 502 would send an operator looking for damage.
	if errors.Is(err, errAclJobBusy) {
		c.JSON(http.StatusConflict, body)
		return
	}
	c.JSON(http.StatusBadGateway, body)
}

func restartWarning(session *model.SetupSession, restartErr error) string {
	return "WARNING: the VTA did not come back up (" + restartErr.Error() +
		"). Agent " + session.VtaName + " is down — check the deployment before retrying."
}

// noColorEnv mirrors internal/setup's fsNoColorEnv, which is unexported for the
// same reason shellQuote is duplicated here — internal/setup and
// internal/handler stay decoupled.
//
// Matters because the Job's output is read back: `already_present` is decided by
// matching a marker in it, and some of these binaries colorize even when stdout
// is a Job log rather than a TTY.
func noColorEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "NO_COLOR", Value: "1"},
		{Name: "CLICOLOR", Value: "0"},
	}
}
