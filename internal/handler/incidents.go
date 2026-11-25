package handler

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
)

const incidentLogPadding = 5 * time.Minute

var tenantAlertTypes = map[string]string{
	"VTAFarmCPUUsageHigh":          "cpu_usage_high",
	"VTAFarmCPUUsageCritical":      "cpu_usage_critical",
	"VTAFarmMemoryUsageHigh":       "memory_usage_high",
	"VTAFarmMemoryUsageCritical":   "memory_usage_critical",
	"VTAFarmStorageUsageHigh":      "storage_usage_high",
	"VTAFarmStorageUsageCritical":  "storage_usage_critical",
	"VTAFarmMemoryOOMKilled":       "memory_oom",
	"VTAFarmComponentCrashLooping": "component_crash_looping",
	"VTAFarmComponentUnavailable":  "component_unavailable",
}

type alertmanagerWebhook struct {
	Status string              `json:"status"`
	Alerts []alertmanagerAlert `json:"alerts"`
}

type alertmanagerAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
	Fingerprint string            `json:"fingerprint"`
}

type incidentResponse struct {
	model.Incident
	SessionID       string `json:"session_id"`
	DowntimeSeconds *int64 `json:"downtime_seconds,omitempty"`
	Suppressed      bool   `json:"suppressed,omitempty"`
}

func (h *SetupHandler) ConfigureAlertmanagerWebhook(token string) {
	h.alertmanagerWebhookToken = strings.TrimSpace(token)
}

func (h *SetupHandler) AlertmanagerWebhook(c *gin.Context) {
	if h.alertmanagerWebhookToken == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "alertmanager webhook is not configured"})
		return
	}
	authorization := c.GetHeader("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid alertmanager webhook token"})
		return
	}
	provided := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	if len(provided) != len(h.alertmanagerWebhookToken) || subtle.ConstantTimeCompare([]byte(provided), []byte(h.alertmanagerWebhookToken)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid alertmanager webhook token"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	var payload alertmanagerWebhook
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid alertmanager webhook payload"})
		return
	}
	accepted, ignored := 0, 0
	for _, alert := range payload.Alerts {
		ok, err := h.recordTenantAlert(c, alert)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not record alert"})
			return
		}
		if ok {
			accepted++
		} else {
			ignored++
		}
	}
	c.JSON(http.StatusAccepted, gin.H{"accepted": accepted, "ignored": ignored})
}

func (h *SetupHandler) recordTenantAlert(c *gin.Context, alert alertmanagerAlert) (bool, error) {
	alertName := alert.Labels["alertname"]
	incidentType, allowed := tenantAlertTypes[alertName]
	if !allowed || alert.Labels["audience"] != "tenant" {
		return false, nil
	}
	sessionID64, err := strconv.ParseUint(alert.Labels["label_session_id"], 10, 64)
	if err != nil || sessionID64 == 0 || alert.Fingerprint == "" || alert.StartsAt.IsZero() {
		return false, nil
	}
	var session model.SetupSession
	if err := h.db.WithContext(c.Request.Context()).First(&session, uint(sessionID64)).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	status := alert.Status
	if status != model.IncidentFiring && status != model.IncidentResolved {
		return false, nil
	}
	component := publicIncidentComponent(alert.Labels["label_app"])
	if component == "" {
		return false, nil
	}
	now := time.Now().UTC()
	labelsJSON, _ := json.Marshal(alert.Labels)
	annotationsJSON, _ := json.Marshal(alert.Annotations)
	usagePercent := parseUsagePercent(alert.Annotations["usage_percent"])
	severity := alert.Labels["severity"]
	if severity != "warning" && severity != "critical" {
		severity = "warning"
	}
	title := strings.TrimSpace(alert.Annotations["summary"])
	if title == "" {
		title = alertName
	}
	message := strings.TrimSpace(alert.Annotations["description"])
	if message == "" {
		message = title
	}
	resolvedAt := (*time.Time)(nil)
	if status == model.IncidentResolved {
		end := alert.EndsAt.UTC()
		if end.IsZero() || end.Before(alert.StartsAt) {
			end = now
		}
		resolvedAt = &end
	}
	suppressed, err := h.suppressIncident(c, &session)
	if err != nil {
		return false, err
	}
	lookup := model.Incident{Cluster: alert.Labels["cluster"], Fingerprint: alert.Fingerprint, StartedAt: alert.StartsAt.UTC()}
	if lookup.Cluster == "" {
		lookup.Cluster = "unknown"
	}
	return true, h.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var existing model.Incident
		err := tx.Where("cluster = ? AND fingerprint = ? AND started_at = ?", lookup.Cluster, lookup.Fingerprint, lookup.StartedAt).First(&existing).Error
		if err == nil {
			if existing.Status == model.IncidentResolved && status == model.IncidentFiring {
				return nil
			}
			updates := map[string]any{
				"status": status, "severity": severity, "title": title, "message": message,
				"usage_percent": usagePercent, "resolved_at": resolvedAt, "last_seen_at": now,
				"suppressed": suppressed || existing.Suppressed, "labels": string(labelsJSON), "annotations": string(annotationsJSON), "updated_at": now,
			}
			return tx.Model(&existing).Updates(updates).Error
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		incident := model.Incident{
			PublicID: uuid.NewString(), SetupSessionID: session.ID,
			Cluster: lookup.Cluster, Fingerprint: lookup.Fingerprint, AlertName: alertName,
			IncidentType: incidentType, Component: component, Resource: alert.Labels["resource"],
			Severity: severity, Status: status, Title: title, Message: message, UsagePercent: usagePercent,
			StartedAt: lookup.StartedAt, ResolvedAt: resolvedAt, LastSeenAt: now, Suppressed: suppressed,
			LabelsJSON: string(labelsJSON), AnnotationsJSON: string(annotationsJSON),
		}
		return tx.Create(&incident).Error
	})
}

