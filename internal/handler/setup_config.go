package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sync/errgroup"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
)

const (
	maxConfigBytes          = 192 << 10
	configOperationLimit    = 12 * time.Minute
	configCrashRestartLimit = 3
)

type stackConfigTarget struct {
	name       string
	deployment string
	selector   string
	pvc        string
	mountPath  string
	configPath string
	inputKey   string
}

type stackConfigRequest struct {
	Component string `json:"component"`
	Content   string `json:"content"`
}

func stackConfigTargets(session *model.SetupSession) []stackConfigTarget {
	sessionID := session.ID
	if !session.IsFullStack() {
		return []stackConfigTarget{{"vta", k8s.VtaDeploymentName(sessionID), fmt.Sprintf("app=vta,session-id=%d", sessionID), k8s.VtaPVCName(sessionID), "/work/vta", "/work/vta/config.toml", "vta.toml"}}
	}
	return []stackConfigTarget{
		{"vta", k8s.FSVtaName(sessionID), fmt.Sprintf("app=fs-vta,session-id=%d", sessionID), k8s.FSVtaName(sessionID), "/work/vta", "/work/vta/config.toml", "vta.toml"},
		{"mediator", k8s.FSMediatorName(sessionID), fmt.Sprintf("app=fs-mediator,session-id=%d", sessionID), k8s.FSMediatorName(sessionID), "/work/mediator", "/work/mediator/conf/mediator.toml", "mediator.toml"},
		{"dids", k8s.FSDidsName(sessionID), fmt.Sprintf("app=fs-dids,session-id=%d", sessionID), k8s.FSDidsName(sessionID), "/work/dids", "/work/dids/config.toml", "dids.toml"},
		{"vtc", k8s.FSVtcName(sessionID), fmt.Sprintf("app=fs-vtc,session-id=%d", sessionID), k8s.FSVtcName(sessionID), "/work/vtc", "/work/vtc/config.toml", "vtc.toml"},
	}
}

// Owner-facing routes use the session's own components. A vta_only agent does
// not own the platform components it uses.
func (h *SetupHandler) GetStackConfigs(c *gin.Context) {
	if session := h.userSession(c); session != nil {
		h.getStackConfigs(c, session)
	}
}

func (h *SetupHandler) ValidateStackConfigs(c *gin.Context) {
	if session := h.userSession(c); session != nil {
		h.validateStackConfigs(c, session)
	}
}

func (h *SetupHandler) ApplyStackConfigs(c *gin.Context) {
	if session := h.userSession(c); session != nil {
		h.applyStackConfigs(c, session)
	}
}

// Admin variants deliberately resolve only the platform stack. General admins
// do not gain a TOML editor for customer stacks through this feature.
func (h *SetupHandler) AdminGetPlatformStackConfigs(c *gin.Context) {
	if session := h.platformSession(c); session != nil {
		h.getStackConfigs(c, session)
	}
}

func (h *SetupHandler) AdminValidatePlatformStackConfigs(c *gin.Context) {
	if session := h.platformSession(c); session != nil {
		h.validateStackConfigs(c, session)
	}
}

func (h *SetupHandler) AdminApplyPlatformStackConfigs(c *gin.Context) {
	if session := h.platformSession(c); session != nil {
		h.applyStackConfigs(c, session)
	}
}

func configSessionReady(c *gin.Context, session *model.SetupSession) bool {
	if session.Status != "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "the session must be running before its configuration can be changed"})
		return false
	}
	return true
}

func (h *SetupHandler) getStackConfigs(c *gin.Context, session *model.SetupSession) {
	if !configSessionReady(c, session) {
		return
	}
	target, ok := stackConfigTargetFor(session, c.Query("component"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown configuration component"})
		return
	}
	content, err := h.readStackConfig(c.Request.Context(), session, target)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read configuration: " + err.Error()})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"content": content})
}

func (h *SetupHandler) validateStackConfigs(c *gin.Context, session *model.SetupSession) {
	if !configSessionReady(c, session) {
		return
	}
	req, ok := bindStackConfig(c)
	if !ok {
		return
	}
	if validationErrors := validateTOMLConfig(req, stackConfigTargets(session)); len(validationErrors) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "TOML validation failed", "validation_errors": validationErrors})
		return
	}
	c.JSON(http.StatusOK, gin.H{"valid": true})
}

