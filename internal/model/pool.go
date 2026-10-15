package model

import "time"

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
	ID             uint      `gorm:"primaryKey" json:"id"`
	CreatedBy      uint      `json:"created_by"`
	Label          string    `json:"label"`
	RequestedCount int       `json:"requested_count"`
	IdempotencyKey string    `json:"-"`
	RequestDigest  string    `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
}

// PoolSession is membership, not another copy of setup-session state.
type PoolSession struct {
	SessionID uint `gorm:"primaryKey" json:"session_id"`
	BatchID   uint `gorm:"not null;index" json:"batch_id"`
}
