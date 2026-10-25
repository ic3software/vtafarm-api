package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ic3software/vtafarm-api/internal/k8s"
)

func TestMonitorTokenRequired(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name        string
		configured  string
		authorize   string
		queryToken  string
		wantStatus  int
		wantReached bool
	}{
		{name: "disabled", configured: "", authorize: "Bearer monitor-secret", wantStatus: http.StatusNotFound},
		{name: "valid bearer", configured: "monitor-secret", authorize: "Bearer monitor-secret", wantStatus: http.StatusNoContent, wantReached: true},
		{name: "case insensitive scheme", configured: "monitor-secret", authorize: "bearer monitor-secret", wantStatus: http.StatusNoContent, wantReached: true},
		{name: "missing header", configured: "monitor-secret", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", configured: "monitor-secret", authorize: "Bearer wrong-secret", wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", configured: "monitor-secret", authorize: "Basic monitor-secret", wantStatus: http.StatusUnauthorized},
		{name: "malformed bearer", configured: "monitor-secret", authorize: "Bearer monitor-secret extra", wantStatus: http.StatusUnauthorized},
		{name: "legacy query token accepted during cutover", configured: "monitor-secret", queryToken: "monitor-secret", wantStatus: http.StatusNoContent, wantReached: true},
		{name: "invalid bearer does not fall back to query", configured: "monitor-secret", authorize: "Bearer wrong-secret", queryToken: "monitor-secret", wantStatus: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewMonitorHandler(nil, nil, MonitorConfig{Token: tt.configured})
			reached := false
			router := gin.New()
			router.GET("/monitor", h.TokenRequired, func(c *gin.Context) {
				reached = true
				c.Status(http.StatusNoContent)
			})

			req := httptest.NewRequest(http.MethodGet, "/monitor", nil)
			if tt.authorize != "" {
				req.Header.Set("Authorization", tt.authorize)
			}
			if tt.queryToken != "" {
				query := req.URL.Query()
				query.Set("token", tt.queryToken)
				req.URL.RawQuery = query.Encode()
			}
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)

			if resp.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", resp.Code, tt.wantStatus, resp.Body.String())
			}
			if reached != tt.wantReached {
				t.Fatalf("downstream reached = %v, want %v", reached, tt.wantReached)
			}
		})
	}
}

func TestCapacityIssues(t *testing.T) {
	healthy := func() *k8s.ClusterStats {
		return &k8s.ClusterStats{
			MetricsAvailable: true,
			StorageAvailable: true,
			Nodes: []k8s.NodeResourceStat{{
				Name:                 "node-1",
				CPUAllocatableMillis: 4000, CPUUsedMillis: 1000,
				MemAllocatableBytes: 8 << 30, MemUsedBytes: 2 << 30,
			}},
			StorageNodes: []k8s.StorageNodeStat{{
				Name:         "node-1",
				MaximumBytes: 100 << 30, AvailableBytes: 60 << 30,
			}},
		}
	}

	t.Run("all under thresholds", func(t *testing.T) {
		if got := capacityIssues(healthy(), 90, 90, 85); len(got) != 0 {
			t.Fatalf("expected no issues, got %+v", got)
		}
	})

	t.Run("memory over threshold", func(t *testing.T) {
		stats := healthy()
		stats.Nodes[0].MemUsedBytes = 7500 << 20 // ~91.5% of 8Gi
		got := capacityIssues(stats, 90, 90, 85)
		if len(got) != 1 || got[0].Resource != "memory" || got[0].Node != "node-1" {
			t.Fatalf("expected one memory issue, got %+v", got)
		}
		if got[0].Percent < 90 || got[0].Threshold != 90 {
			t.Fatalf("unexpected percent/threshold: %+v", got[0])
		}
	})

	t.Run("storage over threshold", func(t *testing.T) {
		stats := healthy()
		stats.StorageNodes[0].AvailableBytes = 10 << 30 // 90% used
		got := capacityIssues(stats, 90, 90, 85)
		if len(got) != 1 || got[0].Resource != "storage" {
			t.Fatalf("expected one storage issue, got %+v", got)
		}
	})

	t.Run("unreadable sources are issues themselves", func(t *testing.T) {
		stats := healthy()
		stats.MetricsAvailable = false
		stats.StorageAvailable = false
		got := capacityIssues(stats, 90, 90, 85)
		if len(got) != 2 {
			t.Fatalf("expected metrics + storage_stats issues, got %+v", got)
		}
		if got[0].Resource != "metrics" || got[1].Resource != "storage_stats" {
			t.Fatalf("unexpected resources: %+v", got)
		}
	})

	t.Run("zero allocatable never divides", func(t *testing.T) {
		stats := healthy()
		stats.Nodes[0].CPUAllocatableMillis = 0
		stats.StorageNodes[0].MaximumBytes = 0
		if got := capacityIssues(stats, 90, 90, 85); len(got) != 0 {
			t.Fatalf("expected no issues, got %+v", got)
		}
	})
}
