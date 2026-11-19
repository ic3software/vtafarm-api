package handler

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ic3software/vtafarm-api/internal/model"
)

// A wrong path only shows up as an empty archive against a live cluster.
func TestExportComponentPaths(t *testing.T) {
	vtaOnly := exportComponents(&model.SetupSession{Mode: model.ModeVtaOnly})
	if len(vtaOnly) != 1 {
		t.Fatalf("vta_only exports %d components, want 1", len(vtaOnly))
	}
	if got, want := vtaOnly[0].selector, "app=vta,session-id=0"; got != want {
		t.Errorf("vta_only selector = %q, want %q", got, want)
	}
	if got, want := vtaOnly[0].configPath, "/work/vta/config.toml"; got != want {
		t.Errorf("vta_only config path = %q, want %q", got, want)
	}

	want := map[string]struct{ selector, configPath string }{
		"vta":      {"app=fs-vta,session-id=7", "/work/vta/config.toml"},
		"mediator": {"app=fs-mediator,session-id=7", "/work/mediator/conf/mediator.toml"},
		"dids":     {"app=fs-dids,session-id=7", "/work/dids/config.toml"},
		"vtc":      {"app=fs-vtc,session-id=7", "/work/vtc/config.toml"},
	}

	full := exportComponents(&model.SetupSession{Mode: model.ModeFullStack, ID: 7})
	if len(full) != len(want) {
		t.Fatalf("full_stack exports %d components, want %d", len(full), len(want))
	}
	for _, comp := range full {
		w, ok := want[comp.name]
		if !ok {
			t.Errorf("unexpected component %q", comp.name)
			continue
		}
		if comp.selector != w.selector {
			t.Errorf("%s selector = %q, want %q", comp.name, comp.selector, w.selector)
		}
		if comp.configPath != w.configPath {
			t.Errorf("%s config path = %q, want %q", comp.name, comp.configPath, w.configPath)
		}
	}
}

func TestLogExportWindowFollowsOwnerEntitlement(t *testing.T) {
	free := logExportEntitlement(model.User{}, false)
	if free.Plan != "free" || free.RetentionDays != 7 {
		t.Fatalf("free export = %#v", free)
	}

	premium := logExportEntitlement(model.User{FullstackAccess: true}, false)
	if premium.Plan != "premium" || premium.RetentionDays != 30 {
		t.Fatalf("premium export = %#v", premium)
	}

	admin := logExportEntitlement(model.User{}, true)
	if admin.Plan != "premium" || admin.RetentionDays != 30 {
		t.Fatalf("admin export = %#v", admin)
	}
}

func TestLogExportWindowDoesNotPredateSession(t *testing.T) {
	end := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	created := end.Add(-2 * time.Hour)
	start := logExportStart(created, end, 7)
	if !start.Equal(created) {
		t.Fatalf("start = %s, want session creation %s", start, created)
	}
}

func TestRequestedLogRange(t *testing.T) {
	tests := []struct {
		preset  string
		maxDays int
		want    int
		wantErr error
	}{
		{"", 7, 7, nil},
		{"1d", 7, 1, nil},
		{"7d", 7, 7, nil},
		{"30d", 30, 30, nil},
		{"30d", 7, 0, errLogRangeLimit},
		{"2d", 30, 0, errors.New("invalid")},
	}
	for _, test := range tests {
		got, err := requestedLogRange(test.preset, test.maxDays)
		if got != test.want || (test.wantErr == nil) != (err == nil) {
			t.Errorf("requestedLogRange(%q, %d) = %d, %v", test.preset, test.maxDays, got, err)
		}
		if test.wantErr == errLogRangeLimit && !errors.Is(err, errLogRangeLimit) {
			t.Errorf("requestedLogRange(%q, %d) error = %v, want retention limit", test.preset, test.maxDays, err)
		}
	}
}

func TestSessionPodPatternIsBoundedToSession(t *testing.T) {
	if got := sessionPodPattern(&model.SetupSession{ID: 7, Mode: model.ModeFullStack}); got != `^fs-7-.+$` {
		t.Fatalf("full stack pattern = %q", got)
	}
	if got := sessionPodPattern(&model.SetupSession{ID: 7, Mode: model.ModeVtaOnly}); got != `^(vta-7-|vta-setup-7-|vta-provision-7-|vta-acl-7-).+$` {
		t.Fatalf("VTA-only pattern = %q", got)
	}
}

func TestRetainedLogArchiveUsesReadableLogAndHidesPlan(t *testing.T) {
	dir := t.TempDir()
	logsPath := filepath.Join(dir, "logs.log")
	if err := os.WriteFile(logsPath, []byte("one log line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dir, "logs.zip")
	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := logExportManifest{RetentionDays: 30, RangeDays: 7, Entries: 1}
	if err := writeRetainedLogArchive(archive, logsPath, manifest); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if len(zr.File) != 2 || zr.File[0].Name != "all-logs.log" || zr.File[1].Name != "manifest.json" {
		t.Fatalf("archive members = %#v", zr.File)
	}
	manifestFile, err := zr.File[1].Open()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(manifestFile)
	manifestFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["plan"]; ok {
		t.Fatal("manifest must not expose the account plan")
	}
	if fields["retention_days"] != float64(30) || fields["range_days"] != float64(7) {
		t.Fatalf("manifest fields = %#v", fields)
	}
}
