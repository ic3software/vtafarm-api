package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/metrics"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
)

type MetricsHandlerConfig struct {
	LiveInterval        time.Duration
	LiveWindow          time.Duration
	StreamMaxAge        time.Duration
	MaxPoints           int
	CacheTTL            time.Duration
	MaxStreamsPerUser   int
	MaxStreamsPerTarget int
}

type cachedSample struct {
	at       time.Time
	series   []metrics.Series
	warnings []string
}

type MetricsHandler struct {
	db      *gorm.DB
	k8s     *k8s.Client
	client  *metrics.Client
	config  MetricsHandlerConfig
	group   singleflight.Group
	mu      sync.Mutex
	cache   map[string]cachedSample
	streams map[string]int
}

func NewMetricsHandler(db *gorm.DB, k8sClient *k8s.Client, client *metrics.Client, config MetricsHandlerConfig) *MetricsHandler {
	return &MetricsHandler{db: db, k8s: k8sClient, client: client, config: config, cache: make(map[string]cachedSample), streams: make(map[string]int)}
}

type metricsContext struct {
	session model.SetupSession
	user    model.User
	pods    []podMeta
}

type podMeta struct {
	Name                  string `json:"name"`
	Component             string `json:"component"`
	Ready                 bool   `json:"ready"`
	StartedAt             string `json:"started_at,omitempty"`
	LastTerminationReason string `json:"last_termination_reason,omitempty"`
}

func (h *MetricsHandler) loadContext(c *gin.Context) (metricsContext, bool) {
	if h.k8s == nil || h.client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "metrics are not configured"})
		return metricsContext{}, false
	}
	userID := c.MustGet(middleware.ContextUserID).(uint)
	var user model.User
	if err := h.db.First(&user, userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return metricsContext{}, false
	}
	var session model.SetupSession
	if err := h.db.Where("vta_name = ? AND user_id = ?", c.Param("id"), userID).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load session"})
		}
		return metricsContext{}, false
	}
	// Provisioning namespaces are keyed by the database user ID, not the
	// public account identifier.
	pods, err := h.k8s.ListSessionPods(c.Request.Context(), fmt.Sprint(session.UserID), session.ID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to list session pods"})
		return metricsContext{}, false
	}
	result := metricsContext{session: session, user: user, pods: make([]podMeta, 0, len(pods))}
	for _, pod := range pods {
		ready := false
		startedAt := ""
		lastTerminationReason := ""
		if pod.Status.StartTime != nil {
			startedAt = pod.Status.StartTime.UTC().Format(time.RFC3339)
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
			}
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Terminated != nil {
				lastTerminationReason = status.State.Terminated.Reason
			} else if status.LastTerminationState.Terminated != nil {
				lastTerminationReason = status.LastTerminationState.Terminated.Reason
			}
		}
		result.pods = append(result.pods, podMeta{Name: pod.Name, Component: publicComponent(pod.Labels["app"]), Ready: ready, StartedAt: startedAt, LastTerminationReason: lastTerminationReason})
	}
	sort.Slice(result.pods, func(i, j int) bool { return result.pods[i].Name < result.pods[j].Name })
	return result, true
}

func publicComponent(label string) string {
	if label == "vta" || label == "fs-vta" {
		return "vta"
	}
	if len(label) > 3 && label[:3] == "fs-" {
		return label[3:]
	}
	return label
}

func (h *MetricsHandler) target(c *gin.Context, ctx metricsContext) (metrics.Target, bool) {
	target := metrics.Target{Scope: c.DefaultQuery("scope", "component"), Component: c.DefaultQuery("component", "vta"), Pod: c.Query("pod")}
	names := make([]string, 0, len(ctx.pods))
	for _, pod := range ctx.pods {
		names = append(names, pod.Name)
	}
	if err := metrics.ValidateTarget(target, ctx.session.Mode, names); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return metrics.Target{}, false
	}
	return target, true
}

// Meta provides all selector options and enforces ownership before the UI
// opens a potentially long-lived EventSource connection.
func (h *MetricsHandler) Meta(c *gin.Context) {
	ctx, ok := h.loadContext(c)
	if !ok {
		return
	}
	entitlement := metrics.EntitlementFor(ctx.user)
	components := []string{"vta"}
	if ctx.session.Mode == "full_stack" {
		components = []string{"vta", "mediator", "dids", "vtc", "trust-registry"}
	}
	c.JSON(http.StatusOK, gin.H{
		"plan": entitlement.Plan, "retention_days": entitlement.RetentionDays,
		"live_enabled": true, "live_interval_seconds": int(h.config.LiveInterval.Seconds()),
		"live_window_seconds": int(h.config.LiveWindow.Seconds()), "mode": ctx.session.Mode,
		"components": components, "pods": ctx.pods,
	})
}

