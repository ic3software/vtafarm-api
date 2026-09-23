package handler

import (
	"strings"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
)

func TestValidateTOMLConfig(t *testing.T) {
	targets := stackConfigTargets(&model.SetupSession{ID: 7, Mode: model.ModeFullStack})
	for _, name := range []string{"vta", "mediator", "dids", "vtc"} {
		if got := validateTOMLConfig(stackConfigRequest{Component: name, Content: "[server]\nport = 8100\n"}, targets); len(got) != 0 {
			t.Errorf("valid %s config returned errors: %v", name, got)
		}
	}

	cases := []struct {
		req stackConfigRequest
		key string
	}{
		{stackConfigRequest{Component: "vta", Content: "[server\n"}, "vta"},
		{stackConfigRequest{Component: "mediator", Content: ""}, "mediator"},
		{stackConfigRequest{Component: "extra", Content: "ok = true"}, "component"},
		{stackConfigRequest{Content: "ok = true"}, "component"},
	}
	for _, test := range cases {
		if got := validateTOMLConfig(test.req, targets); got[test.key] == "" {
			t.Errorf("missing %q validation error for %+v: %v", test.key, test.req, got)
		}
	}
}

func TestValidateTOMLConfigRejectsOversize(t *testing.T) {
	targets := stackConfigTargets(&model.SetupSession{ID: 7, Mode: model.ModeFullStack})
	req := stackConfigRequest{Component: "vta", Content: "value = \"" + strings.Repeat("x", maxConfigBytes) + "\""}
	if got := validateTOMLConfig(req, targets)["vta"]; !strings.Contains(got, "exceeds") {
		t.Fatalf("oversize error = %q, want size limit", got)
	}
}

func TestStackConfigTargets(t *testing.T) {
	targets := stackConfigTargets(&model.SetupSession{ID: 7, Mode: model.ModeFullStack})
	if len(targets) != 4 {
		t.Fatalf("targets = %d, want 4", len(targets))
	}
	want := map[string]string{
		"vta": "/work/vta/config.toml", "mediator": "/work/mediator/conf/mediator.toml",
		"dids": "/work/dids/config.toml", "vtc": "/work/vtc/config.toml",
	}
	for _, target := range targets {
		if target.configPath != want[target.name] {
			t.Errorf("%s path = %q, want %q", target.name, target.configPath, want[target.name])
		}
	}
}

func TestVtaOnlyConfigTargetsAndValidation(t *testing.T) {
	targets := stackConfigTargets(&model.SetupSession{ID: 7, Mode: model.ModeVtaOnly})
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	target := targets[0]
	if target.name != "vta" || target.deployment != "vta-7" || target.pvc != "vta-data-7" || target.selector != "app=vta,session-id=7" || target.configPath != "/work/vta/config.toml" {
		t.Fatalf("unexpected VTA-only target: %+v", target)
	}
	if got := validateTOMLConfig(stackConfigRequest{Component: "vta", Content: "[server]\nport = 8100\n"}, targets); len(got) != 0 {
		t.Fatalf("valid VTA-only config returned errors: %v", got)
	}
	if got := validateTOMLConfig(stackConfigRequest{Component: "mediator", Content: "ok = true"}, targets); got["component"] == "" {
		t.Fatalf("shared component was accepted: %v", got)
	}
	if got := validateTOMLConfig(stackConfigRequest{Component: "vta", Content: "[server\n"}, targets); got["vta"] == "" {
		t.Fatalf("invalid VTA-only TOML was accepted: %v", got)
	}
}

func TestStackConfigTargetForSelectsOnlyRequestedComponent(t *testing.T) {
	session := &model.SetupSession{ID: 7, Mode: model.ModeFullStack}
	target, ok := stackConfigTargetFor(session, "vtc")
	if !ok || target.name != "vtc" || target.deployment != "fs-7-vtc" {
		t.Fatalf("VTC target = %+v, %v", target, ok)
	}
	if _, ok := stackConfigTargetFor(session, "unknown"); ok {
		t.Fatal("unknown component was accepted")
	}
}
