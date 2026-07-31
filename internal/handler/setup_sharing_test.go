package handler

import (
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/setup"
)

func sharedStack() *model.SetupSession {
	code := "K7M29XQP4B8W3NR"
	return &model.SetupSession{
		Mode:          model.ModeFullStack,
		Status:        "running",
		VtaName:       "alice",
		Domain:        "firstperson.dev",
		DidsSubdomain: "dids-alice",
		MediatorDid:   "did:webvh:mediator-alice",
		DIDHostingDid: "did:webvh:dids-alice",
		ShareCode:     &code,
	}
}

func TestBuildConnectionBundle(t *testing.T) {
	got := buildConnectionBundle(sharedStack(), "firstperson.dev")
	if got == nil {
		t.Fatal("a shared, running stack must produce a bundle")
	}

	want := connectionBundle{
		Version:             connectionBundleVersion,
		Kind:                connectionBundleKind,
		Farm:                "firstperson.dev",
		Stack:               "alice",
		Code:                "K7M2-9XQP-4B8W-3NR",
		MediatorDid:         "did:webvh:mediator-alice",
		DidHostingServerURL: "https://dids-alice.firstperson.dev",
		DidHostingDid:       "did:webvh:dids-alice",
	}
	if *got != want {
		t.Errorf("bundle = %+v\nwant     %+v", *got, want)
	}
}

// The code is stored normalised and displayed grouped. A recipient must be able
// to paste the displayed form straight back and have it match.
func TestBundleCodeRoundTripsToStored(t *testing.T) {
	s := sharedStack()
	bundle := buildConnectionBundle(s, "firstperson.dev")

	if !setup.ShareCodeMatches(bundle.Code, *s.ShareCode) {
		t.Errorf("displayed code %q does not match stored %q", bundle.Code, *s.ShareCode)
	}
}

// Absent, not empty: a bundle that would be refused on arrival must not be
// offered at all, or the UI shows a green "copy this" for something broken.
func TestBuildConnectionBundleRefusesUnshareableStacks(t *testing.T) {
	empty := ""
	tests := []struct {
		name   string
		mutate func(*model.SetupSession)
	}{
		{"not shared", func(s *model.SetupSession) { s.ShareCode = nil }},
		{"share code cleared to empty", func(s *model.SetupSession) { s.ShareCode = &empty }},
		{"not running", func(s *model.SetupSession) { s.Status = "step_dids_p1" }},
		{"no mediator DID yet", func(s *model.SetupSession) { s.MediatorDid = "" }},
		{"no daemon DID yet", func(s *model.SetupSession) { s.DIDHostingDid = "" }},
		{"vta_only cannot provide", func(s *model.SetupSession) { s.Mode = model.ModeVtaOnly }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := sharedStack()
			tc.mutate(s)
			if got := buildConnectionBundle(s, "firstperson.dev"); got != nil {
				t.Errorf("expected no bundle, got %+v", *got)
			}
		})
	}
}

// IsShared is the readiness bar the bundle builder and the create-time lookup
// both lean on, so its edges are worth pinning directly.
func TestIsShared(t *testing.T) {
	if !sharedStack().IsShared() {
		t.Error("a running, shared full_stack must report as shared")
	}

	s := sharedStack()
	s.ShareCode = nil
	if s.IsShared() {
		t.Error("a stack with no share code must not report as shared")
	}
}

// A vta_only session deploys neither a mediator nor a DID host, so it can never
// be a provider however its columns are set.
func TestVtaOnlyIsNeverShared(t *testing.T) {
	s := sharedStack()
	s.Mode = model.ModeVtaOnly
	if s.IsShared() {
		t.Error("a vta_only session must never report as shared")
	}
}

func TestIsOrphaned(t *testing.T) {
	providerID := uint(7)
	tests := []struct {
		name   string
		source string
		pid    *uint
		want   bool
	}{
		{"connected in farm", model.ConnectionInFarm, &providerID, false},
		{"provider deleted", model.ConnectionInFarm, nil, true},
		// A platform session never had a provider row, so a nil link there says
		// nothing was ever deleted.
		{"platform default", model.ConnectionPlatform, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &model.SetupSession{ConnectionSource: tc.source, ProviderSessionID: tc.pid}
			if got := s.IsOrphaned(); got != tc.want {
				t.Errorf("IsOrphaned() = %v, want %v", got, tc.want)
			}
		})
	}
}
