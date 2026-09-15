package model

import "time"

const (
	SIOPPurposeLogin = "login"
	SIOPPurposeLink  = "link"
)

type UserSIOPIdentity struct {
	ID                  uint64     `json:"id"`
	UserID              uint       `json:"-"`
	DID                 string     `json:"did" gorm:"column:did"`
	Label               string     `json:"label"`
	CreatedAt           time.Time  `json:"created_at"`
	LastAuthenticatedAt *time.Time `json:"last_authenticated_at"`
	LastKID             string     `json:"last_kid" gorm:"column:last_kid"`
}

type AdminSIOPIdentity struct {
	ID                  uint64     `json:"id"`
	AdminID             uint       `json:"-"`
	DID                 string     `json:"did" gorm:"column:did"`
	Label               string     `json:"label"`
	CreatedAt           time.Time  `json:"created_at"`
	LastAuthenticatedAt *time.Time `json:"last_authenticated_at"`
	LastKID             string     `json:"last_kid" gorm:"column:last_kid"`
}

type SIOPChallenge struct {
	ID          string
	Purpose     string
	AccountRole string
	ExpectedDID string `gorm:"column:expected_did"`
	UserID      *uint
	AdminID     *uint
	NonceSHA256 []byte `gorm:"column:nonce_sha256"`
	ExpiresAt   time.Time
	CreatedAt   time.Time
}
