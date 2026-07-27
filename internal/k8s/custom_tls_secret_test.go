package k8s

import "testing"

// The name is keyed by domain, and the reuse that saves Let's Encrypt quota
// rests entirely on that: two sessions on one domain must ask for the same
// Secret, or the rebuild issues from scratch and spends one of five per week.
func TestCustomTLSSecretIsKeyedByDomain(t *testing.T) {
	// Derived from the domain alone: no session id reaches it, which is what
	// makes a rebuild name the Secret its predecessor left behind.
	if got, want := CustomTLSSecret(4), "custom-4-tls"; got != want {
		t.Fatalf("CustomTLSSecret(4) = %q, want %q", got, want)
	}
	if CustomTLSSecret(4) == CustomTLSSecret(5) {
		t.Error("two domains share a Secret name — they would overwrite each other")
	}
}

// It must not collide with the per-session names in the same namespace, which
// are keyed by session id over the same small integers.
func TestCustomTLSSecretDoesNotCollideWithSessionNames(t *testing.T) {
	const id = 4
	taken := map[string]string{
		FSVtaName(id):      "vta",
		FSMediatorName(id): "mediator",
		FSDidsName(id):     "dids",
		FSVtcName(id):      "vtc",
	}
	if component, clash := taken[CustomTLSSecret(id)]; clash {
		t.Fatalf("CustomTLSSecret(%d) collides with the %s resource name", id, component)
	}
}
