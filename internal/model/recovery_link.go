package model

import "time"

// RecoveryLink is an admin-issued, single-use, short-lived login link that
// restores access to an existing user account after a lost passkey. The admin
// verifies the requester's identity out of band (the system never verifies
// email ownership) and delivers the URL out of band too — nothing is emailed.
// Consuming the link revokes every passkey on the account — whoever asked for
// recovery can't use them, so any survivor is presumed to be in someone
// else's hands — and logs the holder in so they can register a fresh one.
type RecoveryLink struct {
	ID        uint       `json:"id"         gorm:"primaryKey;autoIncrement"`
	Token     string     `json:"token"      gorm:"not null;uniqueIndex"`
	UserID    uint       `json:"user_id"    gorm:"not null;index"`
	AdminID   uint       `json:"admin_id"   gorm:"not null"`
	ExpiresAt time.Time  `json:"expires_at" gorm:"not null"`
	UsedAt    *time.Time `json:"used_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}
