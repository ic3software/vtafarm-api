package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRateLimitReturnsMachineReadableReason(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/limited", RateLimit(1, time.Hour), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	request := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/limited", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		router.ServeHTTP(response, req)
		return response
	}
	if response := request(); response.Code != http.StatusNoContent {
		t.Fatalf("first response = %d", response.Code)
	}
	response := request()
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("limited response = %d", response.Code)
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Reason != "rate_limited" {
		t.Fatalf("reason = %q", body.Reason)
	}
}
