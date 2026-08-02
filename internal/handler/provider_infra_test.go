package handler

import (
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
)

// runningProvider is a full_stack row that has finished provisioning — the
// state a vta_only session may be wired to.
func runningProvider() *model.SetupSession {
	return &model.SetupSession{
		Mode:          model.ModeFullStack,
		Status:        "running",
		Domain:        "firstperson.dev",
		DidsSubdomain: "dids-alice",
		MediatorDid:   "did:webvh:mediator-alice",
		DIDHostingDid: "did:webvh:dids-alice",
	}
}

func TestProviderInfraFromRunningStack(t *testing.T) {
	got, reason, detail := providerInfra(runningProvider())

	if reason != "" {
		t.Fatalf("reason = %q (%s), want usable", reason, detail)
	}
	want := sharedInfra{
		MediatorDid: "did:webvh:mediator-alice",
		ServerURL:   "https://dids-alice.firstperson.dev",
		ControlURL:  "https://dids-alice.firstperson.dev",
		DaemonDid:   "did:webvh:dids-alice",
	}
	if got != want {
		t.Errorf("providerInfra() = %+v, want %+v", got, want)
	}
}

func TestProviderInfraRefusesUnusableStacks(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*model.SetupSession)
		wantReason string
	}{{
		name:       "still provisioning",
		mutate:     func(s *model.SetupSession) { s.Status = "step_vta_setup" },
		wantReason: reasonPlatformNotReady,
	}, {
		name:       "failed",
		mutate:     func(s *model.SetupSession) { s.Status = "failed" },
		wantReason: reasonPlatformNotReady,
	}, {
		// Running, but step 1b never landed. A session created here would carry
		// an empty mediator DID and never deliver a message.
		name:       "running without a mediator DID",
		mutate:     func(s *model.SetupSession) { s.MediatorDid = "" },
		wantReason: reasonSharedUnconfigured,
	}, {
		name:       "running without a dids hostname",
		mutate:     func(s *model.SetupSession) { s.DidsSubdomain = ""; s.Domain = "" },
		wantReason: reasonSharedUnconfigured,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := runningProvider()
			tc.mutate(s)

			got, reason, detail := providerInfra(s)
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if detail == "" {
				t.Error("a refusal must carry a sentence for the user")
			}
			if got != (sharedInfra{}) {
				t.Errorf("refused, but returned %+v — callers must not be handed partial values", got)
			}
		})
	}
}

// A daemon DID is what arms didhosting's audience check, but it is deliberately
// not part of the readiness bar: a platform stack provisioned before the column
// carried this meaning would otherwise stop serving vta_only creation.
func TestProviderInfraAcceptsMissingDaemonDid(t *testing.T) {
	s := runningProvider()
	s.DIDHostingDid = ""

	got, reason, detail := providerInfra(s)
	if reason != "" {
		t.Fatalf("reason = %q (%s), want usable", reason, detail)
	}
	if got.DaemonDid != "" {
		t.Errorf("DaemonDid = %q, want empty — no expectation on record", got.DaemonDid)
	}
	if got.MediatorDid == "" || got.ServerURL == "" {
		t.Error("the rest of the values must still come through")
	}
}
