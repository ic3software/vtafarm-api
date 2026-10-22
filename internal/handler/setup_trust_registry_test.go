package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
)

func TestRegistryDefaultsAndOwnerBoundary(t *testing.T) {
	db := testutil.Postgres(t)
	session := testutil.SetupSession(t, db, model.ModeFullStack)
	if session.TrustRegistryEnabled || session.TrustRegistryState != "disabled" {
		t.Fatal("existing/default stacks must remain opted out")
	}
	h := &SetupHandler{db: db}
	for _, tt := range []struct {
		name string
		user uint
		want int
	}{
		{"owner", session.UserID, http.StatusOK}, {"another account", session.UserID + 1, http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := gin.New()
			r.Use(func(c *gin.Context) { c.Set(middleware.ContextUserID, tt.user) })
			r.GET("/setup/:id/trust-registry", h.GetTrustRegistry)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/setup/"+session.VtaName+"/trust-registry", nil))
			if w.Code != tt.want {
				t.Fatalf("status=%d want %d", w.Code, tt.want)
			}
		})
	}
	if err := db.Model(&session).Updates(map[string]any{"trust_registry_enabled": true, "trust_registry_state": "enabling"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := h.acquireSessionMaintenance(session.ID); err == nil {
		t.Fatal("maintenance raced with registry enablement")
	}
	if err := db.Model(&session).Update("trust_registry_state", "failed").Error; err != nil {
		t.Fatal(err)
	}
	var stored model.SetupSession
	if err := db.First(&stored, session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != session.Status {
		t.Fatal("registry failure changed the parent setup state")
	}
}

func TestVtaOnlyRegistryEnablementIsRejectedWithoutInfrastructure(t *testing.T) {
	h := &SetupHandler{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"image":"registry:1-test"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.enableTrustRegistry(c, &model.SetupSession{Mode: model.ModeVtaOnly, Status: "running"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestRegistryReturnsPersistedJobError(t *testing.T) {
	db := testutil.Postgres(t)
	session := testutil.SetupSession(t, db, model.ModeFullStack)
	const failure = "identity job failed (Job has reached the specified backoff limit)\n\n--- Job Logs ---\nError: an unknown failure\nCaused by:\n    storage unavailable"
	if err := db.Model(&session).Updates(map[string]any{
		"trust_registry_enabled": true,
		"trust_registry_state":   "failed",
		"trust_registry_phase":   "provision_identity",
		"trust_registry_error":   failure,
	}).Error; err != nil {
		t.Fatal(err)
	}
	h := &SetupHandler{db: db}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(middleware.ContextUserID, session.UserID) })
	r.GET("/setup/:id/trust-registry", h.GetTrustRegistry)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/setup/"+session.VtaName+"/trust-registry", nil))
	var response struct {
		State string `json:"state"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || response.State != "failed" || response.Error != failure {
		t.Fatalf("stored Job error was not returned intact: %s", w.Body.String())
	}
}
