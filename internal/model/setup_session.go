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

	// full_stack — three subdomains instead of one (Subdomain stays for vta_only back-compat).
	VtaSubdomain      string `gorm:"column:vta_subdomain;not null;default:''"      json:"vta_subdomain,omitempty"`
	MediatorSubdomain string `gorm:"column:mediator_subdomain;not null;default:''" json:"mediator_subdomain,omitempty"`
	DidsSubdomain     string `gorm:"column:dids_subdomain;not null;default:''"     json:"dids_subdomain,omitempty"`

	// full_stack — one Cloudflare record id per host (CFRecordID stays for vta_only).
	CFRecordVta      string `gorm:"column:cf_record_vta;not null;default:''"      json:"-"`
	CFRecordMediator string `gorm:"column:cf_record_mediator;not null;default:''" json:"-"`
	CFRecordDids     string `gorm:"column:cf_record_dids;not null;default:''"     json:"-"`

	// full_stack — per-component images (VtaImage above covers the VTA).
	MediatorImage string `gorm:"column:mediator_image;not null;default:''" json:"mediator_image,omitempty"`
	DidsImage     string `gorm:"column:dids_image;not null;default:''"     json:"dids_image,omitempty"`

	// full_stack — collected outputs. MediatorDid (1b) is reused from above;
	// AdminDid already holds the user-supplied PNM admin DID (4a).
	MediatorAdminDid string `gorm:"column:mediator_admin_did;not null;default:''" json:"mediator_admin_did,omitempty"` // 2b
	WebvhAdminDid    string `gorm:"column:webvh_admin_did;not null;default:''"    json:"webvh_admin_did,omitempty"`    // 3b
	DidsDaemonDid    string `gorm:"column:dids_daemon_did;not null;default:''"    json:"dids_daemon_did,omitempty"`    // 3d

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

// MediatorFQDN/DidsFQDN are full_stack-only — VtaFQDN below covers the VTA
// in that mode (VtaSubdomain instead of the shared Subdomain field).
func (s *SetupSession) VtaFQDN() string {
	return s.VtaSubdomain + "." + s.Domain
}

func (s *SetupSession) MediatorFQDN() string {
	return s.MediatorSubdomain + "." + s.Domain
}

func (s *SetupSession) DidsFQDN() string {
	return s.DidsSubdomain + "." + s.Domain
}
