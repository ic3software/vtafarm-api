package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/vault"
)

const (
	statusProbeTimeout   = 3 * time.Second
	statusOverallTimeout = 8 * time.Second
)

type StatusConfig struct {
	ClusterDomain   string
	VaultAddr       string
	VaultSkipVerify bool
}

type publicStatusChecks interface {
	Farm(context.Context) error
	VTA(context.Context) error
	Platform(context.Context) error
}

// StatusHandler exposes customer-safe aggregate health. Detailed failures are
// logged for operators and never returned to the caller.
type StatusHandler struct {
	checks  publicStatusChecks
	timeout time.Duration
	now     func() time.Time
}

func NewStatusHandler(db *gorm.DB, k8sClient *k8s.Client, cfg StatusConfig) *StatusHandler {
	client := &http.Client{
		Timeout: statusProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &StatusHandler{
		checks: &livePublicStatusChecks{
			db: db, k8s: k8sClient, cfg: cfg, http: client,
		},
		timeout: statusOverallTimeout,
		now:     time.Now,
	}
}

func (h *StatusHandler) Farm(c *gin.Context) {
	h.respond(c, "farm", h.checks.Farm)
}

func (h *StatusHandler) VTA(c *gin.Context) {
	h.respond(c, "vta", h.checks.VTA)
}

func (h *StatusHandler) Platform(c *gin.Context) {
	h.respond(c, "platform", h.checks.Platform)
}

func (h *StatusHandler) respond(c *gin.Context, component string, check func(context.Context) error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()

	status := "operational"
	code := http.StatusOK
	if err := check(ctx); err != nil {
		log.Printf("public status %s unavailable: %v", component, err)
		status, code = "unavailable", http.StatusServiceUnavailable
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(code, gin.H{
		"status":     status,
		"checked_at": h.now().UTC().Format(time.RFC3339),
	})
}

type livePublicStatusChecks struct {
	db   *gorm.DB
	k8s  *k8s.Client
	cfg  StatusConfig
	http *http.Client
}

func (h *livePublicStatusChecks) Farm(ctx context.Context) error {
	if h.db == nil {
		return errors.New("database unavailable")
	}
	if h.k8s == nil {
		return errors.New("kubernetes client unavailable")
	}
	if h.cfg.VaultAddr == "" {
		return errors.New("vault not configured")
	}

	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		componentCtx, cancel := context.WithTimeout(groupCtx, statusProbeTimeout)
		defer cancel()
		sqlDB, err := h.db.DB()
		if err != nil {
			return fmt.Errorf("database handle: %w", err)
		}
		if err := sqlDB.PingContext(componentCtx); err != nil {
			return fmt.Errorf("database ping: %w", err)
		}
		return nil
	})
	group.Go(func() error {
		componentCtx, cancel := context.WithTimeout(groupCtx, statusProbeTimeout)
		defer cancel()
		if err := h.k8s.Ping(componentCtx); err != nil {
			return fmt.Errorf("kubernetes ping: %w", err)
		}
		return nil
	})
	group.Go(func() error {
		componentCtx, cancel := context.WithTimeout(groupCtx, statusProbeTimeout)
		defer cancel()
		status, err := vault.Health(componentCtx, h.cfg.VaultAddr, h.cfg.VaultSkipVerify)
		if err != nil {
			return fmt.Errorf("vault health: %w", err)
		}
		if status != "ok" {
			return fmt.Errorf("vault state: %s", status)
		}
		return nil
	})
	return group.Wait()
}

func (h *livePublicStatusChecks) VTA(ctx context.Context) error {
	stack, err := h.platformStack(ctx)
	if err != nil {
		return err
	}

	target := strings.TrimRight(stack.PublicURL(), "/") + "/health"
	if err := validateStatusProbeURL(target, h.cfg.ClusterDomain); err != nil {
		return fmt.Errorf("platform VTA target: %w", err)
	}
	return h.probe(ctx, target)
}

func (h *livePublicStatusChecks) Platform(ctx context.Context) error {
	stack, err := h.platformStack(ctx)
	if err != nil {
		return err
	}

	targets := map[string]string{
		"mediator": "https://" + stack.MediatorFQDN() + "/mediator/v1/readyz",
		"dids":     strings.TrimRight(stack.DidsURL(), "/") + "/health",
		"vtc":      "https://" + stack.VtcFQDN() + "/health",
	}
	for component, target := range targets {
		if err := validateStatusProbeURL(target, h.cfg.ClusterDomain); err != nil {
			return fmt.Errorf("%s target: %w", component, err)
		}
	}

	group, groupCtx := errgroup.WithContext(ctx)
	for component, target := range targets {
		component, target := component, target
		group.Go(func() error {
			if err := h.probe(groupCtx, target); err != nil {
				return fmt.Errorf("%s: %w", component, err)
			}
			return nil
		})
	}
	return group.Wait()
}

func (h *livePublicStatusChecks) platformStack(ctx context.Context) (*model.SetupSession, error) {
	if h.db == nil {
		return nil, errors.New("database unavailable")
	}

	var sessions []model.SetupSession
	if err := h.db.WithContext(ctx).
		Where("domain_type = ? AND mode = ?", model.DomainPlatform, model.ModeFullStack).
		Limit(2).Find(&sessions).Error; err != nil {
		return nil, fmt.Errorf("load platform stack: %w", err)
	}
	if len(sessions) != 1 {
		return nil, fmt.Errorf("platform stack count: %d", len(sessions))
	}
	stack := &sessions[0]
	if stack.Status != "running" {
		return nil, fmt.Errorf("platform stack state: %s", stack.Status)
	}
	return stack, nil
}

func (h *livePublicStatusChecks) probe(ctx context.Context, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vtafarm-api-status/1")
	resp, err := h.http.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func validateStatusProbeURL(rawURL, clusterDomain string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	domain := strings.ToLower(strings.TrimSuffix(clusterDomain, "."))
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if parsed.Scheme != "https" || parsed.User != nil || host == "" {
		return errors.New("target must be an HTTPS URL without credentials")
	}
	if domain == "" || (host != domain && !strings.HasSuffix(host, "."+domain)) {
		return errors.New("target is outside the configured cluster domain")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return errors.New("target uses a non-HTTPS port")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("target must not contain a query or fragment")
	}
	return nil
}
