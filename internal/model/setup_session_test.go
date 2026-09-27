package model

import "testing"

func TestFailureStage(t *testing.T) {
	tests := []struct {
		name    string
		session SetupSession
		want    string
	}{
		{
			name:    "persisted stage wins",
			session: SetupSession{Status: "failed", FailedStage: "step_mediator_p2", ErrorMsg: "unrelated"},
			want:    "step_mediator_p2",
		},
		{
			name:    "legacy full stack error",
			session: SetupSession{Status: "failed", Mode: ModeFullStack, ErrorMsg: "dids provision-integration failed: job failed"},
			want:    "step_dids_provision",
		},
		{
			name:    "legacy vta provisioning error",
			session: SetupSession{Status: "failed", Mode: ModeVtaOnly, ErrorMsg: "VTA deployment did not become ready: timeout"},
			want:    "provisioning",
		},
		{
			name:    "legacy vta setup fallback",
			session: SetupSession{Status: "failed", Mode: ModeVtaOnly, ErrorMsg: "job reached backoff limit"},
			want:    "vta_setup_running",
		},
		{
			name:    "legacy vta environment error",
			session: SetupSession{Status: "failed", Mode: ModeVtaOnly, ErrorMsg: "failed to ensure k8s namespace: forbidden"},
			want:    "dns_provisioned",
		},
		{
			name:    "not failed",
			session: SetupSession{Status: "running", FailedStage: "step_vta_setup"},
			want:    "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.session.FailureStage(); got != test.want {
				t.Fatalf("FailureStage() = %q, want %q", got, test.want)
			}
		})
	}
}
