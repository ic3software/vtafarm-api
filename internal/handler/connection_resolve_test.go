package handler

import (
	"net/http"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/setup"
)

// validBundle is a well-formed bundle for the farm the handler below serves.
// Its share code is real, so the check-symbol test passes and the tier-1
// refusals under test are the ones actually being exercised.
func validBundle(t *testing.T) *connectionBundle {
	t.Helper()
	code, err := setup.NewShareCode()
	if err != nil {
		t.Fatalf("NewShareCode: %v", err)
	}
	return &connectionBundle{
		Version:             connectionBundleVersion,
		Kind:                connectionBundleKind,
		Farm:                "firstperson.dev",
		Stack:               "alice",
		Code:                code,
		MediatorDid:         "did:webvh:mediator-alice",
		DidHostingServerURL: "https://dids-alice.firstperson.dev",
		DidHostingDid:       "did:webvh:dids-alice",
	}
}

// Every case here is refused before the database is touched, which is why a
// handler with a nil db is a valid fixture — and is itself worth asserting:
// malformed input must not reach a query.
func TestResolveBundleProviderRejectsBeforeAnyQuery(t *testing.T) {
	h := &SetupHandler{clusterDomain: "firstperson.dev"}

	tests := []struct {
		name       string
		mutate     func(*connectionBundle)
		wantReason string
	}{{
		name:       "not a connection bundle",
		mutate:     func(b *connectionBundle) { b.Kind = "something.else" },
		wantReason: reasonBadBundle,
	}, {
		name:       "from a future version",
		mutate:     func(b *connectionBundle) { b.Version = 99 },
		wantReason: reasonBadBundle,
	}, {
		name:       "no stack named",
		mutate:     func(b *connectionBundle) { b.Stack = "" },
		wantReason: reasonBadBundle,
	}, {
		name:       "no code",
		mutate:     func(b *connectionBundle) { b.Code = "" },
		wantReason: reasonBadBundle,
	}, {
		// The check symbol earns its keep here: a single mistyped character is
		// diagnosed as a typo instead of falling through to the deliberately
		// vague invalid_bundle.
		name:       "mistyped code",
		mutate:     func(b *connectionBundle) { b.Code = mistype(b.Code) },
		wantReason: reasonBadBundle,
	}, {
		name:       "another farm's bundle",
		mutate:     func(b *connectionBundle) { b.Farm = "someone-else.example" },
		wantReason: reasonWrongFarm,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := validBundle(t)
			tc.mutate(b)

			infra, provider, reason, detail := h.resolveBundleProvider(b)
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if detail == "" {
				t.Error("a refusal must carry a sentence for the user")
			}
			if provider != nil || infra != (sharedInfra{}) {
				t.Error("a refusal must not return a provider or partial values")
			}
		})
	}
}

// The farm name is compared case-insensitively — hostnames are, and a bundle
// that travelled through a mail client that title-cased it is still ours.
func TestResolveBundleProviderFarmMatchIsCaseInsensitive(t *testing.T) {
	h := &SetupHandler{clusterDomain: "firstperson.dev"}
	b := validBundle(t)
	b.Farm = "FirstPerson.DEV"

	// Reaches the database lookup, so it must not have been refused as
	// wrong_farm first. A nil db panics there, which is what we assert against.
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected the lookup to be reached (nil db panics); it was refused earlier instead")
		}
	}()
	_, _, reason, _ := h.resolveBundleProvider(b)
	t.Fatalf("expected to reach the database, got reason %q", reason)
}

// The five situations behind invalid_bundle must not be distinguishable, or
// this becomes a way to discover which stacks exist and which are shared. The
// status has to be uniform too — a different code per case leaks just as much.
func TestConnectionRefusalStatus(t *testing.T) {
	tests := []struct {
		reason string
		want   int
	}{
		{reasonBadBundle, http.StatusBadRequest},
		{reasonInvalidBundle, http.StatusForbidden},
		{reasonStackNotRunning, http.StatusConflict},
		{reasonStackAtConnLimit, http.StatusConflict},
		{reasonWrongFarm, http.StatusUnprocessableEntity},
		{reasonStackChanged, http.StatusUnprocessableEntity},
	}
	for _, tc := range tests {
		if got := connectionRefusalStatus(tc.reason); got != tc.want {
			t.Errorf("connectionRefusalStatus(%q) = %d, want %d", tc.reason, got, tc.want)
		}
	}
}

// mistype changes one data character of a share code to a different valid
// symbol, leaving the check symbol stale.
func mistype(code string) string {
	n := []byte(setup.NormalizeShareCode(code))
	if n[0] == 'A' {
		n[0] = 'B'
	} else {
		n[0] = 'A'
	}
	return string(n)
}
