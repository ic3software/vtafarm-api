package model

import "time"

type AdminEnrollmentToken struct {
	ID        uint       `json:"id"         gorm:"primaryKey;autoIncrement"`
	Token     string     `json:"-"          gorm:"not null;uniqueIndex"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}
