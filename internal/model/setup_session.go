package model

import "time"

// The only two setup modes. full_stack always provisions all four components
// (VTA + mediator + dids daemon + VTC) — the VTC is not optional. An earlier
// iteration split this into full_stack (three components) and
// full_stack_with_vtc (four); that split is retired and the
// full_stack_with_vtc identifier no longer exists anywhere.
const (
	ModeVtaOnly   = "vta_only"
	ModeFullStack = "full_stack"
)

// Where a vta_only session's mediator and DID host came from. Orthogonal to
// both Mode and DomainType, and meaningless for full_stack, which provisions
// its own.
//
// There is deliberately no "external" value: the farm's client DID is enrolled
// as an admin in every full_stack daemon it provisioned and in nothing else, so
// a stack this farm did not build cannot be a target. See
// docs/custom-stack-connection-design.md §1.
const (
	// ConnectionPlatform is the default and the only value any session created
	// before the connection feature can have.
	ConnectionPlatform = "platform"
	// ConnectionInFarm means the session named another full_stack in this farm
	// by pasting its owner's connection bundle.
	ConnectionInFarm = "in_farm"
)

// Where a session's hostnames come from. Orthogonal to Mode: a session is
// vta_only or full_stack, and independently managed, custom or platform.
const (
	// DomainManaged is the default — labels derived from the user's chosen
	// name in our own zone (vta-<name>.firstperson.dev). DomainID is NULL.
	DomainManaged = "managed"
	// DomainCustom is a user-owned zone under fixed labels. Reaches ACME.
	DomainCustom = "custom"
	// DomainPlatform is our own zone under fixed labels — the flagship stack.
	// DNS is ours to create, and the wildcard already covers TLS, so it costs
	// no verification and no certificate work.
	DomainPlatform = "platform"
)

