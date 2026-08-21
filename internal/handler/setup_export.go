package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/model"
)

const exportLogTailLines = 10000

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
	return []exportComponent{
		{"vta", fmt.Sprintf("app=fs-vta,session-id=%d", s.ID), "/work/vta/config.toml"},
		{"mediator", fmt.Sprintf("app=fs-mediator,session-id=%d", s.ID), "/work/mediator/conf/mediator.toml"},
		{"dids", fmt.Sprintf("app=fs-dids,session-id=%d", s.ID), "/work/dids/config.toml"},
		{"vtc", fmt.Sprintf("app=fs-vtc,session-id=%d", s.ID), "/work/vtc/config.toml"},
	}
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
		h.exportArchive(c, s, exportKindLogs)
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
		h.exportArchive(c, s, exportKindLogs)
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
