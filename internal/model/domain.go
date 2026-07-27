package model

import "time"

// The two domain kinds. They share a hostname layout — four fixed labels
// (vta / vtc / mediator / dids) rather than the managed zone's name-derived
// ones — and differ in who owns the zone, which is what decides whether any
// verification or certificate work is needed at all.
const (
	// DomainKindCustom is a zone the user owns. We never write to their DNS:
	// they create the records, we verify them live, and cert-manager issues a
	// certificate covering all four names.
	DomainKindCustom = "custom"
	// DomainKindPlatform is our own zone under fixed labels — the farm's
	// flagship stack. No verification (we control the zone) and no ACME (the
	// wildcard already covers it). Only POST /admin/platform-stack mints one.
	DomainKindPlatform = "platform"
)

// Domain is a zone a session can run under instead of a generated name in the
// managed zone. A domain backs at most one session, enforced by
// setup_sessions_domain_unique — its four labels are fixed, so a second
// session on the same domain would want the same hostnames.
type Domain struct {
	ID uint `gorm:"primaryKey;autoIncrement" json:"-"`
	// UserID owns the domain. For platform rows this is the system account
	// (design §3.3.6) rather than an admin: admins are a separate table, and
	// this id also derives the Kubernetes namespace.
	UserID uint   `gorm:"not null;index"           json:"-"`
	Domain string `gorm:"not null"                 json:"domain"`
	Kind   string `gorm:"not null"                 json:"kind"`
	// VerifyToken is the expected TXT value, minted per attach. Custom only —
	// platform rows carry ''.
	VerifyToken string `gorm:"not null;default:''" json:"-"`
	// VerifiedAt is nil until the DNS check passes. Platform rows are verified
	// on insert. Checked only at verification time: no periodic
	// re-verification, so a user tidying their DNS later never breaks a
	// running session.
	VerifiedAt *time.Time `json:"verified_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Verified reports whether a session may be created against this domain.
func (d *Domain) Verified() bool { return d.VerifiedAt != nil }
