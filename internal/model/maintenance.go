package model

import "time"

// MaintenanceMode is the farm-wide workflow admission switch. The database
// permits exactly one row (ID 1), so the state survives API restarts without
// becoming another per-replica setting.
type MaintenanceMode struct {
	ID               int16      `gorm:"primaryKey" json:"-"`
	Enabled          bool       `json:"enabled"`
	Message          string     `json:"message"`
	EnabledAt        *time.Time `json:"enabled_at,omitempty"`
	EnabledByAdminID *uint      `json:"-"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func (MaintenanceMode) TableName() string { return "maintenance_mode" }

// OperationLock is display metadata for a live shared advisory lock. It is
// deliberately not the authority for admission: PostgreSQL releases advisory
// locks when their connection dies, while this row may survive a crash until
// the next exclusive maintenance check cleans it up.
type OperationLock struct {
	ID            uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	OperationType string    `gorm:"not null"                 json:"operation_type"`
	Target        string    `gorm:"not null;default:''"      json:"target"`
	LockKey       int64     `gorm:"not null"                 json:"-"`
	StartedAt     time.Time `gorm:"not null"                 json:"started_at"`
}
