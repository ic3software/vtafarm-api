package model

import "time"

type MobileConnection struct {
	ID          string `gorm:"type:uuid;primaryKey"`
	SessionID   uint
	VtaDid      string
	Status      string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	AcceptedAt  *time.Time
	ConnectedAt *time.Time
}

// The session primary key is also the durable, unique initial provisioning operation.
type InitialProvision struct {
	SessionID  uint `gorm:"primaryKey"`
	AdminDid   string
	CreatedAt  time.Time
	FinishedAt *time.Time
}
