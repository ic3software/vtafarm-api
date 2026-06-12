package model

import "time"

type AdminPasskey struct {
	ID           uint64     `json:"id"           gorm:"primaryKey;autoIncrement"`
	AdminID      uint       `json:"-"            gorm:"not null;index"`
	CredentialID []byte     `json:"-"            gorm:"not null;uniqueIndex;type:bytea"`
	Credential   []byte     `json:"-"            gorm:"not null;type:bytea"`
	Name         string     `json:"name"         gorm:"not null;default:''"`
	LastUsedAt   *time.Time `json:"last_used_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}
