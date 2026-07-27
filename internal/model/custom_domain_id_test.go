package model

import "testing"

// CustomDomainID is what every per-domain resource name goes through, so the
// property that matters is the one asserted here: it says "yes" only for a
// custom session that actually carries a domain_id, and never dereferences a
// nil pointer to do it.
func TestCustomDomainID(t *testing.T) {
	id := uint(4)

	cases := []struct {
		name    string
		session SetupSession
		wantID  uint
		wantOK  bool
	}{
		{
			name:    "custom with a domain",
			session: SetupSession{DomainType: DomainCustom, DomainID: &id},
			wantID:  4,
			wantOK:  true,
		},
		{
			// The cluster wildcard covers these; they own no certificate.
			name:    "managed",
			session: SetupSession{DomainType: DomainManaged},
			wantOK:  false,
		},
		{
			name:    "platform",
			session: SetupSession{DomainType: DomainPlatform, DomainID: &id},
			wantOK:  false,
		},
		{
			// setup_sessions_domain_link_check rules this out in the database.
			// It must still not panic if a row ever arrives that way.
			name:    "custom without a domain id",
			session: SetupSession{DomainType: DomainCustom},
			wantOK:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotOK := tc.session.CustomDomainID()
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if gotOK && gotID != tc.wantID {
				t.Errorf("id = %d, want %d", gotID, tc.wantID)
			}
		})
	}
}
