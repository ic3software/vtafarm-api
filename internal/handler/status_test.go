package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type fakePublicStatusChecks struct {
	farmComponents     farmComponentStatus
	farmErr            error
	platformComponents platformComponentStatus
	platformErr        error
}

func (f *fakePublicStatusChecks) Farm(context.Context) (farmComponentStatus, error) {
	return f.farmComponents, f.farmErr
}
func (f *fakePublicStatusChecks) Platform(context.Context) (platformComponentStatus, error) {
	return f.platformComponents, f.platformErr
}

func TestStatusHandlerResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixed := time.Date(2026, 10, 24, 23, 0, 0, 0, time.FixedZone("test", -7*60*60))

	tests := []struct {
		name           string
		path           string
		configure      func(*fakePublicStatusChecks)
		wantCode       int
		wantStatus     string
		wantComponents map[string]string
	}{
		{
			name: "farm operational", path: "/farm", wantCode: http.StatusOK, wantStatus: "operational",
			configure: func(f *fakePublicStatusChecks) {
				f.farmComponents = farmComponentStatus{Database: "operational", Kubernetes: "operational", Vault: "operational"}
			},
			wantComponents: map[string]string{"database": "operational", "kubernetes": "operational", "vault": "operational"},
		},
		{
			name: "farm exposes sanitized component failures", path: "/farm", wantCode: http.StatusServiceUnavailable, wantStatus: "unavailable",
			configure: func(f *fakePublicStatusChecks) {
				f.farmComponents = farmComponentStatus{Database: "unavailable", Kubernetes: "operational", Vault: "unavailable"}
				f.farmErr = errors.New("private failure detail")
			},
			wantComponents: map[string]string{"database": "unavailable", "kubernetes": "operational", "vault": "unavailable"},
		},
		{
			name: "platform operational", path: "/platform", wantCode: http.StatusOK, wantStatus: "operational",
			configure: func(f *fakePublicStatusChecks) {
				f.platformComponents = platformComponentStatus{VTA: "operational", Mediator: "operational", DIDs: "operational", VTC: "operational"}
			},
			wantComponents: map[string]string{"vta": "operational", "mediator": "operational", "dids": "operational", "vtc": "operational"},
		},
		{
			name: "platform exposes sanitized component failures", path: "/platform", wantCode: http.StatusServiceUnavailable, wantStatus: "unavailable",
			configure: func(f *fakePublicStatusChecks) {
				f.platformComponents = platformComponentStatus{VTA: "unavailable", Mediator: "unavailable", DIDs: "operational", VTC: "operational"}
				f.platformErr = errors.New("private failure detail")
			},
			wantComponents: map[string]string{"vta": "unavailable", "mediator": "unavailable", "dids": "operational", "vtc": "operational"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := &fakePublicStatusChecks{}
			tt.configure(checks)
			h := &StatusHandler{checks: checks, timeout: time.Second, now: func() time.Time { return fixed }}
			router := gin.New()
			router.GET("/farm", h.Farm)
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
			wantFields := 2
			if tt.wantComponents != nil {
				wantFields = 3
				components, ok := body["components"].(map[string]any)
				if !ok {
					t.Fatalf("components = %#v", body["components"])
				}
				for component, want := range tt.wantComponents {
					if components[component] != want {
						t.Errorf("components[%q] = %#v, want %q", component, components[component], want)
					}
				}
			}
			if len(body) != wantFields {
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

func TestRunStatusChecksCollectsConcurrentFailures(t *testing.T) {
	var arrived atomic.Int32
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if arrived.Add(1) == 4 {
			close(ready)
		}
		select {
		case <-ready:
		case <-r.Context().Done():
			return
		}
		if r.URL.Path == "/vta" || r.URL.Path == "/mediator" {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	checks := &livePublicStatusChecks{http: server.Client()}
	components, err := runStatusChecks(t.Context(), []statusComponentCheck{
		{component: "vta", check: func(ctx context.Context) error { return checks.probe(ctx, server.URL+"/vta") }},
		{component: "mediator", check: func(ctx context.Context) error { return checks.probe(ctx, server.URL+"/mediator") }},
		{component: "dids", check: func(ctx context.Context) error { return checks.probe(ctx, server.URL+"/dids") }},
		{component: "vtc", check: func(ctx context.Context) error { return checks.probe(ctx, server.URL+"/vtc") }},
	})
	if err == nil || !strings.Contains(err.Error(), "vta:") || !strings.Contains(err.Error(), "mediator:") {
		t.Fatalf("error = %v, want both VTA and mediator failures", err)
	}
	want := map[string]string{"vta": "unavailable", "mediator": "unavailable", "dids": "operational", "vtc": "operational"}
	for component, wantStatus := range want {
		if components[component] != wantStatus {
			t.Errorf("components[%q] = %q, want %q", component, components[component], wantStatus)
		}
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
