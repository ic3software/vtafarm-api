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
	Farm(context.Context) (farmComponentStatus, error)
	Platform(context.Context) (platformComponentStatus, error)
}

type farmComponentStatus struct {
	Database   string `json:"database"`
	Kubernetes string `json:"kubernetes"`
	Vault      string `json:"vault"`
}

func unknownFarmComponentStatus() farmComponentStatus {
	return farmComponentStatus{
		Database:   "unknown",
		Kubernetes: "unknown",
		Vault:      "unknown",
	}
}

func (s *farmComponentStatus) set(component, status string) {
	switch component {
	case "database":
		s.Database = status
	case "kubernetes":
		s.Kubernetes = status
	case "vault":
		s.Vault = status
	}
}

type platformComponentStatus struct {
	VTA      string `json:"vta"`
	Mediator string `json:"mediator"`
	DIDs     string `json:"dids"`
	VTC      string `json:"vtc"`
}

func unknownPlatformComponentStatus() platformComponentStatus {
	return platformComponentStatus{
		VTA:      "unknown",
		Mediator: "unknown",
		DIDs:     "unknown",
		VTC:      "unknown",
	}
}

func (s *platformComponentStatus) set(component, status string) {
	switch component {
	case "vta":
		s.VTA = status
	case "mediator":
		s.Mediator = status
	case "dids":
		s.DIDs = status
	case "vtc":
		s.VTC = status
	}
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
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()

	components, err := h.checks.Farm(ctx)
	h.respond(c, "infra", components, err)
}

func (h *StatusHandler) Platform(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()

	components, err := h.checks.Platform(ctx)
	h.respond(c, "platform", components, err)
}

func (h *StatusHandler) respond(c *gin.Context, component string, components any, err error) {
	status := "operational"
	code := http.StatusOK
	if err != nil {
		log.Printf("public status %s unavailable: %v", component, err)
		status, code = "unavailable", http.StatusServiceUnavailable
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(code, gin.H{
		"status":     status,
		"checked_at": h.now().UTC().Format(time.RFC3339),
		"components": components,
	})
}

type livePublicStatusChecks struct {
	db   *gorm.DB
	k8s  *k8s.Client
	cfg  StatusConfig
	http *http.Client
}

func (h *livePublicStatusChecks) Farm(ctx context.Context) (farmComponentStatus, error) {
	checks := []statusComponentCheck{
		{component: "database", check: func(componentCtx context.Context) error {
			if h.db == nil {
				return errors.New("database unavailable")
			}
			sqlDB, err := h.db.DB()
			if err != nil {
				return fmt.Errorf("database handle: %w", err)
			}
			if err := sqlDB.PingContext(componentCtx); err != nil {
				return fmt.Errorf("database ping: %w", err)
			}
			return nil
		}},
		{component: "kubernetes", check: func(componentCtx context.Context) error {
			if h.k8s == nil {
				return errors.New("kubernetes client unavailable")
			}
			if err := h.k8s.Ping(componentCtx); err != nil {
				return fmt.Errorf("kubernetes ping: %w", err)
			}
			return nil
		}},
		{component: "vault", check: func(componentCtx context.Context) error {
			if h.cfg.VaultAddr == "" {
				return errors.New("vault not configured")
			}
			status, err := vault.Health(componentCtx, h.cfg.VaultAddr, h.cfg.VaultSkipVerify)
			if err != nil {
				return fmt.Errorf("vault health: %w", err)
			}
			if status != "ok" {
				return fmt.Errorf("vault state: %s", status)
			}
			return nil
		}},
	}

	components := unknownFarmComponentStatus()
	results, err := runStatusChecks(ctx, checks)
	for component, status := range results {
		components.set(component, status)
	}
	return components, err
}

func (h *livePublicStatusChecks) Platform(ctx context.Context) (platformComponentStatus, error) {
	components := unknownPlatformComponentStatus()
	stack, err := h.platformStack(ctx)
	if err != nil {
		return components, err
	}

	targets := []statusProbeTarget{
		{component: "vta", target: strings.TrimRight(stack.PublicURL(), "/") + "/health"},
		{component: "mediator", target: "https://" + stack.MediatorFQDN() + "/mediator/v1/readyz"},
		{component: "dids", target: strings.TrimRight(stack.DidsURL(), "/") + "/health"},
		{component: "vtc", target: "https://" + stack.VtcFQDN() + "/health"},
	}

	validChecks := make([]statusComponentCheck, 0, len(targets))
	failures := make([]error, 0, len(targets))
	for _, probe := range targets {
		if err := validateStatusProbeURL(probe.target, h.cfg.ClusterDomain); err != nil {
			components.set(probe.component, "unavailable")
			failures = append(failures, fmt.Errorf("%s target: %w", probe.component, err))
			continue
		}
		target := probe.target
		validChecks = append(validChecks, statusComponentCheck{
			component: probe.component,
			check: func(componentCtx context.Context) error {
				return h.probe(componentCtx, target)
			},
		})
	}

	results, probeErr := runStatusChecks(ctx, validChecks)
	for component, status := range results {
		components.set(component, status)
	}
	if probeErr != nil {
		failures = append(failures, probeErr)
	}
	return components, errors.Join(failures...)
}

type statusProbeTarget struct {
	component string
	target    string
}

type statusComponentCheck struct {
	component string
	check     func(context.Context) error
}

type statusComponentResult struct {
	component string
	err       error
}

func runStatusChecks(ctx context.Context, checks []statusComponentCheck) (map[string]string, error) {
	results := make(chan statusComponentResult, len(checks))

	var group errgroup.Group
	for _, check := range checks {
		check := check
		group.Go(func() error {
			componentCtx, cancel := context.WithTimeout(ctx, statusProbeTimeout)
			defer cancel()
			results <- statusComponentResult{component: check.component, err: check.check(componentCtx)}
			return nil
		})
	}
	_ = group.Wait()
	close(results)

	statuses := make(map[string]string, len(checks))
	failures := make([]error, 0, len(checks))
	for result := range results {
		status := "operational"
		if result.err != nil {
			status = "unavailable"
			failures = append(failures, fmt.Errorf("%s: %w", result.component, result.err))
		}
		statuses[result.component] = status
	}
	return statuses, errors.Join(failures...)
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
