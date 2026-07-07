package model

import "time"

const (
	ModeVtaOnly          = "vta_only"
	ModeFullStack        = "full_stack"
	ModeFullStackWithVtc = "full_stack_with_vtc"
)

type SetupSession struct {
	ID         uint   `gorm:"primaryKey;autoIncrement" json:"-"`
	UniqueId   string `gorm:"column:unique_id;size:8;not null;uniqueIndex" json:"id"`
	UserID     uint   `gorm:"not null;index"           json:"user_id"`
	Status     string `gorm:"not null;default:pending" json:"status"`
	Mode       string `gorm:"not null"                 json:"mode"`
	Domain     string `gorm:"not null"                 json:"domain"`
	Subdomain  string `gorm:"not null"                 json:"subdomain"`
	CFRecordID string `                                json:"-"`
	ErrorMsg   string `gorm:"not null;default:''"      json:"error_msg,omitempty"`
	// VTA config inputs
	VtaName          string `gorm:"not null;default:'personal-vta'" json:"vta_name"`
	MediatorDid      string `gorm:"column:mediator_did;not null;default:''"  json:"mediator_did"`
	VtaDidUrl        string `gorm:"column:vta_did_url;not null;default:''"   json:"vta_did_url"`
	Portable         bool   `gorm:"not null;default:true"           json:"portable"`
	PreRotationCount int    `gorm:"not null;default:1"              json:"pre_rotation_count"`
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

	// full_stack_with_vtc — the VTC component. Subdomain/CFRecordVtc follow
	// the same pattern as MediatorSubdomain/CFRecordMediator above. Empty ('')
	// for other modes, same convention as the mediator/dids columns.
	VtcSubdomain string  `gorm:"column:vtc_subdomain;not null;default:''" json:"vtc_subdomain,omitempty"`
	CFRecordVtc  *string `gorm:"column:cf_record_vtc" json:"-"`

	// VtcName doubles as the VTA context id the VTC's community lives under
	// (design §8/§9); VtcImage is required for full_stack_with_vtc, like
	// MediatorImage/DidsImage.
	VtcName  string `gorm:"column:vtc_name;not null;default:'personal-vtc'" json:"vtc_name,omitempty"`
	VtcImage string `gorm:"column:vtc_image;not null;default:''" json:"vtc_image,omitempty"`

	// full_stack_with_vtc — collected outputs. VtcSetupKeyDid is the ephemeral
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

// VtcFQDN is full_stack_with_vtc-only, same convention as MediatorFQDN/DidsFQDN.
func (s *SetupSession) VtcFQDN() string {
	return s.VtcSubdomain + "." + s.Domain
}

// IsFullStackFamily reports whether the session runs the full_stack component
// pipeline (full_stack or full_stack_with_vtc) — used wherever handlers and
// the orchestrator dispatch vta_only vs the full_stack-shaped flows.
func (s *SetupSession) IsFullStackFamily() bool {
	return s.Mode == ModeFullStack || s.Mode == ModeFullStackWithVtc
}
