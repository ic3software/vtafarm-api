package model

import "time"

const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

type User struct {
	ID       uint   `json:"id"         gorm:"primaryKey;autoIncrement"`
	UniqueId string `json:"unique_id"  gorm:"column:unique_id;not null"`

	// BetaAccess gates access to features still in beta (currently: full_stack
	// setup mode). A plain on/off switch, not a tier — if a second beta feature
	// ever needs independent control, add another column then rather than
	// building a generic flag system now.
	BetaAccess bool `json:"beta_access" gorm:"column:beta_access;not null;default:false"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
