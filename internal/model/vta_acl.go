package model

import "time"

// VtaAclSnapshot records when the farm last read the complete ACL directly
// from a stopped VTA. MaintenanceStartedAt is also the cross-replica lock for
// every operation that stops one or more components in that session.
type VtaAclSnapshot struct {
	SessionID            uint       `json:"-" gorm:"primaryKey;column:session_id"`
	SyncedAt             *time.Time `json:"synced_at"`
	EntryCount           int        `json:"entry_count"`
	MaintenanceStartedAt *time.Time `json:"-"`
}

func (VtaAclSnapshot) TableName() string { return "vta_acl_snapshots" }

// VtaAclEntry is one row from the most recently synchronized `vta acl list`.
// Text fields intentionally preserve the CLI's rendered values.
type VtaAclEntry struct {
	SessionID    uint   `json:"-" gorm:"primaryKey;column:session_id"`
	Did          string `json:"did" gorm:"primaryKey"`
	Role         string `json:"role"`
	Label        string `json:"label,omitempty"`
	Contexts     string `json:"contexts"`
	AclCreatedAt string `json:"created_at" gorm:"column:acl_created_at"`
}

func (VtaAclEntry) TableName() string { return "vta_acl_entries" }
