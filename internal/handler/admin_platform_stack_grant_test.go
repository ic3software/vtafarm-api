package handler

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
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

// The Job imports the DID, unless it is already there, then reads the complete
// ACL while the store is already offline so the database snapshot stays fresh.
func TestGrantCmdImportsAndListsAcl(t *testing.T) {
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
	if !strings.Contains(cmd, "vta acl list 2>&1") {
		t.Error("grantCmd must read the authoritative ACL after importing")
	}
	if !strings.Contains(cmd, aclListBeginMarker) || !strings.Contains(cmd, aclListEndMarker) {
		t.Error("grantCmd must delimit the ACL output for parsing")
	}
}

func TestParseVtaAclList(t *testing.T) {
	logs := "import output\n" + aclListBeginMarker + `
2 ACL entries:

  DID:      did:key:z6MkAlice
  Role:     admin (super admin)
  Label:    Alice phone
  Contexts: (unrestricted)
  Created:  2026-09-21 09:10:11 +00:00

  DID:      did:key:z6MkService
  Role:     application
  Contexts: team/one
  Created:  2026-09-20 08:00:00 +00:00

` + aclListEndMarker + "\n"

	entries, err := parseVtaAclList(logs)
	if err != nil {
		t.Fatalf("parseVtaAclList() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("parseVtaAclList() returned %d entries, want 2", len(entries))
	}
	if entries[0].Did != "did:key:z6MkAlice" || entries[0].Role != "admin (super admin)" ||
		entries[0].Label != "Alice phone" || entries[0].Contexts != "(unrestricted)" ||
		entries[0].AclCreatedAt != "2026-09-21 09:10:11 +00:00" {
		t.Fatalf("first entry parsed incorrectly: %#v", entries[0])
	}
	if entries[1].Label != "" {
		t.Fatalf("missing label should stay empty, got %q", entries[1].Label)
	}
}

func TestParseVtaAclListEmpty(t *testing.T) {
	logs := aclListBeginMarker + "\nNo ACL entries found.\n" + aclListEndMarker
	entries, err := parseVtaAclList(logs)
	if err != nil {
		t.Fatalf("parseVtaAclList() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("parseVtaAclList() returned %d entries, want 0", len(entries))
	}
}

func TestParseVtaAclListRejectsIncompleteOutput(t *testing.T) {
	logs := aclListBeginMarker + "\n1 ACL entries:\n  DID: did:key:z6MkBroken\n  Role: admin\n" + aclListEndMarker
	if _, err := parseVtaAclList(logs); err == nil {
		t.Fatal("parseVtaAclList() accepted an incomplete entry")
	}
}

func TestParseVtaAclListRejectsUnknownFormat(t *testing.T) {
	logs := aclListBeginMarker + "\nACL output changed\n" + aclListEndMarker
	if _, err := parseVtaAclList(logs); err == nil {
		t.Fatal("parseVtaAclList() accepted output without an entry count")
	}
}

func TestParseVtaAclListRejectsCountMismatch(t *testing.T) {
	logs := aclListBeginMarker + `
2 ACL entries:
  DID: did:key:z6MkOnlyOne
  Role: admin (super admin)
  Contexts: (unrestricted)
  Created: 2026-09-21 09:10:11 +00:00
` + aclListEndMarker
	if _, err := parseVtaAclList(logs); err == nil {
		t.Fatal("parseVtaAclList() accepted a mismatched entry count")
	}
}

func TestSortVtaAclEntriesNewestFirst(t *testing.T) {
	entries := []model.VtaAclEntry{
		{Did: "did:key:zOld", AclCreatedAt: "2026-09-21 09:57:56 -07:00"},
		{Did: "did:key:zInvalid", AclCreatedAt: "unknown"},
		{Did: "did:key:zNewest", AclCreatedAt: "2026-09-21 17:32:35 +00:00"},
		{Did: "did:key:zMiddle", AclCreatedAt: "2026-09-21 09:53:03 -07:00"},
	}

	sortVtaAclEntriesNewestFirst(entries)
	want := []string{"did:key:zNewest", "did:key:zOld", "did:key:zMiddle", "did:key:zInvalid"}
	for i, did := range want {
		if entries[i].Did != did {
			t.Fatalf("entry %d = %q, want %q", i, entries[i].Did, did)
		}
	}
}

func TestSuperAdminAclEntriesFiltersResponseWithoutMutatingSnapshot(t *testing.T) {
	entries := []model.VtaAclEntry{
		{Did: "did:key:zSuperAdmin", Role: superAdminAclRole},
		{Did: "did:key:zScopedAdmin", Role: "admin"},
		{Did: "did:key:zApplication", Role: "application"},
	}

	filtered := superAdminAclEntries(entries)
	if len(filtered) != 1 || filtered[0].Did != "did:key:zSuperAdmin" {
		t.Fatalf("superAdminAclEntries() = %#v, want only the super admin", filtered)
	}
	if len(entries) != 3 {
		t.Fatalf("superAdminAclEntries() mutated the complete snapshot: got %d entries, want 3", len(entries))
	}
}

// A label identifies the entry after PNM rotates the DID away. It is optional,
// so an empty value must omit the flag rather than pass an empty string.
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
	const sessionID = uint(42)
	if !aclJobLocks.TryLock(sessionID) {
		t.Fatal("lock was already held at the start of the test")
	}
	defer aclJobLocks.Unlock(sessionID)

	// A second caller must be turned away immediately. Waiting would mean
	// sitting through one outage and then starting another.
	if aclJobLocks.TryLock(sessionID) {
		aclJobLocks.Unlock(sessionID)
		t.Fatal("a second ACL job acquired the lock; concurrent windows would corrupt each other")
	}

	// A different VTA has different resources and must not be blocked by this
	// session's maintenance window.
	const otherSessionID = uint(43)
	if !aclJobLocks.TryLock(otherSessionID) {
		t.Fatal("an unrelated session was blocked by this session's ACL job")
	}
	aclJobLocks.Unlock(otherSessionID)
}

func TestVtaAclTargetForEachMode(t *testing.T) {
	tests := []struct {
		name    string
		session model.SetupSession
		want    vtaAclTarget
	}{
		{
			name:    "vta only",
			session: model.SetupSession{ID: 42, Mode: model.ModeVtaOnly},
			want: vtaAclTarget{
				deployment: "vta-42",
				selector:   "app=vta,session-id=42",
				job:        "vta-acl-42",
				pvc:        "vta-data-42",
			},
		},
		{
			name:    "full stack",
			session: model.SetupSession{ID: 42, Mode: model.ModeFullStack},
			want: vtaAclTarget{
				deployment: "fs-42-vta",
				selector:   "app=fs-vta,session-id=42",
				job:        "fs-42-vta-acl",
				pvc:        "fs-42-vta",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := vtaAclTargetFor(&tt.session); got != tt.want {
				t.Fatalf("vtaAclTargetFor() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestAdditionalPnmLabelKeepsDidSuffix(t *testing.T) {
	did := "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"
	if got, want := additionalPnmLabel(did), "pnm-backup-nnEGta2doK"; got != want {
		t.Fatalf("additionalPnmLabel() = %q, want %q", got, want)
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
