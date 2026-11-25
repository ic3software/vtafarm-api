package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
)

func TestAlertmanagerWebhookLifecycleAndUserIncidentAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.Postgres(t)
	user := newAdminTestUser(t)
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	session := model.SetupSession{
		UserID: user.ID, VtaName: "alert-session", Mode: model.ModeVtaOnly,
		Status: "running", Domain: "example.com", Subdomain: "alert-session",
	}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	h := &SetupHandler{db: db}
	h.ConfigureAlertmanagerWebhook("test-token")
	router := gin.New()
	router.POST("/webhook", h.AlertmanagerWebhook)
	router.GET("/setup/:id/incidents", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, user.ID)
		h.SessionIncidents(c)
	})
	router.GET("/user/incidents", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, user.ID)
		h.UserIncidents(c)
	})
	router.PATCH("/setup/:id/incidents/:incident_id", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, user.ID)
		h.MarkSessionIncidentRead(c)
	})

	started := time.Date(2026, 11, 24, 14, 32, 0, 0, time.UTC)
	payload := func(status string, endsAt time.Time) string {
		body, err := json.Marshal(alertmanagerWebhook{Status: status, Alerts: []alertmanagerAlert{{
			Status: status, StartsAt: started, EndsAt: endsAt, Fingerprint: "fingerprint-1",
			Labels: map[string]string{
				"alertname": "VTAFarmMemoryUsageCritical", "audience": "tenant", "cluster": "dev",
				"label_session_id": fmt.Sprint(session.ID), "label_app": "vta", "resource": "memory", "severity": "critical",
			},
			Annotations: map[string]string{"summary": "Memory usage is above 90%", "description": "Memory pressure", "usage_percent": "93.5"},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		router.ServeHTTP(w, req)
		return w
	}
	if w := request(http.MethodPost, "/webhook", payload("firing", time.Time{}), ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized webhook = %d: %s", w.Code, w.Body.String())
	}
	for range 2 {
		if w := request(http.MethodPost, "/webhook", payload("firing", time.Time{}), "test-token"); w.Code != http.StatusAccepted {
			t.Fatalf("firing webhook = %d: %s", w.Code, w.Body.String())
		}
	}
	var incidents []model.Incident
	if err := db.Find(&incidents).Error; err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 || incidents[0].Status != model.IncidentFiring || incidents[0].UsagePercent == nil || *incidents[0].UsagePercent != 93.5 {
		t.Fatalf("firing incidents = %+v", incidents)
	}
	resolvedAt := started.Add(3 * time.Minute)
	if w := request(http.MethodPost, "/webhook", payload("resolved", resolvedAt), "test-token"); w.Code != http.StatusAccepted {
		t.Fatalf("resolved webhook = %d: %s", w.Code, w.Body.String())
	}
	w := request(http.MethodGet, "/setup/alert-session/incidents", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list incidents = %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Incidents []incidentResponse `json:"incidents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Incidents) != 1 || response.Incidents[0].Status != model.IncidentResolved ||
		response.Incidents[0].DowntimeSeconds == nil || *response.Incidents[0].DowntimeSeconds != 180 {
		t.Fatalf("resolved incidents = %+v", response.Incidents)
	}
	w = request(http.MethodGet, "/setup/alert-session/incidents?status=resolved&resource=memory&range=all&page=1&limit=1", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("filtered incidents = %d: %s", w.Code, w.Body.String())
	}
	var filtered incidentListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Incidents) != 1 || filtered.Total != 1 || filtered.Page != 1 || filtered.PageSize != 1 || filtered.TotalPages != 1 {
		t.Fatalf("filtered incident page = %+v", filtered)
	}
	for _, path := range []string{
		"/setup/alert-session/incidents?resource=network",
		"/setup/alert-session/incidents?range=2d",
		"/setup/alert-session/incidents?page=0",
	} {
		if w := request(http.MethodGet, path, "", ""); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid filter %q = %d: %s", path, w.Code, w.Body.String())
		}
	}
	if w := request(http.MethodGet, "/user/incidents", "", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"session_id":"alert-session"`) {
		t.Fatalf("user incidents = %d: %s", w.Code, w.Body.String())
	}
	incidentID := response.Incidents[0].PublicID
	if w := request(http.MethodPatch, "/setup/alert-session/incidents/"+incidentID, `{}`, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("missing read field = %d: %s", w.Code, w.Body.String())
	}
	w = request(http.MethodPatch, "/setup/alert-session/incidents/"+incidentID, `{"read":true}`, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"read_at"`) {
		t.Fatalf("mark read = %d: %s", w.Code, w.Body.String())
	}
}
