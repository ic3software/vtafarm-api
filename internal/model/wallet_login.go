package model

import (
	"encoding/json"
	"time"
)

type WalletLoginBinding struct {
	ID              uint64 `gorm:"primaryKey"`
	UserID          uint
	LoginDID        string `gorm:"column:login_did"`
	SourceSessionID uint
	SourceClaimID   string `gorm:"type:uuid"`
	CreatedAt       time.Time
	RevokedAt       *time.Time
}

type WalletLoginRequest struct {
	RequestID            string `gorm:"primaryKey"`
	State                string
	StarterDID           string `gorm:"column:starter_did"`
	StarterNetworkDigest []byte
	ApproverDID          *string `gorm:"column:approver_did"`
	Origin               string
	MatchNumber          *string
	MatchNumberDelivered bool
	RequesterBrowser     string
	RequesterOS          string
	IdentifiedDID        *string `gorm:"column:identified_did"`
	BindingID            *uint64
	UserID               *uint
	Step1                json.RawMessage `gorm:"type:jsonb"`
	Step2Digest          *string
	GrantDocument        json.RawMessage `gorm:"type:jsonb"`
	SessionEndsAt        *time.Time
	WalletSessionID      *string `gorm:"type:uuid"`
	RedeemPollID         *string
	RedeemPollUntil      *time.Time
	CreatedAt            time.Time
	ClaimClosesAt        time.Time
	DecisionClosesAt     *time.Time
	EndedAt              *time.Time
}

type WalletLoginSession struct {
	ID        string `gorm:"type:uuid;primaryKey"`
	BindingID uint64
	UserID    uint
	ExpiresAt time.Time
	CreatedAt time.Time
	RevokedAt *time.Time
}
