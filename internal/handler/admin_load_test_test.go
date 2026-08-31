package handler

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/model"
)

func TestLoadTestRunViewCountsSessionStates(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	run := model.LoadTestRun{
		ID: 7, Status: model.LoadTestActive, RequestedCount: 3, CreatedCount: 3,
		VtaImage: "example/vta:test", CreatedAt: now, UpdatedAt: now,
	}
	runID := run.ID
	sessions := []model.SetupSession{
		{ID: 1, VtaName: "load-7-001", Status: "running", Subdomain: "vta-load-7-001", Domain: "example.com", LoadTestRunID: &runID},
		{ID: 2, VtaName: "load-7-002", Status: "provisioning", Subdomain: "vta-load-7-002", Domain: "example.com", LoadTestRunID: &runID},
		{ID: 3, VtaName: "load-7-003", Status: "failed", ErrorMsg: "boom", Subdomain: "vta-load-7-003", Domain: "example.com", LoadTestRunID: &runID},
	}

	got := loadTestRunView(&run, sessions)
	if got.RunningCount != 1 || got.FailedCount != 1 {
		t.Fatalf("counts = running %d failed %d, want 1 and 1", got.RunningCount, got.FailedCount)
	}
	if len(got.Sessions) != 3 || got.Sessions[2].ErrorMsg != "boom" {
		t.Fatalf("unexpected sessions: %#v", got.Sessions)
	}
	if got.Sessions[0].FQDN != "vta-load-7-001.example.com" {
		t.Fatalf("fqdn = %q", got.Sessions[0].FQDN)
	}
}

func TestLoadTestID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("valid", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Params = gin.Params{{Key: "id", Value: "42"}}
		id, ok := loadTestID(c)
		if !ok || id != 42 {
			t.Fatalf("loadTestID = %d, %v", id, ok)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "nope"}}
		if _, ok := loadTestID(c); ok {
			t.Fatal("expected invalid id")
		}
		if w.Code != 400 {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})
}
