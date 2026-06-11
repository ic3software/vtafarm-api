package model

import "time"

type InvitationLink struct {
	ID        uint       `json:"id"         gorm:"primaryKey;autoIncrement"`
	Token     string     `json:"token"      gorm:"not null;uniqueIndex"`
	AdminID   uint       `json:"admin_id"   gorm:"not null"`
	ExpiresAt time.Time  `json:"expires_at" gorm:"not null"`
	UsedAt    *time.Time `json:"used_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}
