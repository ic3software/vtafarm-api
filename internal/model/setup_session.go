package model

import "time"

const (
	ModeVtaOnly   = "vta_only"
	ModeFullStack = "full_stack"
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
