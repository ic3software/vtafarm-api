package setup

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	valid := []string{"devtest1", "a", "a1", "my-vta", "vtc", "a-b-c", strings.Repeat("x", 48)}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",
		"-devtest1",
		"devtest1-",
		"my--vta",
		"Dev",
		"dev_test",
		"dev.test",
		"dev test",
		strings.Repeat("x", 49),
	}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", name)
		}
	}
}

func TestDidPaths(t *testing.T) {
	if got := VtaDidPath("devtest1"); got != "devtest1-vta" {
		t.Errorf("VtaDidPath = %q, want devtest1-vta", got)
	}
	if got := MediatorDidPath("devtest1"); got != "devtest1-mediator" {
		t.Errorf("MediatorDidPath = %q, want devtest1-mediator", got)
	}
	if got := VtcDidPath("mycommunity"); got != "mycommunity-vtc" {
		t.Errorf("VtcDidPath = %q, want mycommunity-vtc", got)
	}
}

// setup_sessions_did_path_unique indexes vta_name, not the rendered path, and
// only one path per row. That is sound only because every path on the shared
// daemon ends in a suffix its neighbours cannot produce: a vta_only session's
// path (indexed) can never equal the platform stack's -mediator or -vtc paths
// (not indexed), whatever name either side picks. Lose the suffix on vta_only
// and a session named "<platform label>-mediator" slips straight past the
// index and collides on the daemon.
func TestDidPathSuffixesCannotCollide(t *testing.T) {
	// Names chosen to collide if the suffixes did not disambiguate: each is the
	// other's name with a component suffix already baked in.
	names := []string{"alice", "alice-vta", "alice-mediator", "alice-vtc", "firstperson"}

	seen := map[string]string{}
	for _, name := range names {
		for _, p := range []struct{ kind, path string }{
			{"vta", VtaDidPath(name)},
			{"mediator", MediatorDidPath(name)},
			{"vtc", VtcDidPath(name)},
		} {
			// Distinct (name, kind) pairs must render distinct paths — that is
			// what lets one index over names stand in for all three.
			if prev, dup := seen[p.path]; dup {
				t.Errorf("path %q produced by both %s and %s/%s", p.path, prev, name, p.kind)
			}
			seen[p.path] = name + "/" + p.kind
		}
	}
}

func TestEnvPrefix(t *testing.T) {
	if got := EnvPrefix("development"); got != "dev-" {
		t.Errorf("EnvPrefix(development) = %q, want dev-", got)
	}
	// Only the literal "development" marks records as local; anything else —
	// production, staging, an unset APP_ENV — creates unprefixed records.
	for _, env := range []string{"production", "staging", ""} {
		if got := EnvPrefix(env); got != "" {
			t.Errorf("EnvPrefix(%q) = %q, want empty", env, got)
		}
	}
}

func TestHosts(t *testing.T) {
	if got := VtaHost("production", "devtest1"); got != "vta-devtest1" {
		t.Errorf("VtaHost(production) = %q, want vta-devtest1", got)
	}
	if got := VtaHost("development", "devtest1"); got != "dev-vta-devtest1" {
		t.Errorf("VtaHost(development) = %q, want dev-vta-devtest1", got)
	}

	vta, med, dids, vtc := FullStackHosts("production", "devtest1", "mycommunity")
	if vta != "vta-devtest1" || med != "mediator-devtest1" || dids != "dids-devtest1" || vtc != "vtc-mycommunity" {
		t.Errorf("FullStackHosts = %q, %q, %q, %q", vta, med, dids, vtc)
	}

	vta, med, dids, vtc = FullStackHosts("development", "devtest1", "mycommunity")
	if vta != "dev-vta-devtest1" || med != "dev-mediator-devtest1" || dids != "dev-dids-devtest1" {
		t.Errorf("FullStackHosts(development) = %q, %q, %q", vta, med, dids)
	}
	// The VTC host follows vtc_name, not vta_name, so a session whose two
	// names differ must not derive the VTC host from the VTA's.
	if vtc != "dev-vtc-mycommunity" {
		t.Errorf("FullStackHosts(development) vtc = %q, want dev-vtc-mycommunity", vtc)
	}

	// The longest prefix + a max-length name must still fit a DNS label.
	_, med, _, _ = FullStackHosts("development", strings.Repeat("x", maxNameLength), "vtc")
	if len(med) > 63 {
		t.Errorf("mediator host %q exceeds the 63-char DNS label limit", med)
	}
}

// The fixed-label form custom and platform domains use: no user-chosen name in
// the hostname at all, so the label is just the component (plus the dev
// marker). One domain therefore backs at most one session.
func TestFixedLabelHosts(t *testing.T) {
	for _, tc := range []struct {
		env, component, want string
	}{
		{"production", "vta", "vta"},
		{"production", "mediator", "mediator"},
		{"production", "dids", "dids"},
		{"production", "vtc", "vtc"},
		{"development", "vta", "dev-vta"},
		{"development", "mediator", "dev-mediator"},
		{"development", "dids", "dev-dids"},
		{"development", "vtc", "dev-vtc"},
	} {
		if got := componentHost(tc.env, tc.component, ""); got != tc.want {
			t.Errorf("componentHost(%q, %q, \"\") = %q, want %q", tc.env, tc.component, got, tc.want)
		}
	}
}

func TestFixedHosts(t *testing.T) {
	vta, med, dids, vtc := FixedHosts("production")
	if vta != "vta" || med != "mediator" || dids != "dids" || vtc != "vtc" {
		t.Errorf("FixedHosts(production) = %q, %q, %q, %q", vta, med, dids, vtc)
	}

	vta, med, dids, vtc = FixedHosts("development")
	if vta != "dev-vta" || med != "dev-mediator" || dids != "dev-dids" || vtc != "dev-vtc" {
		t.Errorf("FixedHosts(development) = %q, %q, %q, %q", vta, med, dids, vtc)
	}

	// No user-chosen name reaches these labels — that is what makes a domain
	// back exactly one session, so the two environments must be the only
	// thing that can vary.
	if a, _, _, _ := FixedHosts("production"); a != "vta" {
		t.Errorf("FixedHosts is not deterministic: %q", a)
	}
}

func TestCNAMETarget(t *testing.T) {
	if got := CNAMETarget("production", "firstperson.dev"); got != "lb.firstperson.dev" {
		t.Errorf("CNAMETarget(production) = %q, want lb.firstperson.dev", got)
	}
	// Development and production must resolve to different targets, so the
	// same customer domain can be attached in both at once without collision.
	if got := CNAMETarget("development", "firstperson.dev"); got != "dev-lb.firstperson.dev" {
		t.Errorf("CNAMETarget(development) = %q, want dev-lb.firstperson.dev", got)
	}
}
