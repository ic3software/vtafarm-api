package handler

import (
	"strings"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
)

func TestRecoveryForStage(t *testing.T) {
	tests := []struct {
		mode, stage string
	}{
		{model.ModeFullStack, "deploy_dids"},
		{model.ModeFullStack, "deploy_mediator"},
		{model.ModeFullStack, "deploy_vta"},
		{model.ModeFullStack, "deploy_vtc"},
		{model.ModeVtaOnly, "deploy_vta"},
	}

	for _, test := range tests {
		t.Run(test.mode+"/"+test.stage, func(t *testing.T) {
			got, ok := recoveryForStage(test.mode, test.stage)
			if !ok {
				t.Fatalf("recoveryForStage(%q, %q) was not recoverable", test.mode, test.stage)
			}
			if got.Stage != test.stage {
				t.Fatalf("recoveryForStage(%q, %q) = %#v", test.mode, test.stage, got)
			}
		})
	}
}

func TestRecoveryForStageRejectsNonDeploymentStages(t *testing.T) {
	for _, stage := range []string{"", "failed", "step_vtc_setup", "running"} {
		if _, ok := recoveryForStage(model.ModeFullStack, stage); ok {
			t.Errorf("recoveryForStage(%q) unexpectedly recoverable", stage)
		}
	}
	if _, ok := recoveryForStage(model.ModeVtaOnly, "deploy_vtc"); ok {
		t.Error("vta_only deploy_vtc unexpectedly recoverable")
	}
}

func TestValidateRecoveryComponents(t *testing.T) {
	fullStack := []componentImage{
		{Component: "vta", Image: "vta-image"},
		{Component: "mediator", Image: "mediator-image"},
		{Component: "dids", Image: "dids-image"},
		{Component: "vtc", Image: "vtc-image"},
	}
	if err := validateRecoveryComponents(model.ModeFullStack, fullStack); err != nil {
		t.Fatalf("full_stack components rejected: %v", err)
	}
	if err := validateRecoveryComponents(model.ModeVtaOnly, []componentImage{{Component: "vta", Image: "vta-image"}}); err != nil {
		t.Fatalf("vta_only VTA component rejected: %v", err)
	}

	tests := []struct {
		name       string
		mode       string
		components []componentImage
		want       string
	}{
		{"full stack missing VTC", model.ModeFullStack, fullStack[:3], "missing vtc"},
		{"vta only extra VTC", model.ModeVtaOnly, []componentImage{{Component: "vta"}, {Component: "vtc"}}, "unsupported vtc"},
		{"duplicate", model.ModeFullStack, append(fullStack, componentImage{Component: "vta"}), "duplicate component vta"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateRecoveryComponents(test.mode, test.components)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateRecoveryComponents() error = %v, want %q", err, test.want)
			}
		})
	}
}
