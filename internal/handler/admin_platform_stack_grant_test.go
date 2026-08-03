package handler

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDidKeyValidation(t *testing.T) {
	cases := []struct {
		did  string
		want bool
	}{
		{"did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK", true},
		{"", false},
		{"did:key:", false},
		{"did:web:example.com", false},
		{"did:webvh:abc:dids.firstperson.dev:x-vta", false},
		{"did:key:z6Mk", false},                            // too short to be a real multibase
		{"did:key:6MkhaXgBZDvotDkL5257faiztiGiC2Q", false}, // no z prefix
		// Base58btc excludes 0, O, I and l precisely so they cannot be confused.
		{"did:key:z0OIlhaXgBZDvotDkL5257faiztiGiC2Q", false},
		// The shape that matters for the Job command: nothing quotable gets in.
		{"did:key:z6Mkhax'; rm -rf /", false},
		{"did:key:z6Mkhax with spaces aaaaaaaaaaaaa", false},
	}
	for _, tc := range cases {
		if got := didKeyRe.MatchString(tc.did); got != tc.want {
			t.Errorf("didKeyRe.MatchString(%q) = %v, want %v", tc.did, got, tc.want)
		}
	}
}

// The Job does exactly one thing: import the DID, unless it is already there.
// It deliberately does not read the ACL back — nothing on this side stores a
// copy of it.
func TestGrantCmdImportsAndNothingElse(t *testing.T) {
	cmd := grantCmd("did:key:z6MkTest", "alice")
	if !strings.HasPrefix(cmd, "set -e\n") {
		t.Fatalf("grantCmd must start with `set -e`; got:\n%s", cmd)
	}
	// The probe exists because `vta import-did` prompts on an existing entry
	// and dialoguer's interact() errors on a pod's non-TTY stdin.
	if !strings.Contains(cmd, "vta acl get") {
		t.Error("grantCmd must probe with `vta acl get` before importing")
	}
	if !strings.Contains(cmd, alreadyPresentMarker) {
		t.Error("grantCmd must echo the already-present marker so the handler can report it")
	}
	if strings.Contains(cmd, "acl list") {
		t.Error("grantCmd must not read the ACL back — no copy of it is kept")
	}
}

// The label is what identifies the entry after PNM rotates the DID away, so it
// has to reach the ACL. The handler rejects an empty one; grantCmd still omits
// the flag rather than passing an empty string, for any caller that gets there
// another way.
func TestGrantCmdCarriesTheLabel(t *testing.T) {
	withLabel := grantCmd("did:key:z6MkTest", "alice")
	if !strings.Contains(withLabel, "--label 'alice'") {
		t.Errorf("label not quoted into the command:\n%s", withLabel)
	}
	bare := grantCmd("did:key:z6MkTest", "")
	if strings.Contains(bare, "--label") {
		t.Errorf("empty label must omit the flag entirely, not pass '':\n%s", bare)
	}
}

// Quoting matters more than usual here: unlike the DID, the label is free text
// and didKeyRe does not constrain it.
func TestGrantCmdQuotesAHostileLabel(t *testing.T) {
	cmd := grantCmd("did:key:z6MkTest", `alice'; vta acl delete x --yes #`)
	if !strings.Contains(cmd, `--label 'alice'\''; vta acl delete x --yes #'`) {
		t.Errorf("label not shell-escaped:\n%s", cmd)
	}
}

// Two admins clicking "add" at the same time is the case self-service creates
// and the single-operator flow never had. Both windows would scale the same VTA
// down, then delete and recreate the same Job name under each other.
func TestAclJobLockRefusesRatherThanQueues(t *testing.T) {
	if !aclJobLock.TryLock() {
		t.Fatal("lock was already held at the start of the test")
	}
	defer aclJobLock.Unlock()

	// A second caller must be turned away immediately. Waiting would mean
	// sitting through one outage and then starting another.
	if aclJobLock.TryLock() {
		aclJobLock.Unlock()
		t.Fatal("a second ACL job acquired the lock; concurrent windows would corrupt each other")
	}
}

// Busy has to be distinguishable from broken: it maps to 409, not 502, so an
// operator is told to retry rather than sent looking for damage.
func TestBusyErrorIsIdentifiable(t *testing.T) {
	wrapped := fmt.Errorf("grant failed: %w", errAclJobBusy)
	if !errors.Is(wrapped, errAclJobBusy) {
		t.Error("errAclJobBusy must survive wrapping so respondAclJobError can map it to 409")
	}
	if errors.Is(errors.New("k8s exploded"), errAclJobBusy) {
		t.Error("an unrelated error must not be mistaken for busy")
	}
}
