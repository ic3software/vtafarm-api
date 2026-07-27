package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/model"
)

// cooldownLeft is pure — it reads the row and the clock and nothing else — so
// it is tested directly rather than through a handler with a database behind
// it.
func TestCooldownLeft(t *testing.T) {
	ago := func(d time.Duration) *time.Time { t := time.Now().Add(-d); return &t }
	verified := time.Now().Add(-24 * time.Hour)

	cases := []struct {
		name   string
		domain model.Domain
		want   bool // want a wait
	}{
		{
			// Attach performs no lookups, so the first check is always free.
			name:   "never checked",
			domain: model.Domain{},
			want:   false,
		},
		{
			name:   "checked just now",
			domain: model.Domain{LastCheckedAt: ago(time.Second)},
			want:   true,
		},
		{
			name:   "checked most of a cooldown ago",
			domain: model.Domain{LastCheckedAt: ago(VerifyCooldown - 5*time.Second)},
			want:   true,
		},
		{
			name:   "cooldown elapsed",
			domain: model.Domain{LastCheckedAt: ago(VerifyCooldown + time.Second)},
			want:   false,
		},
		{
			// A verified domain's check short-circuits before any lookup, so
			// throttling it would refuse a request that costs nothing.
			name:   "verified inside the window",
			domain: model.Domain{LastCheckedAt: ago(time.Second), VerifiedAt: &verified},
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cooldownLeft(&tc.domain)
			if (got > 0) != tc.want {
				t.Fatalf("cooldownLeft = %v, want wait = %v", got, tc.want)
			}
			if got > VerifyCooldown {
				t.Fatalf("cooldownLeft = %v, longer than the cooldown itself", got)
			}
		})
	}
}

// The 429 has to tell the caller when to come back — in the header for anything
// generic, in the body for the portal's countdown.
func TestThrottledResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	checked := time.Now().Add(-10 * time.Second)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/domains/1/verify", nil)

	h := &DomainHandler{}
	if !h.throttled(c, &model.Domain{LastCheckedAt: &checked}) {
		t.Fatal("throttled = false for a domain checked 10s ago")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}

	var body struct {
		Error      string `json:"error"`
		RetryAfter int    `json:"retry_after"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	// Rounded up, so never the 0 that would invite an immediate retry.
	if body.RetryAfter < 1 || body.RetryAfter > 50 {
		t.Errorf("retry_after = %d, want 1..50", body.RetryAfter)
	}
	if body.Error == "" {
		t.Error("error message is empty")
	}
	if got := w.Header().Get("Retry-After"); got != strconv.Itoa(body.RetryAfter) {
		t.Errorf("Retry-After header = %q, want %d", got, body.RetryAfter)
	}
}

// A domain nobody has checked must not be refused — that would strand a freshly
// attached domain behind a cooldown it never earned.
func TestThrottledAllowsFirstCheck(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/domains/1/verify", nil)

	h := &DomainHandler{}
	if h.throttled(c, &model.Domain{}) {
		t.Fatal("throttled = true for a domain that has never been checked")
	}
	if w.Code != http.StatusOK { // untouched recorder
		t.Fatalf("wrote status %d, want nothing written", w.Code)
	}
}
