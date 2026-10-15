package handler

import "testing"

func TestRecoveryForStage(t *testing.T) {
	tests := []struct {
		stage, component, column string
	}{
		{"deploy_dids", "dids", "dids_image"},
		{"deploy_mediator", "mediator", "mediator_image"},
		{"deploy_vta", "vta", "vta_image"},
		{"deploy_vtc", "vtc", "vtc_image"},
	}

	for _, test := range tests {
		t.Run(test.stage, func(t *testing.T) {
			got, ok := recoveryForStage(test.stage)
			if !ok {
				t.Fatalf("recoveryForStage(%q) was not recoverable", test.stage)
			}
			if got.Stage != test.stage || got.Component != test.component || got.ImageColumn != test.column {
				t.Fatalf("recoveryForStage(%q) = %#v", test.stage, got)
			}
		})
	}
}

func TestRecoveryForStageRejectsNonDeploymentStages(t *testing.T) {
	for _, stage := range []string{"", "failed", "step_vtc_setup", "running"} {
		if _, ok := recoveryForStage(stage); ok {
			t.Errorf("recoveryForStage(%q) unexpectedly recoverable", stage)
		}
	}
}