type SetupSession struct {
	ID uint `gorm:"primaryKey;autoIncrement" json:"-"`
	// VtaName below is the public identifier — there is no opaque id. See its
	// comment for why that makes it globally unique.
	UserID     uint   `gorm:"not null;index"           json:"user_id"`
	Status     string `gorm:"not null;default:pending" json:"status"`
	Mode       string `gorm:"not null"                 json:"mode"`
	Domain     string `gorm:"not null"                 json:"domain"`
	Subdomain  string `gorm:"not null"                 json:"subdomain"`
	CFRecordID string `                                json:"-"`
	ErrorMsg   string `gorm:"not null;default:''"      json:"error_msg,omitempty"`

	// DomainID links to the domains row backing this session; NULL exactly
	// when DomainType is managed (enforced by setup_sessions_domain_link_check).
	// DomainType is denormalised from that row's kind so dispatch stays a
	// single column read. Domain/Subdomain/*Subdomain above keep holding the
	// rendered values either way, so every FQDN accessor works unchanged.
	DomainID   *uint  `gorm:"column:domain_id"                            json:"-"`
	DomainType string `gorm:"column:domain_type;not null;default:managed" json:"domain_type"`
	// VTA config inputs
	// VtaName is the session's public identifier as well as its name: it is the
	// :id in /setup/<name> and the word typed to confirm a delete. Globally
	// unique — not merely per-user and not merely among managed sessions —
	// because the admin routes resolve a session by name with no user_id to
	// scope the lookup. On a fixed-label domain that means a label two users
	// might both want ("main") is first-come.
	VtaName     string `gorm:"not null;default:'personal-vta'" json:"vta_name"`
	MediatorDid string `gorm:"column:mediator_did;not null;default:''"  json:"mediator_did"`
	VtaDidUrl   string `gorm:"column:vta_did_url;not null;default:''"   json:"vta_did_url"`
	// Where this session's did:webvh identifiers are served, and where the
	// daemon serving them is administered — the shared daemon for vta_only, the
	// session's own for full_stack. Recorded per session rather than read from
	// configuration because a did:webvh bakes its host into the identifier at
	// mint time: these are facts about the session, not current settings, so a
	// teardown reaches the daemon the DID was actually uploaded to.
	//
	// Two fields because a standalone DID-hosting service splits resolution from
	// its management API. The daemon build deployed today answers both roles on
	// one host, so they are equal for every session that exists so far.
	//
	// ServerURL scopes setup_sessions_did_path_unique: a DID path only has to be
	// distinct among the DIDs served at the same URL, and which sessions those
	// are follows from neither Mode nor DomainType alone.
	DidHostingServerURL  string `gorm:"column:did_hosting_server_url;not null;default:''"  json:"-"`
	DidHostingControlURL string `gorm:"column:did_hosting_control_url;not null;default:''" json:"-"`

	// ShareCode is the grant that lets somebody else's vta_only session connect
	// to this stack. full_stack only; NULL means "not shared", which is also
	// every session's starting state and the platform stack's permanent one —
	// that stack is reached by the default path, which sends no bundle.
	//
	// Minting enables sharing, clearing disables it, and replacing invalidates
	// every bundle already handed out. None of the three touch a session already
	// connected: the code gates joining, never membership.
	ShareCode *string `gorm:"column:share_code" json:"-"`

	// ConnectionSource says where this session's mediator and DID host came
	// from — ConnectionPlatform or ConnectionInFarm. ProviderSessionID is the
	// full_stack row it connected to, and is NULL both for platform sessions
	// (which never had one) and for a session whose provider has since been
	// deleted.
	//
	// Neither is needed to run the session; the three snapshotted values above
	// do that, and stay authoritative because a did:webvh bakes its host in at
	// mint time. These answer what a snapshot cannot: who the dependents of a
	// stack are, what to call the provider in the UI, and — via
	// ON DELETE SET NULL — whether that provider still exists at all.
	ConnectionSource  string `gorm:"column:connection_source;not null;default:platform" json:"connection_source"`
	ProviderSessionID *uint  `gorm:"column:provider_session_id"                         json:"-"`
	// LoadTestRunID groups sessions created by the admin provisioning load-test
	// workflow. Ordinary user and platform sessions leave it NULL.
	LoadTestRunID    *uint `gorm:"column:load_test_run_id" json:"-"`
	Portable         bool  `gorm:"not null;default:true"           json:"portable"`
	PreRotationCount int   `gorm:"not null;default:1"              json:"pre_rotation_count"`
	// Image used for the vta-setup K8s Job
	VtaImage string `gorm:"not null;default:''"             json:"vta_image,omitempty"`
	// Output populated after vta setup runs
	VtaDid   string `gorm:"column:vta_did;not null;default:''"   json:"vta_did,omitempty"`
	AdminDid string `gorm:"column:admin_did;not null;default:''" json:"admin_did,omitempty"`

	// full_stack — mediator/dids subdomains. The VTA component reuses
	// Subdomain/CFRecordID above (same as vta_only) rather than getting its
	// own columns. Empty ('') for vta_only rows, same convention as VtaImage/
	// VtaDid/AdminDid above.
	MediatorSubdomain string `gorm:"column:mediator_subdomain;not null;default:''" json:"mediator_subdomain,omitempty"`
	DidsSubdomain     string `gorm:"column:dids_subdomain;not null;default:''"     json:"dids_subdomain,omitempty"`

	// full_stack — Cloudflare record ids for mediator/dids (CFRecordID above
	// covers the VTA). Nullable, matching CFRecordID's own convention.
	CFRecordMediator *string `gorm:"column:cf_record_mediator" json:"-"`
	CFRecordDids     *string `gorm:"column:cf_record_dids"     json:"-"`

	// full_stack — per-component images (VtaImage above covers the VTA).
	MediatorImage string `gorm:"column:mediator_image;not null;default:''" json:"mediator_image,omitempty"`
	DidsImage     string `gorm:"column:dids_image;not null;default:''"     json:"dids_image,omitempty"`

	// full_stack — collected outputs. MediatorDid (1b) is reused from above;
	// AdminDid already holds the user-supplied PNM admin DID (4a). Empty ('')
	// until the corresponding setup step completes, same convention as VtaDid.
	MediatorAdminDid   string `gorm:"column:mediator_admin_did;not null;default:''"    json:"mediator_admin_did,omitempty"`    // 2b
	DIDHostingAdminDid string `gorm:"column:did_hosting_admin_did;not null;default:''" json:"did_hosting_admin_did,omitempty"` // 3b
	DIDHostingDid      string `gorm:"column:did_hosting_did;not null;default:''"       json:"did_hosting_did,omitempty"`       // 3d

	// full_stack — admin private keys, returned to the user once for offline backup.
	MediatorAdminKey string `gorm:"column:mediator_admin_key;not null;default:''" json:"mediator_admin_key,omitempty"` // 2c
	WebvhAdminKey    string `gorm:"column:webvh_admin_key;not null;default:''"    json:"webvh_admin_key,omitempty"`    // 3c
	DidsEnrollURL    string `gorm:"column:dids_enroll_url;not null;default:''"    json:"dids_enroll_url,omitempty"`    // 3e

	// DidsEnrollUsed is set by the frontend (POST .../dids/enroll-ack) the
	// moment the user opens DidsEnrollURL — it's single-use at the daemon
	// level, so this just lets the UI stop offering a link that will fail if
	// clicked again. Reissue clears it back to false along with the new URL.
	DidsEnrollUsed bool `gorm:"column:dids_enroll_used;not null;default:false" json:"dids_enroll_used"`

	// full_stack — the VTC component. Subdomain/CFRecordVtc follow the same
	// pattern as MediatorSubdomain/CFRecordMediator above. Empty ('') for
	// vta_only, same convention as the mediator/dids columns.
	VtcSubdomain string  `gorm:"column:vtc_subdomain;not null;default:''" json:"vtc_subdomain,omitempty"`
	CFRecordVtc  *string `gorm:"column:cf_record_vtc" json:"-"`

	// VtcName doubles as the VTA context id the VTC's community lives under
	// (design §6/§7); VtcImage is required for full_stack, like
	// MediatorImage/DidsImage. '' for vta_only.
	VtcName  string `gorm:"column:vtc_name;not null" json:"vtc_name,omitempty"`
	VtcImage string `gorm:"column:vtc_image;not null;default:''" json:"vtc_image,omitempty"`

	// full_stack — collected outputs. VtcSetupKeyDid is the ephemeral
	// did:key from step_vtc_setup_key, kept for audit/debug only — nothing
	// reads it back from the DB. VtcAdminDid is the VTC's own pre-claim
	// install admin from the setup summary, NOT the PNM AdminDid column above.
	VtcSetupKeyDid string `gorm:"column:vtc_setup_key_did;not null;default:''" json:"vtc_setup_key_did,omitempty"`
	VtcDid         string `gorm:"column:vtc_did;not null;default:''" json:"vtc_did,omitempty"`
	VtcAdminDid    string `gorm:"column:vtc_admin_did;not null;default:''" json:"vtc_admin_did,omitempty"`

	// Reveal-once install credentials, like MediatorAdminKey/WebvhAdminKey —
	// the claim code is delivered over a logically separate channel from the URL.
	VtcInstallURL string `gorm:"column:vtc_install_url;not null;default:''" json:"vtc_install_url,omitempty"`
	VtcClaimCode  string `gorm:"column:vtc_claim_code;not null;default:''" json:"vtc_claim_code,omitempty"`

	// VtcInstallUsed mirrors DidsEnrollUsed — set by the frontend (POST
	// .../vtc/install-ack) once the user opens VtcInstallURL, so GET /setup/:id
	// stops re-offering a dead link. The VTC's own install-token state machine
	// already refuses a second claim; this just improves the UI.
	VtcInstallUsed bool `gorm:"column:vtc_install_used;not null;default:false" json:"vtc_install_used"`

	CreatedAt time.Time `                                       json:"created_at"`
	UpdatedAt time.Time `                                       json:"updated_at"`
}