type rangeResponse struct {
	Target   metrics.Target     `json:"target"`
	Start    int64              `json:"start"`
	End      int64              `json:"end"`
	Step     int64              `json:"step_seconds"`
	Series   []metrics.Series   `json:"series"`
	Summary  map[string]float64 `json:"summary"`
	Warnings []string           `json:"warnings,omitempty"`
}

func (h *MetricsHandler) Range(c *gin.Context) {
	ctx, ok := h.loadContext(c)
	if !ok {
		return
	}
	target, ok := h.target(c, ctx)
	if !ok {
		return
	}
	preset := c.DefaultQuery("range", "1h")
	if preset == "30d" && metrics.EntitlementFor(ctx.user).RetentionDays < 30 {
		c.JSON(http.StatusForbidden, gin.H{"error": "30 day history requires premium access", "code": "retention_limit"})
		return
	}
	start, end, step, err := metrics.Preset(preset, time.Now().UTC(), h.config.MaxPoints)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	response, err := h.queryRange(c.Request.Context(), ctx.session.ID, target, metrics.HistoricalDefinitions, start, end, step)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	setPodSummary(response.Summary, ctx.pods, target)
	c.JSON(http.StatusOK, response)
}

func (h *MetricsHandler) queryRange(ctx context.Context, sessionID uint, target metrics.Target, definitions []metrics.Definition, start, end time.Time, step time.Duration) (rangeResponse, error) {
	response := rangeResponse{Target: target, Start: start.Unix(), End: end.Unix(), Step: int64(step.Seconds()), Summary: make(map[string]float64)}
	var mu sync.Mutex
	group, queryCtx := errgroup.WithContext(ctx)
	for _, definition := range definitions {
		definition := definition
		group.Go(func() error {
			series, warnings, err := h.client.Range(queryCtx, definition.Name, metrics.RangeQuery(definition, sessionID, target, step), start, end, step)
			if err != nil {
				return fmt.Errorf("query %s: %w", definition.Name, err)
			}
			mu.Lock()
			response.Series = append(response.Series, series...)
			response.Warnings = append(response.Warnings, warnings...)
			mu.Unlock()
			return nil
		})
	}
	for _, definition := range metrics.SummaryDefinitions() {
		definition := definition
		group.Go(func() error {
			series, warnings, err := h.client.Instant(queryCtx, definition.Name, metrics.Query(definition, sessionID, target), end)
			if err != nil {
				return fmt.Errorf("query current %s: %w", definition.Name, err)
			}
			mu.Lock()
			response.Warnings = append(response.Warnings, warnings...)
			for _, item := range series {
				if len(item.Points) > 0 {
					response.Summary[definition.Name] += item.Points[len(item.Points)-1].Value
				}
			}
			mu.Unlock()
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return rangeResponse{}, err
	}
	sort.Slice(response.Series, func(i, j int) bool {
		if response.Series[i].Metric == response.Series[j].Metric {
			return response.Series[i].Labels["pod"] < response.Series[j].Labels["pod"]
		}
		return response.Series[i].Metric < response.Series[j].Metric
	})
	return response, nil
}

func (h *MetricsHandler) Live(c *gin.Context) {
	ctx, ok := h.loadContext(c)
	if !ok {
		return
	}
	target, ok := h.target(c, ctx)
	if !ok {
		return
	}
	release, ok := h.acquireStream(ctx.user.ID, ctx.session.ID, target)
	if !ok {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many active metrics streams"})
		return
	}
	defer release()
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming is unsupported"})
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	writeEvent(c, flusher, "status", gin.H{"state": "connected"})
	start := time.Now().UTC().Add(-h.config.LiveWindow)
	snapshot, err := h.queryRange(c.Request.Context(), ctx.session.ID, target, metrics.LiveDefinitions(), start, time.Now().UTC(), h.config.LiveInterval)
	if err != nil {
		writeEvent(c, flusher, "problem", gin.H{"message": err.Error(), "retryable": true})
	} else {
		setPodSummary(snapshot.Summary, ctx.pods, target)
		writeEvent(c, flusher, "snapshot", snapshot)
	}

	sampleTicker := time.NewTicker(h.config.LiveInterval)
	heartbeatTicker := time.NewTicker(15 * time.Second)
	expires := time.NewTimer(h.config.StreamMaxAge)
	defer sampleTicker.Stop()
	defer heartbeatTicker.Stop()
	defer expires.Stop()
	lastTimestamp := int64(0)
	degraded := err != nil
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-expires.C:
			writeEvent(c, flusher, "reconnect", gin.H{"reason": "reauthentication"})
			return
		case <-heartbeatTicker.C:
			_, _ = fmt.Fprint(c.Writer, ": heartbeat\n\n")
			flusher.Flush()
		case <-sampleTicker.C:
			series, warnings, queryErr := h.liveSample(c.Request.Context(), ctx.session.ID, target)
			if queryErr != nil {
				writeEvent(c, flusher, "problem", gin.H{"message": queryErr.Error(), "retryable": true})
				degraded = true
				continue
			}
			if degraded {
				recovered, recoveryErr := h.queryRange(c.Request.Context(), ctx.session.ID, target, metrics.LiveDefinitions(), time.Now().UTC().Add(-h.config.LiveWindow), time.Now().UTC(), h.config.LiveInterval)
				if recoveryErr != nil {
					continue
				}
				setPodSummary(recovered.Summary, ctx.pods, target)
				writeEvent(c, flusher, "snapshot", recovered)
				degraded = false
			}
			observed := latestTimestamp(series)
			if observed <= lastTimestamp {
				continue
			}
			lastTimestamp = observed
			writeEvent(c, flusher, "sample", gin.H{"observed_at": observed, "series": series, "warnings": warnings})
		}
	}
}

