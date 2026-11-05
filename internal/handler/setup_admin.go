package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/resourceprofile"
)

// adminSessionsPageSize is fixed server-side regardless of what the client asks for.
const adminSessionsPageSize = 20

type adminSessionStoredResource struct {
	Component     string `json:"component"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

type adminSessionResourceDefault struct {
	Component     string `json:"component"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

type adminSessionQueryItem struct {
	ID                   uint                         `json:"id"`
	UserID               uint                         `json:"user_id"`
	UserUniqueID         string                       `json:"user_unique_id"`
	UserEmail            *string                      `json:"user_email"`
	VtaName              string                       `json:"vta_name"`
	VtcName              string                       `json:"vtc_name"`
	Mode                 string                       `json:"mode"`
	DomainType           string                       `json:"domain_type"`
	Status               string                       `json:"status"`
	FailedStage          string                       `json:"failed_stage"`
	ErrorMsg             string                       `json:"error_msg"`
	FQDN                 string                       `json:"fqdn"`
	VtaImage             string                       `json:"vta_image"`
	MediatorImage        string                       `json:"mediator_image"`
	DidsImage            string                       `json:"dids_image"`
	VtcImage             string                       `json:"vtc_image"`
	TrustRegistryEnabled bool                         `json:"trust_registry_enabled"`
	TrustRegistryImage   string                       `json:"trust_registry_image"`
	TrustRegistryDid     string                       `json:"trust_registry_did"`
	CreatedAt            time.Time                    `json:"created_at"`
	ConnectionSource     string                       `json:"connection_source"`
	ProviderSessionID    *uint                        `json:"provider_session_id"`
	Provider             *string                      `json:"provider"`
	Resources            []adminSessionStoredResource `json:"resources"`
}

type adminSessionQueryResult struct {
	Total            int64  `gorm:"column:total"`
	Statuses         []byte `gorm:"column:statuses"`
	ResourceDefaults []byte `gorm:"column:resource_defaults"`
	Items            []byte `gorm:"column:items"`
}

type adminSessionResourceSummary struct {
	Component     string `json:"component"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
	Customized    bool   `json:"customized"`
}

type adminSessionItem struct {
	ID                   uint                          `json:"id"`
	UserID               uint                          `json:"user_id"`
	UserUniqueID         string                        `json:"user_unique_id"`
	UserEmail            *string                       `json:"user_email,omitempty"`
	VtaName              string                        `json:"vta_name"`
	VtcName              string                        `json:"vtc_name,omitempty"`
	Mode                 string                        `json:"mode"`
	DomainType           string                        `json:"domain_type"`
	Status               string                        `json:"status"`
	FailedStage          string                        `json:"failed_stage,omitempty"`
	ErrorMsg             string                        `json:"error_msg,omitempty"`
	FQDN                 string                        `json:"fqdn"`
	VtaImage             string                        `json:"vta_image,omitempty"`
	MediatorImage        string                        `json:"mediator_image,omitempty"`
	DidsImage            string                        `json:"dids_image,omitempty"`
	VtcImage             string                        `json:"vtc_image,omitempty"`
	TrustRegistryEnabled bool                          `json:"trust_registry_enabled,omitempty"`
	TrustRegistryImage   string                        `json:"trust_registry_image,omitempty"`
	CreatedAt            string                        `json:"created_at"`
	ConnectionSource     string                        `json:"connection_source,omitempty"`
	Provider             string                        `json:"provider,omitempty"`
	ProviderGone         bool                          `json:"provider_gone,omitempty"`
	Resources            []adminSessionResourceSummary `json:"resources"`
}

// AdminListSessions — GET /api/v1/admin/setup-sessions?page=N&mode=M&q=Q (admin only).
// Lists ALL users' setup sessions ordered by id DESC (newest first), paginated
// at adminSessionsPageSize per page. An optional mode filter narrows the list
// (and total) to one setup mode. Search matches session names and owner
// identifiers. This endpoint is deliberately database-only: live Kubernetes
// state belongs to the on-demand resource editor, not a paginated index.
func (h *SetupHandler) AdminListSessions(c *gin.Context) {
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page must be a positive integer"})
		return
	}

	mode := c.Query("mode")
	switch mode {
	case "", model.ModeVtaOnly, model.ModeFullStack:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid mode"})
		return
	}
	status := strings.TrimSpace(c.Query("status"))
	if utf8.RuneCountInString(status) > 50 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be 50 characters or fewer"})
		return
	}
	search := strings.TrimSpace(c.Query("q"))
	if utf8.RuneCountInString(search) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "q must be 200 characters or fewer"})
		return
	}

	baseConditions := []string{"TRUE"}
	queryArgs := make([]any, 0, 10)
	if mode != "" {
		baseConditions = append(baseConditions, "s.mode = ?")
		queryArgs = append(queryArgs, mode)
	}
	if search != "" {
		pattern := "%" + strings.NewReplacer(
			`\`, `\\`,
			`%`, `\%`,
			`_`, `\_`,
		).Replace(search) + "%"
		condition := `(s.vta_name ILIKE ? ESCAPE '\' OR s.vtc_name ILIKE ? ESCAPE '\' OR EXISTS (` +
			`SELECT 1 FROM users search_owner WHERE search_owner.id = s.user_id AND (` +
			`search_owner.unique_id ILIKE ? ESCAPE '\' OR COALESCE(search_owner.email, '') ILIKE ? ESCAPE '\'))`
		searchArgs := []any{pattern, pattern, pattern, pattern}
		if id, err := strconv.ParseUint(strings.TrimPrefix(search, "#"), 10, 64); err == nil {
			condition += " OR s.id = ? OR s.user_id = ?"
			searchArgs = append(searchArgs, id, id)
		}
		condition += ")"
		baseConditions = append(baseConditions, condition)
		queryArgs = append(queryArgs, searchArgs...)
	}

	query := `
WITH base AS (
    SELECT s.* FROM setup_sessions s WHERE ` + strings.Join(baseConditions, " AND ") + `
), filtered AS (
    SELECT b.* FROM base b WHERE (? = '' OR b.status = ?)
), paged AS (
    SELECT f.* FROM filtered f ORDER BY f.id DESC LIMIT ? OFFSET ?
), page_items AS (
    SELECT
        p.id, p.user_id, owner.unique_id AS user_unique_id, owner.email AS user_email,
        p.vta_name, p.vtc_name, p.mode, p.domain_type, p.status, p.failed_stage,
        p.error_msg, p.subdomain || '.' || p.domain AS fqdn,
        p.vta_image, p.mediator_image, p.dids_image, p.vtc_image,
        p.trust_registry_enabled, p.trust_registry_image, p.trust_registry_did,
        p.created_at, p.connection_source, p.provider_session_id,
        provider.vta_name AS provider,
        COALESCE((
            SELECT jsonb_agg(jsonb_build_object(
                'component', wr.component,
                'memory_request', wr.memory_request,
                'memory_limit', wr.memory_limit
            ) ORDER BY wr.component)
            FROM workload_resources wr WHERE wr.setup_session_id = p.id
        ), '[]'::jsonb) AS resources
    FROM paged p
    JOIN users owner ON owner.id = p.user_id
    LEFT JOIN setup_sessions provider ON provider.id = p.provider_session_id
)
SELECT
    (SELECT COUNT(*) FROM filtered) AS total,
    COALESCE((
        SELECT jsonb_agg(options.status ORDER BY options.status)
        FROM (SELECT DISTINCT b.status FROM base b) options
    ), '[]'::jsonb) AS statuses,
    COALESCE((
        SELECT jsonb_agg(jsonb_build_object(
            'component', rd.component,
            'memory_request', rd.memory_request,
            'memory_limit', rd.memory_limit
        ) ORDER BY rd.component) FROM resource_defaults rd
    ), '[]'::jsonb) AS resource_defaults,
    COALESCE((
        SELECT jsonb_agg(to_jsonb(item) ORDER BY item.id DESC) FROM page_items item
    ), '[]'::jsonb) AS items`
	queryArgs = append(queryArgs, status, status, adminSessionsPageSize, (page-1)*adminSessionsPageSize)
	var queryResult adminSessionQueryResult
	if err := h.db.WithContext(c.Request.Context()).Raw(query, queryArgs...).Scan(&queryResult).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch sessions"})
		return
	}

	var statuses []string
	var defaults []adminSessionResourceDefault
	var queryItems []adminSessionQueryItem
	if err := json.Unmarshal(queryResult.Statuses, &statuses); err != nil {
		log.Printf("decode admin session statuses: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not decode session statuses"})
		return
	}
	if err := json.Unmarshal(queryResult.ResourceDefaults, &defaults); err != nil {
		log.Printf("decode admin session resource defaults: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not decode resource defaults"})
		return
	}
	if err := json.Unmarshal(queryResult.Items, &queryItems); err != nil {
		log.Printf("decode admin sessions: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not decode sessions"})
		return
	}
	defaultRows := make([]model.ResourceDefault, len(defaults))
	for i, item := range defaults {
		defaultRows[i] = model.ResourceDefault{
			Component: item.Component, MemoryRequest: item.MemoryRequest, MemoryLimit: item.MemoryLimit,
		}
	}
	profiles, err := resourceprofile.FromRows(defaultRows)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load resource defaults"})
		return
	}

	items := make([]adminSessionItem, len(queryItems))
	for i, row := range queryItems {
		session := model.SetupSession{
			ID: row.ID, UserID: row.UserID, Status: row.Status, FailedStage: row.FailedStage,
			Mode: row.Mode, ErrorMsg: row.ErrorMsg, ConnectionSource: row.ConnectionSource,
			ProviderSessionID: row.ProviderSessionID, TrustRegistryEnabled: row.TrustRegistryEnabled,
			TrustRegistryDid: row.TrustRegistryDid,
		}
		items[i] = adminSessionItem{
			ID:                   row.ID,
			UserID:               row.UserID,
			UserUniqueID:         row.UserUniqueID,
			UserEmail:            row.UserEmail,
			VtaName:              row.VtaName,
			VtcName:              row.VtcName,
			Mode:                 row.Mode,
			DomainType:           row.DomainType,
			Status:               row.Status,
			FailedStage:          session.FailureStage(),
			ErrorMsg:             row.ErrorMsg,
			FQDN:                 row.FQDN,
			VtaImage:             row.VtaImage,
			MediatorImage:        row.MediatorImage,
			DidsImage:            row.DidsImage,
			VtcImage:             row.VtcImage,
			TrustRegistryEnabled: row.TrustRegistryEnabled,
			TrustRegistryImage:   row.TrustRegistryImage,
			CreatedAt:            row.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		storedResources := make(map[string]adminSessionStoredResource, len(row.Resources))
		for _, resource := range row.Resources {
			storedResources[resource.Component] = resource
		}
		for _, target := range sessionWorkloadTargets(&session) {
			stored, ok := storedResources[target.Component]
			if !ok {
				continue
			}
			defaults := profiles[target.Component]
			items[i].Resources = append(items[i].Resources, adminSessionResourceSummary{
				Component:     target.Component,
				MemoryRequest: stored.MemoryRequest,
				MemoryLimit:   stored.MemoryLimit,
				Customized: !sameMemory(stored.MemoryRequest, defaults.MemoryRequest) ||
					!sameMemory(stored.MemoryLimit, defaults.MemoryLimit),
			})
		}
		if session.IsFullStack() {
			continue
		}
		items[i].ConnectionSource = row.ConnectionSource
		if row.Provider != nil {
			items[i].Provider = *row.Provider
		} else {
			items[i].ProviderGone = session.IsOrphaned()
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"items":     items,
		"total":     queryResult.Total,
		"page":      page,
		"page_size": adminSessionsPageSize,
		"statuses":  statuses,
	})
}

