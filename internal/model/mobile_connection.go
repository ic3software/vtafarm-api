package model

import "time"

type MobileConnection struct {
	ID             string `gorm:"type:uuid;primaryKey"`
	SessionID      uint
	VtaDid         string
	Operation      string
	AdminDid       string
	Status         string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	AcceptedAt     *time.Time
	ProvisionedAt  *time.Time
	ConnectedAt    *time.Time
	ProvisionError string
}
