package trusttask

import (
	"context"
	"testing"
	"time"

	"github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/trusttasks"

	"github.com/ic3software/vtafarm-api/internal/testutil"
)

func TestReplayGuardLifecycle(t *testing.T) {
	db := testutil.Postgres(t)
	guard := NewReplayGuard(db)
	ctx := context.Background()
	now := time.Now().UTC()
	retainUntil := now.Add(time.Hour)

	verdict, err := guard.Claim(ctx, "doc-1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", retainUntil, now)
	if err != nil || verdict.Kind != trusttasks.VerdictFresh {
		t.Fatalf("first claim = %+v, %v", verdict, err)
	}
	verdict, err = guard.Claim(ctx, "doc-1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", retainUntil, now.Add(time.Second))
	if err != nil || verdict.Kind != trusttasks.VerdictDuplicate || !verdict.InFlight {
		t.Fatalf("in-flight duplicate = %+v, %v", verdict, err)
	}
	verdict, err = guard.Claim(ctx, "doc-1", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", retainUntil, now.Add(time.Second))
	if err != nil || verdict.Kind != trusttasks.VerdictConflict {
		t.Fatalf("conflict = %+v, %v", verdict, err)
	}

	response := []byte(`{"type":"example#response"}`)
	if err := guard.RecordResponse(ctx, "doc-1", response); err != nil {
		t.Fatal(err)
	}
	verdict, err = guard.Claim(ctx, "doc-1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", retainUntil, now.Add(2*time.Second))
	if err != nil || verdict.Kind != trusttasks.VerdictDuplicate || verdict.InFlight || string(verdict.PriorResponse) != string(response) {
		t.Fatalf("completed duplicate = %+v, %v", verdict, err)
	}
}

func TestReplayGuardReclaimsExpiredLeaseAndRecord(t *testing.T) {
	db := testutil.Postgres(t)
	guard := NewReplayGuard(db)
	guard.executionLease = time.Second
	ctx := context.Background()
	now := time.Now().UTC()
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	if verdict, err := guard.Claim(ctx, "doc-lease", digest, now.Add(time.Hour), now); err != nil || verdict.Kind != trusttasks.VerdictFresh {
		t.Fatalf("first claim = %+v, %v", verdict, err)
	}
	if verdict, err := guard.Claim(ctx, "doc-lease", digest, now.Add(time.Hour), now.Add(2*time.Second)); err != nil || verdict.Kind != trusttasks.VerdictFresh {
		t.Fatalf("lease reclaim = %+v, %v", verdict, err)
	}
	if verdict, err := guard.Claim(ctx, "doc-expired", digest, now.Add(time.Second), now); err != nil || verdict.Kind != trusttasks.VerdictFresh {
		t.Fatalf("expiring claim = %+v, %v", verdict, err)
	}
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if verdict, err := guard.Claim(ctx, "doc-expired", other, now.Add(time.Hour), now.Add(2*time.Second)); err != nil || verdict.Kind != trusttasks.VerdictFresh {
		t.Fatalf("record reclaim = %+v, %v", verdict, err)
	}
}

func TestReplayGuardReleaseAllowsRetry(t *testing.T) {
	db := testutil.Postgres(t)
	guard := NewReplayGuard(db)
	ctx := context.Background()
	now := time.Now().UTC()
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	if verdict, err := guard.Claim(ctx, "doc-release", digest, now.Add(time.Hour), now); err != nil || verdict.Kind != trusttasks.VerdictFresh {
		t.Fatalf("first claim = %+v, %v", verdict, err)
	}
	if err := guard.Release(ctx, "doc-release", digest); err != nil {
		t.Fatal(err)
	}
	if verdict, err := guard.Claim(ctx, "doc-release", digest, now.Add(time.Hour), now.Add(time.Second)); err != nil || verdict.Kind != trusttasks.VerdictFresh {
		t.Fatalf("claim after release = %+v, %v", verdict, err)
	}
}
