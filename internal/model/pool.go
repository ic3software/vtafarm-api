package model

import "time"

const (
	PoolPurposeAdminStation = "admin_station"
	PoolPurposePublicSignup = "public_signup"
)

// Pool setup statuses extend the ordinary setup-session lifecycle while a VTA
// is being prepared as inactive inventory.
const (
	StatusPoolCreating   = "pool_creating"
	StatusPoolPublishing = "pool_publishing"
	StatusPoolPrepared   = "pool_prepared"
	StatusPoolWaiting    = "pool_waiting"
)

// PoolBatch records one administrative request to create pooled VTAs. The
// actual users, sessions, images, infrastructure and lifecycle state remain in
// their existing tables.
type PoolBatch struct {
	ID                    uint       `gorm:"primaryKey" json:"id"`
	CreatedBy             uint       `json:"created_by"`
	Label                 string     `json:"label"`
	Purpose               string     `gorm:"not null;default:'admin_station'" json:"purpose"`
	RequestedCount        int        `json:"requested_count"`
	IdempotencyKey        string     `json:"-"`
	RequestDigest         string     `json:"-"`
	PublicSignupEnabledAt *time.Time `json:"public_signup_enabled_at,omitempty"`
	CleanupRequestedAt    *time.Time `json:"-"`
	CreatedAt             time.Time  `json:"created_at"`
}

// PoolSession is membership, not another copy of setup-session state.
type PoolSession struct {
	SessionID uint `gorm:"primaryKey" json:"session_id"`
	BatchID   uint `gorm:"not null;index" json:"batch_id"`
}

// PoolBatchAddition is the immutable audit/idempotency record for one
// inventory increase on an existing batch.
type PoolBatchAddition struct {
	ID             uint      `gorm:"primaryKey"`
	BatchID        uint      `gorm:"not null;index"`
	CreatedBy      uint      `gorm:"not null"`
	RequestedCount int       `gorm:"not null"`
	IdempotencyKey string    `gorm:"not null"`
	RequestDigest  string    `gorm:"not null"`
	CreatedAt      time.Time `gorm:"not null"`
}
