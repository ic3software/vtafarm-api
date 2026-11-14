package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
)

func TestGetSessionUpgradeWithoutHistoryReturnsNoContent(t *testing.T) {
	db := testutil.Postgres(t)
	session := testutil.SetupSession(t, db, model.ModeVtaOnly)
	h := &UpgradeHandler{db: db}

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(middleware.ContextUserID, session.UserID) })
	r.GET("/setup/:id/upgrade", h.GetForSession)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/setup/"+session.VtaName+"/upgrade", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d want %d; body=%s", w.Code, http.StatusNoContent, w.Body.String())
	}
}
