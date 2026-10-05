package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/cloudflare"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
)

func accessTestSession(userID uint, mode, status string) model.SetupSession {
	return model.SetupSession{UserID: userID, VtaName: "access-" + uuid.NewString(), Mode: mode, Status: status, Domain: "example.com", Subdomain: "test"}
}

func TestSessionAccessLimitCountsAllStatusesAndDeletionReleasesSlot(t *testing.T) {
	db := testutil.Postgres(t)
	user := model.User{UniqueId: "limited"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if user.FullstackAccess {
		t.Fatal("new users must default to no Fullstack Access")
	}
	h := &SetupHandler{db: db}
	first := accessTestSession(user.ID, model.ModeVtaOnly, "vta_setup_running")
	second := accessTestSession(user.ID, model.ModeVtaOnly, "failed")
	for _, session := range []*model.SetupSession{&first, &second} {
		if err := h.persistUserSession(context.Background(), session); err != nil {
			t.Fatal(err)
		}
	}
	third := accessTestSession(user.ID, model.ModeVtaOnly, "running")
	if err := h.persistUserSession(context.Background(), &third); !errors.Is(err, errVTALimitReached) {
		t.Fatalf("third VTA: got %v, want limit reached", err)
	}
	other := model.User{UniqueId: "other"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	otherSession := accessTestSession(other.ID, model.ModeVtaOnly, "running")
	if err := h.persistUserSession(context.Background(), &otherSession); err != nil {
		t.Fatalf("another account's quota must be independent: %v", err)
	}
	if err := db.Delete(&second).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.persistUserSession(context.Background(), &third); err != nil {
		t.Fatalf("deletion did not release a slot: %v", err)
	}
}

func TestSessionAccessSerializesConcurrentCreates(t *testing.T) {
	db := testutil.Postgres(t)
	user := model.User{UniqueId: "concurrent"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	first := accessTestSession(user.ID, model.ModeVtaOnly, "running")
	if err := db.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 8)
	for range 8 {
		go func() {
			<-start
			// Separate handler instances must share the database lock.
			h := &SetupHandler{db: db}
			session := accessTestSession(user.ID, model.ModeVtaOnly, "dns_provisioned")
			results <- h.persistUserSession(ctx, &session)
		}()
	}
	close(start)
	succeeded := 0
	for range 8 {
		if err := <-results; err == nil {
			succeeded++
		} else if !errors.Is(err, errVTALimitReached) {
			t.Fatalf("unexpected concurrent creation error: %v", err)
		}
	}
	var count int64
	if err := db.Model(&model.SetupSession{}).Where("user_id = ?", user.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || count != 2 {
		t.Fatalf("successful creates=%d, total=%d; want 1, 2", succeeded, count)
	}
}

func TestFullstackAccessGrantRevocationAndProfile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.Postgres(t)
	user := model.User{UniqueId: "grants"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	uh := NewUserHandler(db)
	sh := &SetupHandler{db: db, cf: cloudflare.New("test-only", "test-zone"), ingressIP: "192.0.2.1", clusterDomain: "example.com"}
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(middleware.ContextUserID, user.ID) })
	router.GET("/me", uh.Me)
	router.GET("/users", uh.List)
	router.PUT("/users/:id/fullstack-access", uh.SetFullstackAccess)
	router.POST("/setup", sh.Create)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, r)
		return w
	}
	assertProfile := func(access bool, count int, limit any) {
		t.Helper()
		w := call(http.MethodGet, "/me", "")
		var profile map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &profile); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || profile["fullstack_access"] != access || profile["vta_count"] != float64(count) || profile["vta_limit"] != limit {
			t.Fatalf("unexpected profile: %d %s", w.Code, w.Body)
		}
	}
	assertProfile(false, 0, float64(2))
	if w := call(http.MethodPut, "/users/grants/fullstack-access", `{}`); w.Code != 400 {
		t.Fatalf("missing grant value accepted: %d %s", w.Code, w.Body)
	}
	if w := call(http.MethodPut, "/users/grants/fullstack-access", `{"fullstack_access":true}`); w.Code != 200 {
		t.Fatalf("grant: %d %s", w.Code, w.Body)
	}
	for _, mode := range []string{model.ModeFullStack, model.ModeVtaOnly, model.ModeFullStack} {
		session := accessTestSession(user.ID, mode, "running")
		if err := sh.persistUserSession(context.Background(), &session); err != nil {
			t.Fatalf("unlimited account's %s creation: %v", mode, err)
		}
	}
	assertProfile(true, 3, nil)
	if w := call(http.MethodGet, "/users", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"fullstack_access":true`) || strings.Contains(w.Body.String(), "beta_access") {
		t.Fatalf("admin list: %d %s", w.Code, w.Body)
	}
	if w := call(http.MethodPut, "/users/grants/fullstack-access", `{"fullstack_access":false}`); w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	assertProfile(false, 3, float64(2))
	for _, tc := range []struct{ mode, reason string }{
		{model.ModeVtaOnly, "vta_limit_reached"},
		{model.ModeFullStack, "fullstack_access_required"},
	} {
		session := accessTestSession(user.ID, tc.mode, "running")
		if err := sh.persistUserSession(context.Background(), &session); err == nil {
			t.Fatal("stale grant allowed creation after revocation")
		}
		w := call(http.MethodPost, "/setup", `{"mode":"`+tc.mode+`","vta_image":"test:v1","vta_name":"blocked"}`)
		var response map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if w.Code != 403 || response["reason"] != tc.reason {
			t.Fatalf("%s refusal: %d %s", tc.mode, w.Code, w.Body)
		}
		if tc.mode == model.ModeVtaOnly && response["error"] != errVTALimitReached.Error() {
			t.Fatalf("limit message changed: %s", w.Body)
		}
	}
	assertProfile(false, 3, float64(2))
}

func TestFullstackAccessMigrationPreservesExistingGrants(t *testing.T) {
	db := testutil.Postgres(t)
	for _, granted := range []bool{false, true} {
		user := model.User{UniqueId: strings.ReplaceAll(uuid.NewString(), "-", "")[:12], FullstackAccess: granted}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, direction := range []string{"down", "up"} {
		sql, err := os.ReadFile("../../migrations/000040_fullstack_access." + direction + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(string(sql)).Error; err != nil {
			t.Fatal(err)
		}
	}
	var users []model.User
	if err := db.Order("id").Find(&users).Error; err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].FullstackAccess || !users[1].FullstackAccess {
		t.Fatalf("migration did not preserve existing flags: %+v", users)
	}
}