func (s *SetupSession) FQDN() string {
	return s.Subdomain + "." + s.Domain
}

func (s *SetupSession) PublicURL() string {
	return "https://" + s.FQDN()
}

// MediatorFQDN/DidsFQDN are full_stack-only — the VTA's hostname is FQDN()
// above (it reuses the shared Subdomain field, same as vta_only).
func (s *SetupSession) MediatorFQDN() string {
	return s.MediatorSubdomain + "." + s.Domain
}

func (s *SetupSession) DidsFQDN() string {
	return s.DidsSubdomain + "." + s.Domain
}

// DidsURL is the public base URL of the session's own DID-hosting daemon —
// what gets recorded as DidHostingServerURL/DidHostingControlURL, both for the
// session itself and, when this is the platform stack, for every vta_only
// session wired to it.
func (s *SetupSession) DidsURL() string {
	return "https://" + s.DidsFQDN()
}

// VtcFQDN is full_stack-only, same convention as MediatorFQDN/DidsFQDN.
func (s *SetupSession) VtcFQDN() string {
	return s.VtcSubdomain + "." + s.Domain
}

// IsFullStack reports whether the session runs the four-component pipeline —
// used wherever handlers and the orchestrator dispatch vta_only vs full_stack.
func (s *SetupSession) IsFullStack() bool {
	return s.Mode == ModeFullStack
}