func bindStackConfig(c *gin.Context) (stackConfigRequest, bool) {
	requestLimit := maxConfigBytes + (64 << 10)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, int64(requestLimit))
	var req stackConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return req, false
	}
	return req, true
}

func stackConfigTargetFor(session *model.SetupSession, name string) (stackConfigTarget, bool) {
	for _, target := range stackConfigTargets(session) {
		if target.name == name {
			return target, true
		}
	}
	return stackConfigTarget{}, false
}

func validateTOMLConfig(req stackConfigRequest, targets []stackConfigTarget) map[string]string {
	validationErrors := make(map[string]string)
	allowed := false
	for _, target := range targets {
		if target.name == req.Component {
			allowed = true
			break
		}
	}
	if !allowed {
		validationErrors["component"] = "unknown component for this session"
		return validationErrors
	}
	if strings.TrimSpace(req.Content) == "" {
		validationErrors[req.Component] = "configuration cannot be empty"
	} else if len(req.Content) > maxConfigBytes {
		validationErrors[req.Component] = fmt.Sprintf("configuration exceeds %d KiB", maxConfigBytes>>10)
	} else {
		var parsed map[string]any
		if err := toml.Unmarshal([]byte(req.Content), &parsed); err != nil {
			validationErrors[req.Component] = err.Error()
		}
	}
	return validationErrors
}

func (h *SetupHandler) applyStackConfigs(c *gin.Context, session *model.SetupSession) {
	if !configSessionReady(c, session) {
		return
	}
	req, ok := bindStackConfig(c)
	if !ok {
		return
	}
	if validationErrors := validateTOMLConfig(req, stackConfigTargets(session)); len(validationErrors) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "TOML validation failed", "validation_errors": validationErrors})
		return
	}
	target, _ := stackConfigTargetFor(session, req.Component)
	targets := []stackConfigTarget{target}
	if h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s not configured"})
		return
	}

	release, err := h.acquireSessionMaintenance(session.ID)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errAclJobBusy) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	defer release()
	var upgradesInFlight int64
	if err := h.db.Model(&model.UpgradeTask{}).
		Where("session_id = ? AND status IN ?", session.ID,
			[]string{model.UpgradeTaskPending, model.UpgradeTaskRunning}).
		Count(&upgradesInFlight).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check for an in-progress upgrade"})
		return
	}
	if upgradesInFlight > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "an upgrade for this session is already in progress"})
		return
	}

	currentContent, err := h.readStackConfig(c.Request.Context(), session, target)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read current component configuration: " + err.Error()})
		return
	}
	if currentContent == req.Content {
		c.JSON(http.StatusOK, gin.H{"status": "unchanged"})
		return
	}
	current := map[string]string{target.name: currentContent}

	ctx, cancel := context.WithTimeout(context.Background(), configOperationLimit)
	defer cancel()
	ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
	if err := h.stopStack(ctx, ns, targets); err != nil {
		recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), configOperationLimit)
		defer recoveryCancel()
		restartErr := h.startStack(recoveryCtx, ns, targets)
		c.JSON(http.StatusBadGateway, gin.H{"error": joinOperationErrors("failed to stop the component", err, restartErr)})
		return
	}

	if err := h.runConfigWriteJob(ctx, ns, session, targets, map[string]string{target.name: req.Content}); err != nil {
		rollbackErr := h.recoverStackConfigs(ns, session, targets, current)
		body := gin.H{
			"error":       "failed to write configuration; the previous configuration was restored",
			"write_error": err.Error(),
			"rolled_back": rollbackErr == nil,
		}
		if rollbackErr != nil {
			body["error"] = "failed to write configuration and automatic rollback was incomplete"
			body["rollback_error"] = rollbackErr.Error()
		}
		c.JSON(http.StatusBadGateway, body)
		return
	}

	if err := h.startStack(ctx, ns, targets); err == nil {
		h.cleanupConfigBackups(ctx, ns, targets)
		c.JSON(http.StatusOK, gin.H{"status": "applied", "validated": true})
		return
	} else {
		startupErr := err
		rollbackErr := h.recoverStackConfigs(ns, session, targets, current)
		body := gin.H{
			"error":         "the new configuration did not pass startup validation; the previous configuration was restored",
			"startup_error": startupErr.Error(),
			"rolled_back":   rollbackErr == nil,
		}
		if rollbackErr != nil {
			body["error"] = "the new configuration did not pass startup validation and automatic rollback was incomplete"
			body["rollback_error"] = rollbackErr.Error()
		}
		c.JSON(http.StatusUnprocessableEntity, body)
	}
}

