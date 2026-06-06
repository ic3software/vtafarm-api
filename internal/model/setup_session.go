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
	MediatorDID      string    `gorm:"not null;default:''"             json:"mediator_did"`
	VtaDidURL        string    `gorm:"not null;default:''"             json:"vta_did_url"`
	Portable         bool      `gorm:"not null;default:true"           json:"portable"`
	PreRotationCount int       `gorm:"not null;default:1"              json:"pre_rotation_count"`
	// Output populated after vta setup runs
	VtaDID           string    `gorm:"not null;default:''"             json:"vta_did,omitempty"`
	CreatedAt        time.Time `                                       json:"created_at"`
	UpdatedAt        time.Time `                                       json:"updated_at"`
}

func (s *SetupSession) FQDN() string {
	return s.Subdomain + "." + s.Domain
}

func (s *SetupSession) PublicURL() string {
	return "https://" + s.FQDN()
}