// IsShared reports whether this stack currently accepts new connections.
//
// Only a full_stack can be shared, and only one that has finished provisioning:
// a bundle for a stack whose mediator DID or daemon DID has not landed yet
// would name values that are about to change. That readiness rule is the same
// one the platform stack has always been held to before a vta_only could be
// wired to it.
func (s *SetupSession) IsShared() bool {
	return s.IsFullStack() &&
		s.ShareCode != nil && *s.ShareCode != "" &&
		s.Status == "running" &&
		s.MediatorDid != "" &&
		s.DIDHostingDid != ""
}

// IsOrphaned reports whether this session connected to a stack that has since
// been deleted. Its pods keep running — nothing in a provider teardown touches
// the consumer's namespace — but its did:webvh no longer resolves and its
// mediator is gone, so it can neither be reached nor deliver.
//
// Derived from ON DELETE SET NULL rather than written by a delete handler,
// which is why it needs no event to have fired and cannot drift.
func (s *SetupSession) IsOrphaned() bool {
	return s.ConnectionSource == ConnectionInFarm && s.ProviderSessionID == nil
}

// IsFixedLabel reports whether the session's hostnames are the four fixed
// labels rather than name-derived ones. True for custom and platform domains,
// which is also exactly when VtaName/VtcName carry no hostname meaning and are
// free to duplicate across users.
func (s *SetupSession) IsFixedLabel() bool {
	return s.DomainType == DomainCustom || s.DomainType == DomainPlatform
}

// OwnsDNS reports whether vtafarm-api created this session's DNS records and
// must therefore delete them on teardown. False for custom domains, where the
// records are the user's to manage — and to remove, since one left pointing at
// us is a dangling-DNS liability.
func (s *SetupSession) OwnsDNS() bool {
	return s.DomainType != DomainCustom
}

// CustomDomainID returns the domain this session runs on and whether it has one
// at all. Only custom sessions do — managed and platform are served by the
// cluster wildcard and own no certificate.
//
// setup_sessions_domain_link_check already guarantees a custom row has a
// domain_id, so the false case is unreachable in a consistent database. It
// exists so the nullable column is dereferenced in exactly one place rather
// than at each caller that names a per-domain resource.
func (s *SetupSession) CustomDomainID() (uint, bool) {
	if s.DomainType != DomainCustom || s.DomainID == nil {
		return 0, false
	}
	return *s.DomainID, true
}
