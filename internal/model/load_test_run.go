package model

import "time"

const (
	LoadTestCreating     = "creating"
	LoadTestActive       = "active"
	LoadTestPartial      = "partial"
	LoadTestFailed       = "failed"
	LoadTestDeleting     = "deleting"
	LoadTestDeleted      = "deleted"
	LoadTestDeleteFailed = "delete_failed"
)

// LoadTestRun is one admin-triggered batch of VTA-only sessions. The session
// rows carry the run id; this record survives teardown so the result remains
// visible after the test resources have been removed.
type LoadTestRun struct {
	ID             uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID         *uint     `gorm:"column:user_id" json:"-"`
	RequestedBy    *uint     `gorm:"column:requested_by" json:"-"`
	RequestedCount int       `gorm:"column:requested_count;not null" json:"requested_count"`
	CreatedCount   int       `gorm:"column:created_count;not null;default:0" json:"created_count"`
	VtaImage       string    `gorm:"column:vta_image;not null" json:"vta_image"`
	Status         string    `gorm:"column:status;not null" json:"status"`
	ErrorMsg       string    `gorm:"column:error_msg;not null;default:''" json:"error_msg,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
