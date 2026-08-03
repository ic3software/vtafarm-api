package model

import "time"

// The lifecycle of one grant. Written pending before any Kubernetes work
// starts, so a client that times out during the maintenance window has not
// lost the operation — the row is the record, the HTTP response is not. A live
// pending row is also how a second API replica knows to refuse a concurrent
// grant, which the in-process lock cannot see across pods.
//
// There is deliberately no revoked state: removing an admin is `pnm acl delete`
// against the live VTA, not something this side does.
const (
	GrantPending = "pending"
	GrantGranted = "granted"
	GrantFailed  = "failed"
)

// VtaAdminGrant is one attempt by this farm to put a DID in a stack's VTA ACL
// as an unrestricted admin — the same authority `step_import_admin_did` gives
// the stack's first admin (docs/platform-stack-admin-grant-design.md §2).
//
// There is no role or contexts field: every grant is unrestricted admin, which
// is the whole feature. See the migration for why that is a deliberate absence
// rather than a default.
//
// **A row is an event, not a permission.** The DID here is the temporary
// did:key `pnm setup` minted, and PNM rotates off it on first connect, so this
// value goes stale by design (§7.2). Nothing on this side tracks where the entry
// moved to — `pnm acl list` against the running VTA is what answers who can act
// on it now.
type VtaAdminGrant struct {
	ID        uint `json:"-"                      gorm:"primaryKey;autoIncrement"`
	SessionID uint `json:"-"                      gorm:"column:session_id;not null;index"`

	Did   string `json:"did"   gorm:"not null"`
	Label string `json:"label" gorm:"not null;default:''"`

	Status   string `json:"status"              gorm:"not null;default:pending"`
	ErrorMsg string `json:"error_msg,omitempty" gorm:"not null;default:''"`

	// RequestedBy is an admins.id (the admin cookie's JWT carries it as
	// UserID — admins are their own table). Nullable so the record outlives
	// the admin who asked for it.
	RequestedBy *uint      `json:"-"                     gorm:"column:requested_by"`
	GrantedAt   *time.Time `json:"granted_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (VtaAdminGrant) TableName() string { return "vta_admin_grants" }