func (h *SetupHandler) suppressIncident(c *gin.Context, session *model.SetupSession) (bool, error) {
	if session.Status != "running" {
		return true, nil
	}
	var count int64
	err := h.db.WithContext(c.Request.Context()).Table("upgrade_tasks").
		Where("session_id = ? AND status IN ?", session.ID, []string{
			model.UpgradeTaskPending, model.UpgradeTaskRunning, model.UpgradeTaskRollingBack,
		}).Count(&count).Error
	return count > 0, err
}

func publicIncidentComponent(label string) string {
	switch label {
	case "vta", "fs-vta":
		return "vta"
	case "fs-mediator":
		return "mediator"
	case "fs-dids":
		return "dids"
	case "fs-vtc":
		return "vtc"
	case "fs-trust-registry":
		return "trust-registry"
	default:
		return ""
	}
}

func parseUsagePercent(value string) *float64 {
	value = strings.TrimSpace(strings.TrimSuffix(value, "%"))
	if value == "" {
		return nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return nil
	}
	return &parsed
}

func (h *SetupHandler) UserIncidents(c *gin.Context) {
	userID := c.MustGet(middleware.ContextUserID).(uint)
	h.listIncidents(c, h.db.Joins("JOIN setup_sessions ON setup_sessions.id = incidents.setup_session_id").
		Where("setup_sessions.user_id = ? AND incidents.suppressed = FALSE", userID), true)
}

func (h *SetupHandler) SessionIncidents(c *gin.Context) {
	if session := h.userSession(c); session != nil {
		h.listIncidents(c, h.db.Where("setup_session_id = ? AND suppressed = FALSE", session.ID), true)
	}
}

func (h *SetupHandler) AdminIncidents(c *gin.Context) {
	h.listIncidents(c, h.db.Model(&model.Incident{}), false)
}

func (h *SetupHandler) AdminSessionIncidents(c *gin.Context) {
	if session := h.adminSession(c); session != nil {
		h.listIncidents(c, h.db.Where("setup_session_id = ?", session.ID), false)
	}
}

func (h *SetupHandler) listIncidents(c *gin.Context, query *gorm.DB, hideSuppressed bool) {
	if status := c.Query("status"); status != "" {
		if status != model.IncidentFiring && status != model.IncidentResolved {
			c.JSON(http.StatusBadRequest, gin.H{"error": "status must be firing or resolved"})
			return
		}
		query = query.Where("incidents.status = ?", status)
	}
	if c.Query("unread") == "true" {
		query = query.Where("incidents.read_at IS NULL")
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit < 1 || limit > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be between 1 and 100"})
		return
	}
	var incidents []model.Incident
	if err := query.Order("incidents.started_at DESC").Limit(limit).Find(&incidents).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load incidents"})
		return
	}
	responses := make([]incidentResponse, 0, len(incidents))
	for _, incident := range incidents {
		if hideSuppressed && incident.Suppressed {
			continue
		}
		var session model.SetupSession
		if err := h.db.Select("vta_name").First(&session, incident.SetupSessionID).Error; err != nil {
			continue
		}
		responses = append(responses, toIncidentResponse(incident, session.VtaName))
	}
	c.JSON(http.StatusOK, gin.H{"incidents": responses})
}

func toIncidentResponse(incident model.Incident, sessionID string) incidentResponse {
	return incidentResponse{
		Incident: incident, SessionID: sessionID, DowntimeSeconds: incident.DowntimeSeconds(), Suppressed: incident.Suppressed,
	}
}

func (h *SetupHandler) GetSessionIncident(c *gin.Context) {
	if session := h.userSession(c); session != nil {
		h.getIncident(c, session, false)
	}
}

