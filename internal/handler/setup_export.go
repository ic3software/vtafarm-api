package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/metrics"
	"github.com/ic3software/vtafarm-api/internal/model"
)

const exportLogTailLines = 10000

const (
	logSourceRetained   = "retained"
	logSourceKubernetes = "kubernetes"
)

// The mediator's config sits under conf/; every other component's is at the
// mount root (docs/full-stack-setup-design.md §"Path ownership").
type exportComponent struct {
	name       string
	selector   string
	configPath string
}

func exportComponents(s *model.SetupSession) []exportComponent {
	if !s.IsFullStack() {
		return []exportComponent{
			{"vta", fmt.Sprintf("app=vta,session-id=%d", s.ID), "/work/vta/config.toml"},
		}
	}
	components := []exportComponent{
		{"vta", fmt.Sprintf("app=fs-vta,session-id=%d", s.ID), "/work/vta/config.toml"},
		{"mediator", fmt.Sprintf("app=fs-mediator,session-id=%d", s.ID), "/work/mediator/conf/mediator.toml"},
		{"dids", fmt.Sprintf("app=fs-dids,session-id=%d", s.ID), "/work/dids/config.toml"},
		{"vtc", fmt.Sprintf("app=fs-vtc,session-id=%d", s.ID), "/work/vtc/config.toml"},
	}
	if s.TrustRegistryEnabled && s.TrustRegistryDid != "" {
		components = append(components, exportComponent{"trust-registry", fmt.Sprintf("app=fs-trust-registry,session-id=%d", s.ID), ""})
	}
	return components
}

// GET /api/v1/setup/:id/export/configs
func (h *SetupHandler) ExportConfigs(c *gin.Context) {
	if s := h.userSession(c); s != nil {
		h.exportArchive(c, s, exportKindConfigs)
	}
}

// GET /api/v1/setup/:id/export/logs
func (h *SetupHandler) ExportLogs(c *gin.Context) {
	if s := h.userSession(c); s != nil {
		h.exportRetainedLogs(c, s, false)
	}
}

// AdminExportConfigs — GET /api/v1/admin/setup-sessions/:id/export/configs.
func (h *SetupHandler) AdminExportConfigs(c *gin.Context) {
	if s := h.adminSession(c); s != nil {
		h.exportArchive(c, s, exportKindConfigs)
	}
}

// AdminExportLogs — GET /api/v1/admin/setup-sessions/:id/export/logs.
func (h *SetupHandler) AdminExportLogs(c *gin.Context) {
	if s := h.adminSession(c); s != nil {
		h.exportRetainedLogs(c, s, true)
	}
}

type exportKind string

const (
	exportKindConfigs exportKind = "configs"
	exportKindLogs    exportKind = "logs"
)

// exportArchive zips one file per component. Best-effort: what could not be
// read is named in errors.txt, and only an empty archive is an error — which
// is why it is built in memory before anything is written to the response.
func (h *SetupHandler) exportArchive(c *gin.Context, session *model.SetupSession, kind exportKind) {
	if h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s not configured"})
		return
	}

	ns := h.k8s.UserNamespace(fmt.Sprintf("%d", session.UserID))
	ctx := c.Request.Context()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var failures []string
	collected := 0

	for _, comp := range exportComponents(session) {
		name, body, err := h.exportOne(ctx, ns, comp, kind)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", comp.name, err))
			continue
		}
		f, err := zw.Create(name)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build archive"})
			return
		}
		if _, err := f.Write([]byte(body)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build archive"})
			return
		}
		collected++
	}

	if collected == 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("nothing to export — %s", strings.Join(failures, "; ")),
		})
		return
	}

	if len(failures) > 0 {
		f, err := zw.Create("errors.txt")
		if err == nil {
			fmt.Fprintf(f, "These components could not be read:\n\n%s\n", strings.Join(failures, "\n"))
		}
	}

	if err := zw.Close(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build archive"})
		return
	}

	filename := fmt.Sprintf("%s-%s-%s.zip", session.VtaName, kind, time.Now().UTC().Format("20060102-150405"))
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	c.Data(http.StatusOK, "application/zip", buf.Bytes())
}

// exportOne returns the archive member name and body for one component.
func (h *SetupHandler) exportOne(ctx context.Context, ns string, comp exportComponent, kind exportKind) (string, string, error) {
	if kind == exportKindLogs {
		body, err := h.k8s.PodLogsSnapshot(ctx, ns, comp.selector, exportLogTailLines)
		return comp.name + ".log", body, err
	}

	if comp.name == "trust-registry" {
		content, err := h.k8s.RegistryEnvironment(ctx, ns, comp.selector)
		return "trust-registry.env", content, err
	}
	pod, container, err := h.k8s.RunningPod(ctx, ns, comp.selector)
	if err != nil {
		return "", "", err
	}
	body, err := h.k8s.ExecCapture(ctx, ns, pod, container,
		[]string{"sh", "-c", "cat " + shellQuote(comp.configPath)})
	if err != nil {
		return "", "", err
	}
	return comp.name + "-config.toml", body, nil
}

type logExportManifest struct {
	RetentionDays int       `json:"retention_days"`
	RangeDays     int       `json:"range_days"`
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	Entries       int       `json:"entries"`
}

type retainedLogFile struct {
	name string
	path string
}