func (h *SetupHandler) recoverStackConfigs(
	ns string,
	session *model.SetupSession,
	targets []stackConfigTarget,
	previous map[string]string,
) error {
	// The apply deadline may already have expired. Recovery needs its own window.
	ctx, cancel := context.WithTimeout(context.Background(), configOperationLimit)
	defer cancel()
	return h.rollbackStackConfigs(ctx, ns, session, targets, previous)
}

func (h *SetupHandler) readStackConfig(ctx context.Context, session *model.SetupSession, target stackConfigTarget) (string, error) {
	if h.k8s == nil {
		return "", fmt.Errorf("k8s not configured")
	}
	ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
	pod, container, err := h.k8s.RunningPod(ctx, ns, target.selector)
	if err != nil {
		return "", fmt.Errorf("%s: %w", target.name, err)
	}
	content, err := h.k8s.ExecCapture(ctx, ns, pod, container,
		[]string{"sh", "-c", "cat " + shellQuote(target.configPath)})
	if err != nil {
		return "", fmt.Errorf("%s: %w", target.name, err)
	}
	return content, nil
}

func (h *SetupHandler) stopStack(ctx context.Context, ns string, targets []stackConfigTarget) error {
	parentCtx := ctx
	var errs []error
	var mu sync.Mutex
	g, ctx := errgroup.WithContext(parentCtx)
	for _, target := range targets {
		target := target
		g.Go(func() error {
			if err := h.k8s.ScaleComponentDeployment(ctx, ns, target.deployment, 0); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", target.name, err))
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	g, ctx = errgroup.WithContext(parentCtx)
	for _, target := range targets {
		target := target
		g.Go(func() error {
			if err := h.k8s.WaitForComponentPodsGone(ctx, ns, target.selector, 2*time.Minute); err != nil {
				return fmt.Errorf("%s: %w", target.name, err)
			}
			return nil
		})
	}
	return g.Wait()
}

func (h *SetupHandler) startStack(ctx context.Context, ns string, targets []stackConfigTarget) error {
	parentCtx := ctx
	var errs []error
	var mu sync.Mutex
	g, ctx := errgroup.WithContext(parentCtx)
	for _, target := range targets {
		target := target
		g.Go(func() error {
			if err := h.k8s.ScaleComponentDeployment(ctx, ns, target.deployment, 1); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s scale: %w", target.name, err))
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()

	g, ctx = errgroup.WithContext(parentCtx)
	for _, target := range targets {
		target := target
		g.Go(func() error {
			if err := h.k8s.WaitForComponentDeploymentReadyOrRestarts(ctx, ns, target.deployment, target.selector, 2*time.Minute, configCrashRestartLimit); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s readiness: %w", target.name, err))
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	return errors.Join(errs...)
}

func (h *SetupHandler) runConfigWriteJob(
	ctx context.Context,
	ns string,
	session *model.SetupSession,
	targets []stackConfigTarget,
	configs map[string]string,
) error {
	jobName := k8s.FSJobConfigUpdate(session.ID)
	h.k8s.DeleteComponentJob(ctx, ns, jobName)
	secretData := make(map[string][]byte, len(targets))
	var commands []string
	for _, target := range targets {
		secretData[target.inputKey] = []byte(configs[target.name])
		path := shellQuote(target.configPath)
		next := shellQuote(target.configPath + ".vtafarm-next")
		backup := shellQuote(target.configPath + ".vtafarm-backup")
		input := shellQuote("/config/" + target.inputKey)
		commands = append(commands,
			fmt.Sprintf("cp -p %s %s", path, backup),
			fmt.Sprintf("cp -p %s %s", path, next),
			fmt.Sprintf("cat %s > %s", input, next),
			fmt.Sprintf("mv %s %s", next, path),
		)
	}
	if err := h.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          session.VtaImage,
		Command:        []string{"sh", "-c", "set -eu; umask 077; " + strings.Join(commands, "; ")},
		WorkingDir:     targets[0].mountPath,
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      configPVCMounts(targets),
		SecretData:     secretData,
		Env:            noColorEnv(),
	}); err != nil {
		h.k8s.DeleteComponentJob(context.Background(), ns, jobName)
		return err
	}
	defer h.k8s.DeleteComponentJob(context.Background(), ns, jobName)
	return h.waitConfigJob(ctx, ns, jobName)
}

func (h *SetupHandler) rollbackStackConfigs(
	ctx context.Context,
	ns string,
	session *model.SetupSession,
	targets []stackConfigTarget,
	previous map[string]string,
) error {
	var errs []error
	if err := h.stopStack(ctx, ns, targets); err != nil {
		errs = append(errs, fmt.Errorf("stop before rollback: %w", err))
		if restartErr := h.startStack(ctx, ns, targets); restartErr != nil {
			errs = append(errs, fmt.Errorf("restart after failed rollback stop: %w", restartErr))
		}
		return errors.Join(errs...)
	}
	jobName := k8s.FSJobConfigRollback(session.ID)
	h.k8s.DeleteComponentJob(ctx, ns, jobName)
	secretData := make(map[string][]byte, len(targets))
	var commands []string
	for _, target := range targets {
		secretData[target.inputKey] = []byte(previous[target.name])
		path := shellQuote(target.configPath)
		next := shellQuote(target.configPath + ".vtafarm-next")
		backup := shellQuote(target.configPath + ".vtafarm-backup")
		input := shellQuote("/config/" + target.inputKey)
		commands = append(commands,
			fmt.Sprintf("rm -f %s", next),
			fmt.Sprintf("cp -p %s %s", path, next),
			fmt.Sprintf("cat %s > %s", input, next),
			fmt.Sprintf("mv %s %s", next, path),
			fmt.Sprintf("rm -f %s", backup),
		)
	}
	if err := h.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{
		Name:           jobName,
		Image:          session.VtaImage,
		Command:        []string{"sh", "-c", "set -eu; " + strings.Join(commands, "; ")},
		WorkingDir:     targets[0].mountPath,
		ServiceAccount: k8s.VtaServiceAccount,
		PVCMounts:      configPVCMounts(targets),
		SecretData:     secretData,
		Env:            noColorEnv(),
	}); err != nil {
		h.k8s.DeleteComponentJob(context.Background(), ns, jobName)
		errs = append(errs, fmt.Errorf("create rollback job: %w", err))
	} else {
		if err := h.waitConfigJob(ctx, ns, jobName); err != nil {
			errs = append(errs, fmt.Errorf("rollback job: %w", err))
		}
		h.k8s.DeleteComponentJob(context.Background(), ns, jobName)
	}
	if err := h.startStack(ctx, ns, targets); err != nil {
		errs = append(errs, fmt.Errorf("restart after rollback: %w", err))
	}
	return errors.Join(errs...)
}

func configPVCMounts(targets []stackConfigTarget) []k8s.PVCMount {
	mounts := make([]k8s.PVCMount, 0, len(targets))
	for _, target := range targets {
		mounts = append(mounts, k8s.PVCMount{
			Name: target.name + "-data", ClaimName: target.pvc, MountPath: target.mountPath,
		})
	}
	return mounts
}

func (h *SetupHandler) waitConfigJob(ctx context.Context, ns, jobName string) error {
	succeeded, failMsg, err := h.k8s.WaitForJob(ctx, ns, jobName)
	if err != nil {
		return err
	}
	if !succeeded {
		if logs, logErr := h.k8s.JobLogs(ctx, ns, jobName); logErr == nil && strings.TrimSpace(logs) != "" {
			failMsg = strings.TrimSpace(logs)
		}
		return fmt.Errorf("job failed: %s", failMsg)
	}
	return nil
}

func (h *SetupHandler) cleanupConfigBackups(ctx context.Context, ns string, targets []stackConfigTarget) {
	for _, target := range targets {
		pod, container, err := h.k8s.RunningPod(ctx, ns, target.selector)
		if err != nil {
			log.Printf("[config] warn: find %s pod for backup cleanup: %v", target.name, err)
			continue
		}
		_, err = h.k8s.ExecCapture(ctx, ns, pod, container,
			[]string{"sh", "-c", "rm -f " + shellQuote(target.configPath+".vtafarm-backup")})
		if err != nil {
			log.Printf("[config] warn: clean %s config backup: %v", target.name, err)
		}
	}
}

func joinOperationErrors(prefix string, operationErr, restartErr error) string {
	message := prefix + ": " + operationErr.Error()
	if restartErr != nil {
		message += "; WARNING: one or more components did not restart: " + restartErr.Error()
	}
	return message
}
