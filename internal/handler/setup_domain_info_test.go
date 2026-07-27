package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// DomainInfo reads nothing but config, so the handler is constructed directly
// rather than through NewSetupHandler — no DB, Cloudflare or k8s client needed.
func domainInfoResponse(t *testing.T, h *SetupHandler) map[string]string {
	t.Helper()
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/setup/domain-info", nil)
	h.DomainInfo(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return got
}

func TestDomainInfo(t *testing.T) {
	tests := []struct {
		name string
		h    SetupHandler
		want map[string]string
	}{{
		name: "production",
		h: SetupHandler{
			appEnv:        "production",
			clusterDomain: "firstperson.dev",
			ingressIP:     "157.180.68.139",
		},
		want: map[string]string{
			"managed_domain": "firstperson.dev",
			"env_prefix":     "",
			"target_ip":      "157.180.68.139",
			"target_host":    "lb.firstperson.dev",
		},
	}, {
		// The whole point of the endpoint: a locally run API must report its
		// own prefixed shape, not production's.
		name: "development prefixes labels and its own CNAME target",
		h: SetupHandler{
			appEnv:        "development",
			clusterDomain: "firstperson.dev",
			ingressIP:     "157.180.68.139",
		},
		want: map[string]string{
			"managed_domain": "firstperson.dev",
			"env_prefix":     "dev-",
			"target_ip":      "157.180.68.139",
			"target_host":    "dev-lb.firstperson.dev",
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := domainInfoResponse(t, &tc.h)
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("%s = %q, want %q", k, got[k], want)
				}
			}
		})
	}
}
