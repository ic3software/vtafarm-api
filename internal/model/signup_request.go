package model

import "time"

const (
	SignupRequestPending  = "pending"
	SignupRequestApproved = "approved"
)

// SignupRequest is a visitor's request for an account, made from the public
// home page. An admin reviews it and approves it, which issues an invitation
// link and emails it to the requester. Email is unique — repeated requests
// for the same address are idempotent.
type SignupRequest struct {
	ID           uint       `json:"id"            gorm:"primaryKey;autoIncrement"`
	Email        string     `json:"email"         gorm:"not null;uniqueIndex"`
	Status       string     `json:"status"        gorm:"not null;default:pending"`
	AdminID      *uint      `json:"admin_id"`      // who approved it
	InvitationID *uint      `json:"invitation_id"` // link issued on approval
	EmailSentAt  *time.Time `json:"email_sent_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}
