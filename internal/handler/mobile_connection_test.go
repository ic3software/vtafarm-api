package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/setup"
	"github.com/ic3software/vtafarm-api/internal/testutil"
)

func TestMobileTokensAreScoped(t *testing.T) {
	h := NewMobileConnectionHandler(nil, nil, config.MobileConnectionConfig{SigningKey: strings.Repeat("test-only-", 4)}, "example.com")
	id := uuid.NewString()
	token := h.token("callback", id)
	if got, ok := h.tokenID("callback", token); !ok || got != id {
		t.Fatal("valid token rejected")
	}
	for _, bad := range []string{token + "x", uuid.NewString() + token[36:], h.token("progress", id), "invalid"} {
		if _, ok := h.tokenID("callback", bad); ok {
			t.Fatal("invalid/scoped token accepted")
		}
	}
	h.key = []byte(strings.Repeat("different-test-key", 3))
	if _, ok := h.tokenID("callback", token); ok {
		t.Fatal("token accepted under different signing key")
	}
}

func TestMobileConfigFailsClosed(t *testing.T) {
	for _, domain := range []string{"", "example.com/path", "user@example.com", "example.com?token=x"} {
		h := NewMobileConnectionHandler(nil, nil, config.MobileConnectionConfig{SigningKey: strings.Repeat("x", 32)}, domain)
		if h.origin != "" || h.enabled {
			t.Errorf("accepted cluster domain %q", domain)
		}
	}
}

func TestMobileConfigEnablesWithoutFeatureFlag(t *testing.T) {
	h := NewMobileConnectionHandler(nil, &setup.Orchestrator{}, config.MobileConnectionConfig{
		SigningKey: strings.Repeat("x", 32),
	}, "example.com")
	if !h.enabled || h.origin != "https://vtafarm-api.example.com" {
		t.Fatal("valid mobile connection configuration was not enabled")
	}

	h = NewMobileConnectionHandler(nil, &setup.Orchestrator{}, config.MobileConnectionConfig{}, "example.com")
	if h.enabled {
		t.Fatal("mobile connection enabled without a signing key")
	}
}

func TestMobileHTTPFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.Postgres(t)
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	other := testutil.SetupSession(t, db, model.ModeVtaOnly)
	h := NewMobileConnectionHandler(db, nil, config.MobileConnectionConfig{SigningKey: strings.Repeat("test-only-", 4)}, "example.com")
	h.enabled = true
	h.kick = func(uint) {}
	router := gin.New()
	router.Use(middleware.NoStore(), middleware.MobilePrivacy())
	router.POST("/callback/:token", h.Callback)
	router.GET("/progress/:request_id", h.Progress)
	router.POST("/progress/:request_id/complete", h.Progress)
	owner := router.Group("/owner", func(c *gin.Context) { c.Set(middleware.ContextUserID, s.UserID) })
	owner.GET("/:id/current", h.Current)
	owner.POST("/:id", h.Change)
	owner.POST("/:id/:request_id/refresh", h.Change)
	owner.DELETE("/:id/:request_id", h.Change)
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := call("POST", "/owner/"+other.VtaName, "", ""); w.Code != 404 {
		t.Fatalf("cross-owner creation=%d", w.Code)
	}
	if w := call("GET", "/owner/"+other.VtaName+"/current", "", ""); w.Code != 404 {
		t.Fatalf("cross-owner read=%d", w.Code)
	}
	w := call("POST", "/owner/"+s.VtaName, "", "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	r, err := connection.Current(db, s.ID)
	if err != nil || r == nil {
		t.Fatal("no request", err)
	}
	token := h.token("callback", r.ID)
	did, err := didkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if w := call("POST", "/callback/"+token, `{"admin_did":"`+did+`","vta_did":"did:web:other"}`, ""); w.Code != 400 {
		t.Fatal("target override accepted", w.Code)
	}
	if w := call("POST", "/callback/"+token, `{"admin_did":"`+did+`"} {}`, ""); w.Code != 400 {
		t.Fatal("trailing JSON accepted")
	}
	if w := call("POST", "/callback/"+token, `{"admin_did":"`+strings.Repeat("a", 5000)+`"}`, ""); w.Code != 400 {
		t.Fatal("oversized body accepted")
	}
	body := `{"admin_did":"` + did + `"}`
	w = call("POST", "/callback/"+token, body, "")
	if w.Code != 202 {
		t.Fatalf("accept=%d %s", w.Code, w.Body)
	}
	var result struct {
		ProgressToken     string    `json:"progress_token"`
		ProgressExpiresAt time.Time `json:"progress_expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ProgressToken == "" || result.ProgressToken == token {
		t.Fatal("missing or reused progress credential")
	}
	if w := call("GET", "/progress/"+r.ID, "", token); w.Code != 401 {
		t.Fatal("callback token used as progress credential")
	}
	if w := call("GET", "/progress/"+uuid.NewString(), "", result.ProgressToken); w.Code != 401 {
		t.Fatal("cross-request credential accepted")
	}
	if w := call("POST", "/progress/"+r.ID+"/complete", `{"status":"connected"}`, result.ProgressToken); w.Code != 409 {
		t.Fatal("premature completion accepted")
	}
	w = call("POST", "/callback/"+token, body, "")
	var retry struct {
		ProgressToken     string    `json:"progress_token"`
		ProgressExpiresAt time.Time `json:"progress_expires_at"`
	}
	json.Unmarshal(w.Body.Bytes(), &retry)
	if retry.ProgressToken != result.ProgressToken || !retry.ProgressExpiresAt.Equal(result.ProgressExpiresAt) {
		t.Fatal("retry renewed credential")
	}
	if w := call("GET", "/owner/"+s.VtaName+"/current", "", ""); bytes.Contains(w.Body.Bytes(), []byte("callback_url")) || bytes.Contains(w.Body.Bytes(), []byte("progress_token")) {
		t.Fatal("accepted request exposed credentials in owner view")
	}
	db.Model(&s).Update("status", "running")
	w = call("POST", "/progress/"+r.ID+"/complete", `{"status":"connected"}`, result.ProgressToken)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"status":"connected"`)) {
		t.Fatal("completion failed", w.Code, w.Body)
	}
	// A disabled rollout still exposes existing state, but creates no new QR.
	h.enabled = false
	if w := call("POST", "/owner/"+s.VtaName, "", ""); w.Code != 503 {
		t.Fatal("disabled creation accepted")
	}
}

func TestMobilePanicDoesNotExposeCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.MobilePrivacy())
	r.GET("/:token", func(c *gin.Context) { panic("secret-in-panic") })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/secret-in-url", nil))
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("unsafe panic response: %s", w.Body)
	}
}
