package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
	"gorm.io/gorm"
)

func TestDeleteCancelsEnablingRegistry(t *testing.T) {
	for _, initialStatus := range []string{"running", "deleting"} {
		for _, admin := range []bool{false, true} {
			name := initialStatus + "/owner"
			if admin {
				name = initialStatus + "/admin"
			}
			t.Run(name, func(t *testing.T) {
				db := testutil.Postgres(t)
				session := testutil.SetupSession(t, db, model.ModeFullStack)
				if err := db.Model(&session).Updates(map[string]any{
					"status":                 initialStatus,
					"trust_registry_enabled": true,
					"trust_registry_state":   "enabling",
					"trust_registry_phase":   "grant_access",
					"trust_registry_image":   "registry:test",
					"trust_registry_did":     "did:webvh:registry.example",
				}).Error; err != nil {
					t.Fatal(err)
				}

				h := &SetupHandler{db: db}
				r := gin.New()
				path := "/setup/" + session.VtaName
				if admin {
					r.DELETE("/setup/:id", h.AdminDeleteSession)
				} else {
					r.Use(func(c *gin.Context) { c.Set(middleware.ContextUserID, session.UserID) })
					r.DELETE("/setup/:id", h.Delete)
				}
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, path, nil))
				if w.Code != http.StatusNoContent {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if err := db.First(&model.SetupSession{}, session.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
					t.Fatalf("session still exists after deletion: %v", err)
				}
			})
		}
	}
}

func TestTeardownMaintenanceRequiresDeletingState(t *testing.T) {
	db := testutil.Postgres(t)
	session := testutil.SetupSession(t, db, model.ModeFullStack)
	h := &SetupHandler{db: db}

	if release, err := h.acquireTeardownMaintenance(session.ID); err == nil {
		release()
		t.Fatal("teardown maintenance accepted a session that was not deleting")
	}
	if err := db.Model(&session).Updates(map[string]any{
		"status":                 "deleting",
		"trust_registry_enabled": true,
		"trust_registry_state":   "enabling",
	}).Error; err != nil {
		t.Fatal(err)
	}
	release, err := h.acquireTeardownMaintenance(session.ID)
	if err != nil {
		t.Fatalf("teardown maintenance rejected cancelled Registry work: %v", err)
	}
	release()
}
