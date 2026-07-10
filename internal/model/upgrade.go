package model

import "time"

const (
	UpgradeBatchRunning   = "running"
	UpgradeBatchPaused    = "paused" // fail-fast: set on the first task failure
	UpgradeBatchCompleted = "completed"
	UpgradeBatchCancelled = "cancelled"

	UpgradeTaskPending   = "pending"
	UpgradeTaskRunning   = "running"
	UpgradeTaskSucceeded = "succeeded"
	UpgradeTaskFailed    = "failed"
	UpgradeTaskSkipped   = "skipped"
)

// UpgradeComponents are the deployable components an admin can upgrade —
// the same set GET /setup/images serves tags for.
var UpgradeComponents = []string{"vta", "mediator", "dids", "vtc"}

// UpgradeBatch is one admin-triggered image rollout over a set of sessions.
// The background runner (internal/upgrade) processes its tasks with at most
// Concurrency in flight; on the first failure the batch flips to paused so a
// bad image never marches through the whole fleet.
type UpgradeBatch struct {
	ID          uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	AdminID     uint      `gorm:"not null"                 json:"-"`
	Component   string    `gorm:"not null"                 json:"component"`
	Image       string    `gorm:"not null"                 json:"image"`
	Concurrency int       `gorm:"not null;default:3"       json:"concurrency"`
	Status      string    `gorm:"not null;default:running" json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// UpgradeTask is one session's upgrade within a batch. FromImage keeps the
// pre-upgrade image so a failed or regretted upgrade can be reverted by
// creating a new batch back to that image.
type UpgradeTask struct {
	ID        uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	BatchID   uint      `gorm:"not null;index"           json:"-"`
	SessionID uint      `gorm:"not null"                 json:"-"`
	FromImage string    `gorm:"not null;default:''"      json:"from_image"`
	Status    string    `gorm:"not null;default:pending" json:"status"`
	ErrorMsg  string    `gorm:"not null;default:''"      json:"error_msg,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UpgradeComponentModes maps each component to the session modes that run it.
// A session is only a valid upgrade target for components its mode deploys.
var UpgradeComponentModes = map[string][]string{
	"vta":      {ModeVtaOnly, ModeFullStack, ModeFullStackWithVtc},
	"mediator": {ModeFullStack, ModeFullStackWithVtc},
	"dids":     {ModeFullStack, ModeFullStackWithVtc},
	"vtc":      {ModeFullStackWithVtc},
}

// UpgradeImageColumn returns the setup_sessions column holding the given
// component's image, or "" for an unknown component.
func UpgradeImageColumn(component string) string {
	switch component {
	case "vta":
		return "vta_image"
	case "mediator":
		return "mediator_image"
	case "dids":
		return "dids_image"
	case "vtc":
		return "vtc_image"
	}
	return ""
}

// ComponentImage returns the session's current image for component.
func (s *SetupSession) ComponentImage(component string) string {
	switch component {
	case "vta":
		return s.VtaImage
	case "mediator":
		return s.MediatorImage
	case "dids":
		return s.DidsImage
	case "vtc":
		return s.VtcImage
	}
	return ""
}
