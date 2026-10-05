package model

import "time"

const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

type User struct {
	ID       uint   `json:"id"         gorm:"primaryKey;autoIncrement"`
	UniqueId string `json:"unique_id"  gorm:"column:unique_id;not null"`

	// Email is the self-declared identifier left at public signup. It is NOT
	// verified (the system sends no email) — authentication is the passkey;
	// email only tells an admin who an account belongs to. NULL for accounts
	// created before email signup existed and for admin-invited accounts.
	// Unique when present: one email can never map to two accounts.
	Email *string `json:"email,omitempty" gorm:"column:email"`

	FullstackAccess bool `json:"fullstack_access" gorm:"column:fullstack_access;not null;default:false"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