func (h *SetupHandler) exportRetainedLogs(c *gin.Context, session *model.SetupSession, admin bool) {
	var owner model.User
	if err := h.db.WithContext(c.Request.Context()).First(&owner, session.UserID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load log retention entitlement"})
		return
	}
	end := time.Now().UTC()
	entitlement := logExportEntitlement(owner, admin)
	rangeDays, err := requestedLogRange(c.Query("range"), entitlement.RetentionDays)
	if errors.Is(err, errLogRangeLimit) {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error(), "code": "retention_limit"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.logSource() == logSourceKubernetes {
		h.exportKubernetesLogs(c, session, nil)
		return
	}
	if h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "logging backend is not configured"})
		return
	}
	start := logExportStart(session.CreatedAt, end, rangeDays)

	ns := h.k8s.UserNamespace(strconv.FormatUint(uint64(session.UserID), 10))
	components := exportComponents(session)
	logFiles := make([]retainedLogFile, 0, len(components))
	defer func() {
		for _, file := range logFiles {
			os.Remove(file.path)
		}
	}()
	entries := 0
	for _, comp := range components {
		logsFile, err := os.CreateTemp("", "vtafarm-session-"+comp.name+"-*.log")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare log export"})
			return
		}
		logFiles = append(logFiles, retainedLogFile{name: comp.name + ".log", path: logsFile.Name()})
		query := componentLogQuery(ns, session, comp)
		count, queryErr := h.logquery.WriteRange(c.Request.Context(), query, start, end, logsFile)
		closeErr := logsFile.Close()
		if h.logSourceAfter(queryErr) == logSourceKubernetes {
			h.exportKubernetesLogs(c, session, queryErr)
			return
		}
		if closeErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize log export"})
			return
		}
		entries += count
	}

	archive, err := os.CreateTemp("", "vtafarm-session-logs-*.zip")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare log archive"})
		return
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	manifest := logExportManifest{
		RetentionDays: entitlement.RetentionDays,
		RangeDays:     rangeDays, Start: start, End: end, Entries: entries,
	}
	if err := writeRetainedLogArchive(archive, logFiles, manifest); err != nil {
		archive.Close()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build log archive"})
		return
	}
	if err := archive.Close(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize log archive"})
		return
	}

	c.Header("X-Log-Retention-Days", strconv.Itoa(entitlement.RetentionDays))
	c.Header("X-Log-Range-Days", strconv.Itoa(rangeDays))
	c.Header("X-Log-Source", logSourceRetained)
	filename := fmt.Sprintf("%s-logs-%s.zip", session.VtaName, end.Format("20060102-150405"))
	c.FileAttachment(archivePath, filename)
}

func (h *SetupHandler) exportKubernetesLogs(c *gin.Context, session *model.SetupSession, retainedErr error) {
	if retainedErr != nil {
		log.Printf("[log-export] retained logs unavailable for session %d; falling back to Kubernetes: %v", session.ID, retainedErr)
	}
	c.Header("X-Log-Source", logSourceKubernetes)
	h.exportArchive(c, session, exportKindLogs)
}

func (h *SetupHandler) logSource() string {
	return h.logSourceAfter(nil)
}

func (h *SetupHandler) logSourceAfter(retainedErr error) string {
	if h.logquery == nil || retainedErr != nil {
		return logSourceKubernetes
	}
	return logSourceRetained
}

func logExportEntitlement(owner model.User, admin bool) metrics.Entitlement {
	entitlement := metrics.EntitlementFor(owner)
	if admin {
		entitlement = metrics.Entitlement{Plan: "premium", RetentionDays: metrics.PremiumRetentionDays}
	}
	return entitlement
}

var errLogRangeLimit = errors.New("requested log range exceeds plan retention")

func requestedLogRange(preset string, maxDays int) (int, error) {
	if preset == "" {
		return maxDays, nil
	}
	daysByPreset := map[string]int{"1d": 1, "7d": 7, "30d": 30}
	days, ok := daysByPreset[preset]
	if !ok {
		return 0, fmt.Errorf("range must be 1d, 7d, or 30d")
	}
	if days > maxDays {
		return 0, errLogRangeLimit
	}
	return days, nil
}

func logExportStart(sessionCreated, end time.Time, rangeDays int) time.Time {
	start := end.Add(-time.Duration(rangeDays) * 24 * time.Hour)
	if sessionCreated.After(start) {
		start = sessionCreated.UTC()
	}
	return start
}

func componentLogQuery(namespace string, session *model.SetupSession, comp exportComponent) string {
	app := "vta"
	if session.IsFullStack() {
		app = "fs-" + comp.name
	}
	return fmt.Sprintf(`{namespace=%s, app=%s} | session_id=%s`,
		strconv.Quote(namespace), strconv.Quote(app), strconv.Quote(strconv.FormatUint(uint64(session.ID), 10)))
}

func writeRetainedLogArchive(dst *os.File, logFiles []retainedLogFile, manifest logExportManifest) (err error) {
	zw := zip.NewWriter(dst)
	defer func() {
		if closeErr := zw.Close(); err == nil {
			err = closeErr
		}
	}()
	for _, file := range logFiles {
		logs, openErr := os.Open(file.path)
		if openErr != nil {
			return openErr
		}
		member, createErr := zw.Create(file.name)
		if createErr == nil {
			_, createErr = io.Copy(member, logs)
		}
		logs.Close()
		if createErr != nil {
			return createErr
		}
	}
	manifestMember, err := zw.Create("manifest.json")
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err == nil {
		_, err = manifestMember.Write(append(encoded, '\n'))
	}
	if err != nil {
		return err
	}
	return nil
}
