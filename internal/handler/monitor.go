package handler

import (
	"context"
	"crypto/subtle"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/vault"
)

// MonitorConfig mirrors config.MonitorConfig plus the Vault coordinates the
// health check needs (its own type so this package doesn't depend on
// internal/config, matching the vault package's convention).
type MonitorConfig struct {
	Token           string
	CPUPercent      int
	MemPercent      int
	StoragePercent  int
	RestartWindow   time.Duration
	PendingGrace    time.Duration
	ExtraNamespaces []string
	VaultAddr       string
	VaultSkipVerify bool
}

// MonitorHandler serves the token-gated /api/v1/monitor/* endpoints polled by
// an external uptime service (UptimeRobot). Contract: healthy → 200, anything
// wrong → 503, so a plain HTTP monitor on the URL is the whole alerting setup.
type MonitorHandler struct {
	db  *gorm.DB
	k8s *k8s.Client
	cfg MonitorConfig
}

func NewMonitorHandler(db *gorm.DB, k8sClient *k8s.Client, cfg MonitorConfig) *MonitorHandler {
	return &MonitorHandler{db: db, k8s: k8sClient, cfg: cfg}
}

// TokenRequired gates the monitor group. These endpoints expose internal
// cluster state yet must be reachable without a login (the poller can't do
// passkeys), so a shared secret in the URL is the gate. No MONITOR_TOKEN
// configured → 404, indistinguishable from the routes not existing.
func (h *MonitorHandler) TokenRequired(c *gin.Context) {
	if h.cfg.Token == "" {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(c.Query("token")), []byte(h.cfg.Token)) != 1 {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	c.Next()
}

// Health — GET /api/v1/monitor/health. Dependency checks: DB, K8s API, Vault
// (including sealed state). Vault reports "disabled" (not an alarm) when
// VAULT_ADDR is unset so local dev without Vault doesn't alarm forever.
func (h *MonitorHandler) Health(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	checks := gin.H{"db": "ok", "k8s": "ok", "vault": "ok"}
	alarm := false

	if sqlDB, err := h.db.DB(); err != nil {
		checks["db"], alarm = "unreachable", true
		log.Printf("monitor: db handle: %v", err)
	} else if err := sqlDB.PingContext(ctx); err != nil {
		checks["db"], alarm = "unreachable", true
		log.Printf("monitor: db ping: %v", err)
	}

	if h.k8s == nil {
		checks["k8s"], alarm = "unavailable", true
	} else if err := h.k8s.Ping(ctx); err != nil {
		checks["k8s"], alarm = "unreachable", true
		log.Printf("monitor: k8s ping: %v", err)
	}

	if h.cfg.VaultAddr == "" {
		checks["vault"] = "disabled"
	} else if status, err := vault.Health(ctx, h.cfg.VaultAddr, h.cfg.VaultSkipVerify); err != nil {
		checks["vault"], alarm = "unreachable", true
		log.Printf("monitor: vault health: %v", err)
	} else {
		checks["vault"] = status
		if status != "ok" {
			alarm = true
		}
	}

	respondMonitor(c, alarm, gin.H{"checks": checks})
}

// Workloads — GET /api/v1/monitor/workloads. Unhealthy pods in the user and
// infra namespaces; the pod *status* signals (crash loops, restarts, stuck
// states) that actually mean "something is broken".
func (h *MonitorHandler) Workloads(c *gin.Context) {
	if h.k8s == nil {
		respondMonitor(c, true, gin.H{"error": "kubernetes client unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	alerts, err := h.k8s.WorkloadAlerts(ctx, k8s.WorkloadAlertConfig{
		RestartWindow:   h.cfg.RestartWindow,
		PendingGrace:    h.cfg.PendingGrace,
		ExtraNamespaces: h.cfg.ExtraNamespaces,
	})
	if err != nil {
		log.Printf("monitor: workload alerts: %v", err)
		respondMonitor(c, true, gin.H{"error": "scan pods: " + err.Error()})
		return
	}
	if alerts == nil {
		alerts = []k8s.PodAlert{}
	}
	respondMonitor(c, len(alerts) > 0, gin.H{"pods": alerts})
}

// CapacityIssue is one threshold breach reported by GET /api/v1/monitor/capacity.
type CapacityIssue struct {
	Resource  string `json:"resource"` // cpu | memory | storage | metrics | storage_stats
	Node      string `json:"node,omitempty"`
	Percent   int    `json:"percent,omitempty"`
	Threshold int    `json:"threshold,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Capacity — GET /api/v1/monitor/capacity. Per-node CPU/memory/disk usage
// against the alarm thresholds. Defaults come from MONITOR_*_PCT and can be
// overridden per-poll via ?cpu= / ?mem= / ?storage= — tune sensitivity by
// editing the UptimeRobot URL, no redeploy.
func (h *MonitorHandler) Capacity(c *gin.Context) {
	if h.k8s == nil {
		respondMonitor(c, true, gin.H{"error": "kubernetes client unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	cpuTh := thresholdParam(c, "cpu", h.cfg.CPUPercent)
	memTh := thresholdParam(c, "mem", h.cfg.MemPercent)
	storageTh := thresholdParam(c, "storage", h.cfg.StoragePercent)

	stats, err := h.k8s.ClusterResourceStats(ctx)
	if err != nil {
		log.Printf("monitor: cluster stats: %v", err)
		respondMonitor(c, true, gin.H{"error": "cluster stats: " + err.Error()})
		return
	}

	issues := capacityIssues(stats, cpuTh, memTh, storageTh)
	respondMonitor(c, len(issues) > 0, gin.H{
		"issues":     issues,
		"thresholds": gin.H{"cpu": cpuTh, "mem": memTh, "storage": storageTh},
	})
}

// capacityIssues compares cluster stats against the thresholds. A source
// being unreadable (metrics-server, Longhorn) is itself an issue: "can't see"
// must never look like "all green".
func capacityIssues(stats *k8s.ClusterStats, cpuTh, memTh, storageTh int) []CapacityIssue {
	issues := []CapacityIssue{}
	if !stats.MetricsAvailable {
		issues = append(issues, CapacityIssue{
			Resource: "metrics", Message: "metrics-server unreachable — live CPU/memory usage unknown",
		})
	}
	if !stats.StorageAvailable {
		issues = append(issues, CapacityIssue{
			Resource: "storage_stats", Message: "longhorn stats unreachable — disk usage unknown",
		})
	}

	for _, n := range stats.Nodes {
		if pct, ok := usagePercent(n.CPUUsedMillis, n.CPUAllocatableMillis); ok && pct >= cpuTh {
			issues = append(issues, CapacityIssue{Resource: "cpu", Node: n.Name, Percent: pct, Threshold: cpuTh})
		}
		if pct, ok := usagePercent(n.MemUsedBytes, n.MemAllocatableBytes); ok && pct >= memTh {
			issues = append(issues, CapacityIssue{Resource: "memory", Node: n.Name, Percent: pct, Threshold: memTh})
		}
	}
	for _, s := range stats.StorageNodes {
		if pct, ok := usagePercent(s.MaximumBytes-s.AvailableBytes, s.MaximumBytes); ok && pct >= storageTh {
			issues = append(issues, CapacityIssue{Resource: "storage", Node: s.Name, Percent: pct, Threshold: storageTh})
		}
	}
	return issues
}

func usagePercent(used, total int64) (int, bool) {
	if total <= 0 {
		return 0, false
	}
	return int(used * 100 / total), true
}

// thresholdParam reads a 1–100 percentage override from the query string,
// falling back to the configured default.
func thresholdParam(c *gin.Context, name string, defaultVal int) int {
	if v, err := strconv.Atoi(c.Query(name)); err == nil && v >= 1 && v <= 100 {
		return v
	}
	return defaultVal
}

// respondMonitor renders the shared healthy/alarm contract: 200 vs 503 with
// a "status" field the poller can also keyword-match on.
func respondMonitor(c *gin.Context, alarm bool, body gin.H) {
	body["status"] = "ok"
	code := http.StatusOK
	if alarm {
		body["status"] = "alarm"
		code = http.StatusServiceUnavailable
	}
	c.JSON(code, body)
}