// AdminSessionLogs — GET /api/v1/admin/setup-sessions/:id/logs (admin only).
//
// The admin-cookie twin of GET /setup/:id/logs, differing only in the lookup:
// vta_name alone, with no user_id filter. It exists because the platform stack
// is owned by a passkey-less system account — nobody can ever hold that user's
// cookie, so without this route the farm's own stack is the one session whose
// provisioning nobody can watch.
func (h *SetupHandler) AdminSessionLogs(c *gin.Context) {
	publicID := c.Param("id")

	var session model.SetupSession
	if err := h.db.Where("vta_name = ?", publicID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	if h.k8s == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "k8s not configured"})
		return
	}
	if !session.IsFullStack() {
		// vta_only's log sources are keyed to statuses the admin panel has no
		// view for; there is no caller for it and no reason to duplicate it.
		c.JSON(http.StatusNotImplemented, gin.H{"error": "admin log streaming covers full_stack sessions only"})
		return
	}

	h.logsFullStack(c, &session)
}

// AdminDeleteSession — DELETE /api/v1/admin/setup-sessions/:id (admin only).
// Tears down any user's session, identified by vta_name alone — the one
// difference from the user-facing DELETE /setup/:id, which is scoped to the
// caller. Destructive and irreversible: the frontend gates it behind a
// type-the-session-id confirmation.
//
// Deleting the platform stack additionally requires {"confirm": "<label>"} in
// the body. That one is not left to the UI: it is the only deletion in the
// product that can degrade other users' service by removing a mediator or DID
// host their agents use, and it sits one mis-click away in an admin table.
func (h *SetupHandler) AdminDeleteSession(c *gin.Context) {
	publicID := c.Param("id")

	var session model.SetupSession
	if err := h.db.Where("vta_name = ?", publicID).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	if session.DomainType == model.DomainPlatform {
		var body struct {
			Confirm string `json:"confirm"`
		}
		// A missing/!JSON body is the mis-click case, so bind errors are not
		// distinguished from a wrong value — both mean "not confirmed".
		_ = c.ShouldBindJSON(&body)
		if body.Confirm != session.VtaName {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "deleting the platform stack disrupts agents using its mediator or DID host — " +
					`send {"confirm": "` + session.VtaName + `"} to proceed`,
			})
			return
		}
	}

	log.Printf("[admin] deleting %s session %d (%s), domain_type=%s, owned by user %d",
		session.Mode, session.ID, session.VtaName, session.DomainType, session.UserID)

	h.teardownSession(c, &session)
}
