package handler

import (
	"testing"

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
