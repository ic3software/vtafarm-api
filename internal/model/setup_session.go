package model

import "time"

type SetupSession struct {
	ID               uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID           uint      `gorm:"not null;index"           json:"user_id"`
	Status           string    `gorm:"not null;default:pending" json:"status"`
	Mode             string    `gorm:"not null"                 json:"mode"`
	Domain           string    `gorm:"not null"                 json:"domain"`
	Subdomain        string    `gorm:"not null"                 json:"subdomain"`
	CFRecordID       string    `                                json:"-"`
	ErrorMsg         string    `gorm:"not null;default:''"      json:"error_msg,omitempty"`
	// VTA config inputs
	VtaName          string    `gorm:"not null;default:'personal-vta'" json:"vta_name"`
	MediatorDid      string    `gorm:"column:mediator_did;not null;default:''"  json:"mediator_did"`
	VtaDidUrl        string    `gorm:"column:vta_did_url;not null;default:''"   json:"vta_did_url"`
	Portable         bool      `gorm:"not null;default:true"           json:"portable"`
	PreRotationCount int       `gorm:"not null;default:1"              json:"pre_rotation_count"`
	// Image used for the vta-setup K8s Job
	VtaImage         string    `gorm:"not null;default:''"             json:"vta_image,omitempty"`
	// Output populated after vta setup runs
	VtaDid   string `gorm:"column:vta_did;not null;default:''"    json:"vta_did,omitempty"`
	DidLog   string `gorm:"column:did_log;not null;default:''"    json:"-"`
	AdminDid string `gorm:"column:admin_did;not null;default:''" json:"admin_did,omitempty"`
	CreatedAt        time.Time `                                       json:"created_at"`
	UpdatedAt        time.Time `                                       json:"updated_at"`
}

func (s *SetupSession) FQDN() string {
	return s.Subdomain + "." + s.Domain
}

func (s *SetupSession) PublicURL() string {
	return "https://" + s.FQDN()
}
