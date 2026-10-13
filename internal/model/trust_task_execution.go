package model

import "time"

type TrustTaskExecution struct {
	DocumentID     string `gorm:"primaryKey"`
	DocumentDigest string
	State          string
	ResponseBytes  []byte
	RetainUntil    time.Time
	LeaseExpiresAt time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
