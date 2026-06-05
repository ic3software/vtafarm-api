package model

import "time"

type SetupSession struct {
	ID         uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID     uint      `gorm:"not null;index"           json:"user_id"`
	Status     string    `gorm:"not null;default:pending" json:"status"`
	Mode       string    `gorm:"not null"                 json:"mode"`
	Domain     string    `gorm:"not null"                 json:"domain"`
	Subdomain  string    `gorm:"not null"                 json:"subdomain"`
	CFRecordID string    `                                json:"cf_record_id,omitempty"`
	DidLog     string    `gorm:"not null;default:''"      json:"-"`
	ErrorMsg   string    `gorm:"not null;default:''"      json:"error_msg,omitempty"`
	CreatedAt  time.Time `                                json:"created_at"`
	UpdatedAt  time.Time `                                json:"updated_at"`
}

func (s *SetupSession) FQDN() string {
	return s.Subdomain + "." + s.Domain
}
