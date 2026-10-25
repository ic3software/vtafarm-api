package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type fakePublicStatusChecks struct {
	farmErr     error
	vtaErr      error
	platformErr error
}

func (f *fakePublicStatusChecks) Farm(context.Context) error     { return f.farmErr }
func (f *fakePublicStatusChecks) VTA(context.Context) error      { return f.vtaErr }
func (f *fakePublicStatusChecks) Platform(context.Context) error { return f.platformErr }

func TestStatusHandlerResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixed := time.Date(2026, 10, 24, 23, 0, 0, 0, time.FixedZone("test", -7*60*60))

	tests := []struct {
		name       string
		path       string
		configure  func(*fakePublicStatusChecks)
		wantCode   int
		wantStatus string
	}{
		{name: "farm operational", path: "/farm", configure: func(*fakePublicStatusChecks) {}, wantCode: http.StatusOK, wantStatus: "operational"},
		{name: "VTA unavailable", path: "/vta", configure: func(f *fakePublicStatusChecks) { f.vtaErr = errors.New("private failure detail") }, wantCode: http.StatusServiceUnavailable, wantStatus: "unavailable"},
		{name: "platform unavailable", path: "/platform", configure: func(f *fakePublicStatusChecks) { f.platformErr = errors.New("private failure detail") }, wantCode: http.StatusServiceUnavailable, wantStatus: "unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := &fakePublicStatusChecks{}
			tt.configure(checks)
			h := &StatusHandler{checks: checks, timeout: time.Second, now: func() time.Time { return fixed }}
			router := gin.New()
			router.GET("/farm", h.Farm)
			router.GET("/vta", h.VTA)
			router.GET("/platform", h.Platform)

			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if resp.Code != tt.wantCode {
				t.Fatalf("status code = %d, want %d; body = %s", resp.Code, tt.wantCode, resp.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["status"] != tt.wantStatus || body["checked_at"] != "2026-10-25T06:00:00Z" {
				t.Fatalf("body = %#v", body)
			}
			if len(body) != 2 {
				t.Fatalf("public response leaked extra fields: %#v", body)
			}
			if resp.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q", resp.Header().Get("Cache-Control"))
			}
			if strings.Contains(resp.Body.String(), "private failure detail") {
				t.Fatal("public response leaked the internal error")
			}
		})
	}
}

func TestStatusProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			http.Error(w, "down", http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	checks := &livePublicStatusChecks{http: &http.Client{
		Timeout: time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
	if err := checks.probe(t.Context(), server.URL+"/ok"); err != nil {
		t.Fatalf("healthy probe failed: %v", err)
	}
	if err := checks.probe(t.Context(), server.URL+"/down"); err == nil {
		t.Fatal("503 probe succeeded")
	}
	if err := checks.probe(t.Context(), server.URL+"/redirect"); err == nil {
		t.Fatal("redirect probe succeeded")
	}
}

func TestValidateStatusProbeURL(t *testing.T) {
	tests := []struct {
		name   string
		url    string
		domain string
		valid  bool
	}{
		{name: "subdomain", url: "https://vta.firstperson.dev/health", domain: "firstperson.dev", valid: true},
		{name: "explicit HTTPS port", url: "https://vta.firstperson.dev:443/health", domain: "firstperson.dev", valid: true},
		{name: "wrong scheme", url: "http://vta.firstperson.dev/health", domain: "firstperson.dev"},
		{name: "lookalike domain", url: "https://vta.firstperson.dev.example.com/health", domain: "firstperson.dev"},
		{name: "credentials", url: "https://user@example.com/health", domain: "example.com"},
		{name: "query", url: "https://vta.firstperson.dev/health?target=other", domain: "firstperson.dev"},
		{name: "nonstandard port", url: "https://vta.firstperson.dev:8443/health", domain: "firstperson.dev"},
		{name: "missing cluster domain", url: "https://vta.firstperson.dev/health", domain: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStatusProbeURL(tt.url, tt.domain)
			if (err == nil) != tt.valid {
				t.Fatalf("error = %v, valid = %v", err, tt.valid)
			}
		})
	}
}