func (h *SetupHandler) AdminGetSessionIncident(c *gin.Context) {
	if session := h.adminSession(c); session != nil {
		h.getIncident(c, session, true)
	}
}

func (h *SetupHandler) getIncident(c *gin.Context, session *model.SetupSession, admin bool) {
	incident := h.findIncident(c, session.ID, admin)
	if incident == nil {
		return
	}
	c.JSON(http.StatusOK, toIncidentResponse(*incident, session.VtaName))
}

func (h *SetupHandler) MarkSessionIncidentRead(c *gin.Context) {
	session := h.userSession(c)
	if session == nil {
		return
	}
	incident := h.findIncident(c, session.ID, false)
	if incident == nil {
		return
	}
	var input struct {
		Read *bool `json:"read"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || input.Read == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read is required"})
		return
	}
	var readAt *time.Time
	if *input.Read {
		now := time.Now().UTC()
		readAt = &now
	}
	if err := h.db.Model(incident).Update("read_at", readAt).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not update incident"})
		return
	}
	incident.ReadAt = readAt
	c.JSON(http.StatusOK, toIncidentResponse(*incident, session.VtaName))
}

func (h *SetupHandler) findIncident(c *gin.Context, sessionID uint, admin bool) *model.Incident {
	query := h.db.Where("public_id = ? AND setup_session_id = ?", c.Param("incident_id"), sessionID)
	if !admin {
		query = query.Where("suppressed = FALSE")
	}
	var incident model.Incident
	if err := query.First(&incident).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
		return nil
	}
	return &incident
}

func (h *SetupHandler) SessionIncidentLogs(c *gin.Context) {
	if session := h.userSession(c); session != nil {
		h.exportIncidentLogs(c, session, false)
	}
}

func (h *SetupHandler) AdminSessionIncidentLogs(c *gin.Context) {
	if session := h.adminSession(c); session != nil {
		h.exportIncidentLogs(c, session, true)
	}
}

func (h *SetupHandler) exportIncidentLogs(c *gin.Context, session *model.SetupSession, admin bool) {
	incident := h.findIncident(c, session.ID, admin)
	if incident == nil {
		return
	}
	if h.logquery == nil || h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "retained logging is not configured"})
		return
	}
	var owner model.User
	if err := h.db.First(&owner, session.UserID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load log retention entitlement"})
		return
	}
	entitlement := logExportEntitlement(owner, admin)
	now := time.Now().UTC()
	start := incident.StartedAt.Add(-incidentLogPadding)
	if session.CreatedAt.After(start) {
		start = session.CreatedAt.UTC()
	}
	if start.Before(now.Add(-time.Duration(entitlement.RetentionDays) * 24 * time.Hour)) {
		c.JSON(http.StatusGone, gin.H{"error": "incident logs are outside the retention window"})
		return
	}
	end := now
	if incident.ResolvedAt != nil {
		end = incident.ResolvedAt.Add(incidentLogPadding)
		if end.After(now) {
			end = now
		}
	}
	var component *exportComponent
	for _, candidate := range exportComponents(session) {
		if candidate.name == incident.Component {
			copy := candidate
			component = &copy
			break
		}
	}
	if component == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "incident component is not part of this session"})
		return
	}
	logsFile, err := os.CreateTemp("", "vtafarm-incident-*.log")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare incident log export"})
		return
	}
	logsPath := logsFile.Name()
	defer os.Remove(logsPath)
	namespace := h.k8s.UserNamespace(strconv.FormatUint(uint64(session.UserID), 10))
	entries, queryErr := h.logquery.WriteRange(c.Request.Context(), componentLogQuery(namespace, session, *component), start, end, logsFile)
	closeErr := logsFile.Close()
	if queryErr != nil || closeErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not query incident logs"})
		return
	}
	archive, err := os.CreateTemp("", "vtafarm-incident-logs-*.zip")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare incident log archive"})
		return
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	rangeDays := int(end.Sub(start)/(24*time.Hour)) + 1
	manifest := logExportManifest{RetentionDays: entitlement.RetentionDays, RangeDays: rangeDays, Start: start, End: end, Entries: entries}
	if err := writeRetainedLogArchive(archive, []retainedLogFile{{name: component.name + ".log", path: logsPath}}, manifest); err != nil {
		archive.Close()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build incident log archive"})
		return
	}
	if err := archive.Close(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize incident log archive"})
		return
	}
	c.Header("X-Log-Source", logSourceRetained)
	c.Header("X-Log-Retention-Days", strconv.Itoa(entitlement.RetentionDays))
	c.FileAttachment(archivePath, fmt.Sprintf("%s-%s-%s-logs.zip", session.VtaName, incident.Component, incident.PublicID))
}
