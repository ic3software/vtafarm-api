package model

import "time"

type MobileConnection struct {
	ID              string `gorm:"type:uuid;primaryKey"`
	SessionID       uint
	VtaDid          string
	Operation       string
	AdminDid        string
	Status          string
	ClaimHandle     *string
	ClaimantDigest  *string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	ConfirmBy       *time.Time
	ClaimedAt       *time.Time
	ConfirmedAt     *time.Time
	ReleasedAt      *time.Time
	RedeemPollID    *string
	RedeemPollUntil *time.Time
	ProvisionError  string
}