func setPodSummary(summary map[string]float64, pods []podMeta, target metrics.Target) {
	var ready, total int
	for _, pod := range pods {
		selected := target.Scope == "stack" ||
			(target.Scope == "component" && pod.Component == target.Component) ||
			(target.Scope == "pod" && pod.Name == target.Pod)
		if !selected {
			continue
		}
		total++
		if pod.Ready {
			ready++
		}
	}
	summary["pod_ready"] = float64(ready)
	summary["pod_total"] = float64(total)
}

func (h *MetricsHandler) acquireStream(userID, sessionID uint, target metrics.Target) (func(), bool) {
	targetBytes, _ := json.Marshal(target)
	userKey := fmt.Sprintf("user:%d", userID)
	targetKey := fmt.Sprintf("target:%d:%s", sessionID, targetBytes)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams[userKey] >= h.config.MaxStreamsPerUser || h.streams[targetKey] >= h.config.MaxStreamsPerTarget {
		return nil, false
	}
	h.streams[userKey]++
	h.streams[targetKey]++
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.streams[userKey]--
		h.streams[targetKey]--
	}, true
}

func (h *MetricsHandler) liveSample(ctx context.Context, sessionID uint, target metrics.Target) ([]metrics.Series, []string, error) {
	keyBytes, _ := json.Marshal(struct {
		Session uint           `json:"session"`
		Target  metrics.Target `json:"target"`
	}{sessionID, target})
	key := string(keyBytes)
	h.mu.Lock()
	if cached, ok := h.cache[key]; ok && time.Since(cached.at) < h.config.CacheTTL {
		h.mu.Unlock()
		return cached.series, cached.warnings, nil
	}
	h.mu.Unlock()
	value, err, _ := h.group.Do(key, func() (any, error) {
		var series []metrics.Series
		var warnings []string
		var mu sync.Mutex
		group, queryCtx := errgroup.WithContext(ctx)
		for _, definition := range metrics.LiveDefinitions() {
			definition := definition
			group.Go(func() error {
				items, itemWarnings, queryErr := h.client.Instant(queryCtx, definition.Name, metrics.Query(definition, sessionID, target), time.Now().UTC())
				if queryErr != nil {
					return queryErr
				}
				mu.Lock()
				series = append(series, items...)
				warnings = append(warnings, itemWarnings...)
				mu.Unlock()
				return nil
			})
		}
		if queryErr := group.Wait(); queryErr != nil {
			return nil, queryErr
		}
		result := cachedSample{at: time.Now(), series: series, warnings: warnings}
		h.mu.Lock()
		h.cache[key] = result
		h.mu.Unlock()
		return result, nil
	})
	if err != nil {
		return nil, nil, err
	}
	result := value.(cachedSample)
	return result.series, result.warnings, nil
}

func latestTimestamp(series []metrics.Series) int64 {
	var latest int64
	for _, item := range series {
		for _, point := range item.Points {
			if point.Timestamp > latest {
				latest = point.Timestamp
			}
		}
	}
	return latest
}

func writeEvent(c *gin.Context, flusher http.Flusher, event string, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, encoded)
	flusher.Flush()
}
