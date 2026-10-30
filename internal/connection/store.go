package connection

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/didresolver"
	"github.com/ic3software/vtafarm-api/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const QRLifetime = 5 * time.Minute
const ConfirmationLifetime = 5 * time.Minute
const MobileAdminLabel = "mobile integration"

const (
	OperationProvisionVTA = "provision_vta"
	OperationGrantACL     = "grant_acl"
)

var (
	ErrConflict       = errors.New("connection already claimed or VTA not ready")
	ErrExpired        = errors.New("connection request expired; scan a new QR code")
	ErrInvalidDID     = errors.New("administrator must be a valid Ed25519 did:key")
	ErrCooldown       = errors.New("wait a few seconds before generating another QR code")
	ErrAlreadyClaimed = errors.New("connection handle has already been claimed")
)

func ValidateAdminDID(value string) (string, error) {
	did := strings.TrimSpace(value)
	if len(did) > 128 || !strings.HasPrefix(did, "did:key:z") {
		return "", ErrInvalidDID
	}
	_, err := (didresolver.DIDKeyResolver{}).ResolveAuthenticationKey(context.Background(), did, did+"#"+strings.TrimPrefix(did, "did:key:"))
	if err != nil {
		return "", ErrInvalidDID
	}
	return did, nil
}

func ClaimCheck(did string) string {
	digest := sha256.Sum256([]byte(did))
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:])[:6]
}

func ClaimantDigest(did string) string {
	digest := sha256.Sum256([]byte(did))
	return fmt.Sprintf("%x", digest[:])
}

// AcquireRedeemPoll permits one held redeem request for a reservation across
// all API replicas. Expired leases are reclaimed by the next poller.
func AcquireRedeemPoll(ctx context.Context, db *gorm.DB, requestID, pollID string, lease time.Duration) (bool, error) {
	if db == nil || requestID == "" || pollID == "" || lease <= 0 {
		return false, errors.New("invalid redeem poll lease")
	}
	now, err := Now(db.WithContext(ctx))
	if err != nil {
		return false, err
	}
	result := db.WithContext(ctx).Model(&model.MobileConnection{}).
		Where("id = ? AND status IN ? AND (redeem_poll_until IS NULL OR redeem_poll_until <= ?)", requestID, []string{"claimed", "confirmed", "provisioning"}, now).
		Updates(map[string]any{"redeem_poll_id": pollID, "redeem_poll_until": now.Add(lease)})
	return result.RowsAffected == 1, result.Error
}

func ReleaseRedeemPoll(ctx context.Context, db *gorm.DB, requestID, pollID string) error {
	return db.WithContext(ctx).Model(&model.MobileConnection{}).
		Where("id = ? AND redeem_poll_id = ?", requestID, pollID).
		Updates(map[string]any{"redeem_poll_id": nil, "redeem_poll_until": nil}).Error
}

func newHandle() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func Ready(s *model.SetupSession) bool {
	if s.IsFullStack() {
		return s.Status == "awaiting_admin_did"
	}
	return s.Status == "vta_setup_complete" || s.Status == model.StatusPoolWaiting
}

func lockSession(tx *gorm.DB, id uint) (*model.SetupSession, error) {
	var s model.SetupSession
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&s, id).Error
	return &s, err
}

// Now is intentionally read after locks are acquired so lock contention cannot
// extend a reservation or confirmation deadline.
func Now(tx *gorm.DB) (time.Time, error) {
	var now time.Time
	err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error
	return now, err
}

func Current(db *gorm.DB, sessionID uint) (*model.MobileConnection, error) {
	var r model.MobileConnection
	result := db.Where("session_id = ?", sessionID).Order("created_at DESC, id DESC").Limit(1).Find(&r)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &r, nil
}

func operationFor(s *model.SetupSession, vtaDID string) (string, error) {
	switch {
	case Ready(s) && s.AdminDid == "" && s.VtaDid == vtaDID:
		return OperationProvisionVTA, nil
	case s.Status == "running" && s.VtaDid == vtaDID:
		return OperationGrantACL, nil
	default:
		return "", ErrConflict
	}
}

func finalStatus(status string) bool {
	switch status {
	case "released", "rejected", "lapsed", "cancelled", "expired", "failed":
		return true
	default:
		return false
	}
}

// Change creates, refreshes, or cancels an owner-visible reservation. It always
// locks the setup session first, matching claim/confirm/provision transitions.
func Change(ctx context.Context, db *gorm.DB, sessionID uint, action, expectedID string) (*model.MobileConnection, error) {
	return change(ctx, db, sessionID, action, expectedID, false)
}

// ChangePool is called inside the pool allocation transaction, after the slot
// is reserved. Public owner routes cannot allocate prebuilt pool inventory.
func ChangePool(ctx context.Context, db *gorm.DB, sessionID uint) (*model.MobileConnection, error) {
	return change(ctx, db, sessionID, "create", "", true)
}

func change(ctx context.Context, db *gorm.DB, sessionID uint, action, expectedID string, pooled bool) (*model.MobileConnection, error) {
	var result *model.MobileConnection
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		s, err := lockSession(tx, sessionID)
		if err != nil {
			return err
		}
		now, err := Now(tx)
		if err != nil {
			return err
		}
		current, err := Current(tx, sessionID)
		if err != nil {
			return err
		}
		if expectedID != "" && (current == nil || current.ID != expectedID) {
			return ErrConflict
		}

		if action == "cancel" {
			if current == nil || current.Status != "open" {
				return ErrConflict
			}
			if err := tx.Model(current).Updates(map[string]any{"status": "lapsed", "admin_did": ""}).Error; err != nil {
				return err
			}
			current.Status = "lapsed"
			result = current
			return nil
		}

		operation, err := operationFor(s, s.VtaDid)
		if err != nil {
			return err
		}
		isPool := s.Status == model.StatusPoolWaiting
		if isPool != pooled || isPool && operation != OperationProvisionVTA {
			return ErrConflict
		}
		if current != nil {
			switch current.Status {
			case "claimed", "confirmed", "provisioning":
				return ErrConflict
			case "open":
				if now.Before(current.ExpiresAt) && action == "create" {
					result = current
					return nil
				}
				if now.Sub(current.CreatedAt) < 5*time.Second {
					return ErrCooldown
				}
				if err := tx.Model(current).Update("status", "lapsed").Error; err != nil {
					return err
				}
			default:
				if !finalStatus(current.Status) {
					return ErrConflict
				}
				if now.Sub(current.CreatedAt) < 5*time.Second {
					return ErrCooldown
				}
			}
		}

		handle, err := newHandle()
		if err != nil {
			return err
		}
		result = &model.MobileConnection{
			ID:          uuid.NewString(),
			SessionID:   sessionID,
			VtaDid:      s.VtaDid,
			Operation:   operation,
			Status:      "open",
			ClaimHandle: &handle,
			CreatedAt:   now,
			ExpiresAt:   now.Add(QRLifetime),
		}
		return tx.Create(result).Error
	})
	return result, err
}

func beginProvision(tx *gorm.DB, s *model.SetupSession, did string, now time.Time) error {
	status := "provisioning"
	if s.IsFullStack() {
		status = "step_import_admin_did"
	}
	return tx.Model(s).Updates(map[string]any{"admin_did": did, "status": status, "updated_at": now}).Error
}

// Claim reserves a handle for the proof-bound administrator key. It deliberately
// does not mutate the setup session or start provisioning.
func Claim(ctx context.Context, db *gorm.DB, handle, value string) (*model.MobileConnection, error) {
	return claim(ctx, db, handle, value, "", nil)
}

// ClaimAndRecord commits the consequential reservation transition and its
// already-signed reply in the same transaction as the replay execution row.
func ClaimAndRecord(ctx context.Context, db *gorm.DB, handle, value, documentID string, response []byte) (*model.MobileConnection, error) {
	if documentID == "" || len(response) == 0 {
		return nil, errors.New("claim replay response is required")
	}
	return claim(ctx, db, handle, value, documentID, response)
}

func claim(ctx context.Context, db *gorm.DB, handle, value, documentID string, response []byte) (*model.MobileConnection, error) {
	did, err := ValidateAdminDID(value)
	if err != nil {
		return nil, err
	}
	var result model.MobileConnection
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidate model.MobileConnection
		if err := tx.Select("session_id").First(&candidate, "claim_handle = ?", handle).Error; err != nil {
			return err
		}
		s, err := lockSession(tx, candidate.SessionID)
		if err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&result, "claim_handle = ?", handle).Error; err != nil {
			return err
		}
		now, err := Now(tx)
		if err != nil {
			return err
		}
		if result.Status != "open" {
			if result.Status == "claimed" || result.Status == "confirmed" || result.Status == "provisioning" || result.Status == "released" {
				return ErrAlreadyClaimed
			}
			return ErrExpired
		}
		if !now.Before(result.ExpiresAt) {
			if err := tx.Model(&result).Update("status", "lapsed").Error; err != nil {
				return err
			}
			return ErrExpired
		}
		operation, err := operationFor(s, result.VtaDid)
		if err != nil {
			return err
		}
		confirmBy := now.Add(ConfirmationLifetime)
		claimantDigest := ClaimantDigest(did)
		if err := tx.Model(&result).Updates(map[string]any{
			"status":          "claimed",
			"operation":       operation,
			"admin_did":       did,
			"claimant_digest": claimantDigest,
			"claimed_at":      now,
			"confirm_by":      confirmBy,
			"confirmed_at":    nil,
		}).Error; err != nil {
			return err
		}
		result.Status, result.Operation, result.AdminDid = "claimed", operation, did
		result.ClaimantDigest = &claimantDigest
		result.ClaimedAt, result.ConfirmBy = &now, &confirmBy
		if documentID != "" {
			updated := tx.Model(&model.TrustTaskExecution{}).
				Where("document_id = ? AND state = ?", documentID, "executing").
				Updates(map[string]any{"state": "completed", "response_bytes": append([]byte(nil), response...), "updated_at": now})
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return errors.New("claim replay execution is not active")
			}
		}
		return nil
	})
	return &result, err
}

func Confirm(ctx context.Context, db *gorm.DB, sessionID uint, requestID string) (*model.MobileConnection, error) {
	var result model.MobileConnection
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockSession(tx, sessionID); err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&result, "id = ? AND session_id = ?", requestID, sessionID).Error; err != nil {
			return err
		}
		now, err := Now(tx)
		if err != nil {
			return err
		}
		if result.Status != "claimed" || result.ConfirmBy == nil || !now.Before(*result.ConfirmBy) {
			if result.Status == "claimed" {
				if err := tx.Model(&result).Updates(map[string]any{"status": "lapsed", "admin_did": ""}).Error; err != nil {
					return err
				}
			}
			return ErrExpired
		}
		if err := tx.Model(&result).Updates(map[string]any{"status": "confirmed", "confirmed_at": now}).Error; err != nil {
			return err
		}
		result.Status, result.ConfirmedAt = "confirmed", &now
		return nil
	})
	return &result, err
}

func Reject(ctx context.Context, db *gorm.DB, sessionID uint, requestID string) (*model.MobileConnection, error) {
	var result model.MobileConnection
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockSession(tx, sessionID); err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&result, "id = ? AND session_id = ?", requestID, sessionID).Error; err != nil {
			return err
		}
		if result.Status != "claimed" {
			return ErrConflict
		}
		now, err := Now(tx)
		if err != nil {
			return err
		}
		if result.ConfirmBy == nil || !now.Before(*result.ConfirmBy) {
			if err := tx.Model(&result).Updates(map[string]any{"status": "lapsed", "admin_did": ""}).Error; err != nil {
				return err
			}
			return ErrExpired
		}
		if err := tx.Model(&result).Updates(map[string]any{"status": "rejected", "admin_did": ""}).Error; err != nil {
			return err
		}
		result.Status, result.AdminDid = "rejected", ""
		return nil
	})
	return &result, err
}

// StartProvisioning is called only after Confirm commits. It is idempotent so a
// recovery loop can safely resume confirmed reservations after a restart.
func StartProvisioning(ctx context.Context, db *gorm.DB, requestID string) (*model.MobileConnection, error) {
	var result model.MobileConnection
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidate model.MobileConnection
		if err := tx.Select("session_id").First(&candidate, "id = ?", requestID).Error; err != nil {
			return err
		}
		s, err := lockSession(tx, candidate.SessionID)
		if err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&result, "id = ?", requestID).Error; err != nil {
			return err
		}
		if result.Status == "provisioning" || result.Status == "released" {
			return nil
		}
		if result.Status != "confirmed" {
			return ErrConflict
		}
		now, err := Now(tx)
		if err != nil {
			return err
		}
		switch result.Operation {
		case OperationProvisionVTA:
			if s.Status == model.StatusPoolWaiting && result.ConfirmedAt == nil {
				return ErrConflict
			}
			if err := beginProvision(tx, s, result.AdminDid, now); err != nil {
				return err
			}
		case OperationGrantACL:
			if s.Status != "running" || s.VtaDid != result.VtaDid {
				return ErrConflict
			}
		default:
			return ErrConflict
		}
		if err := tx.Model(&result).Update("status", "provisioning").Error; err != nil {
			return err
		}
		result.Status = "provisioning"
		return nil
	})
	return &result, err
}

func MarkReleased(ctx context.Context, db *gorm.DB, requestID string) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r model.MobileConnection
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, "id = ?", requestID).Error; err != nil {
			return err
		}
		if r.Status == "released" {
			return nil
		}
		if r.Status != "provisioning" {
			return ErrConflict
		}
		now, err := Now(tx)
		if err != nil {
			return err
		}
		if err := tx.Model(&r).Updates(map[string]any{"status": "released", "released_at": now}).Error; err != nil {
			return err
		}
		if r.Operation != OperationProvisionVTA || r.AdminDid == "" || r.ClaimantDigest == nil || ClaimantDigest(r.AdminDid) != *r.ClaimantDigest {
			return nil
		}
		var session model.SetupSession
		if err := tx.First(&session, r.SessionID).Error; err != nil {
			return err
		}
		binding := model.WalletLoginBinding{
			UserID:          session.UserID,
			LoginDID:        r.AdminDid,
			SourceSessionID: session.ID,
			SourceClaimID:   r.ID,
			CreatedAt:       now,
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&binding).Error
	})
}

// MarkFailed ends an additional-administrator request whose grant did not
// complete. The status must be final: anything else keeps the request active,
// which blocks every new QR code for the session and has the recovery queue
// retry the grant, restarting the VTA each time.
func MarkFailed(ctx context.Context, db *gorm.DB, requestID, reason string) error {
	return db.WithContext(ctx).Model(&model.MobileConnection{}).
		Where("id = ? AND operation = ? AND status IN ?", requestID, OperationGrantACL, []string{"confirmed", "provisioning"}).
		Updates(map[string]any{"status": "failed", "provision_error": reason}).Error
}

// Reconcile expires unconfirmed reservations and marks initial provisioning as
// released once the existing setup pipeline reports the VTA running.
func Reconcile(ctx context.Context, db *gorm.DB, r *model.MobileConnection, s *model.SetupSession) error {
	if r == nil {
		return nil
	}
	now, err := Now(db.WithContext(ctx))
	if err != nil {
		return err
	}
	if r.Status == "open" && !now.Before(r.ExpiresAt) {
		updated := db.WithContext(ctx).Model(r).Where("status = ?", "open").Update("status", "lapsed")
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected == 1 {
			r.Status = "lapsed"
		}
	}
	if r.Status == "claimed" && r.ConfirmBy != nil && !now.Before(*r.ConfirmBy) {
		updated := db.WithContext(ctx).Model(r).Where("status = ?", "claimed").Updates(map[string]any{"status": "lapsed", "admin_did": ""})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected == 1 {
			r.Status, r.AdminDid = "lapsed", ""
		}
	}
	if r.Status == "provisioning" && r.Operation == OperationProvisionVTA && s.Status == "running" && s.AdminDid == r.AdminDid && s.VtaDid == r.VtaDid {
		if err := MarkReleased(ctx, db, r.ID); err != nil {
			return err
		}
		r.Status, r.ReleasedAt = "released", &now
	}
	return nil
}

// AcceptManual claims the setup gate directly. It cannot race past an active
// wallet claim awaiting owner confirmation.
func AcceptManual(ctx context.Context, db *gorm.DB, sessionID uint, value string, resume bool) error {
	did, err := ValidateAdminDID(value)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		s, err := lockSession(tx, sessionID)
		if err != nil {
			return err
		}
		if s.Status == model.StatusPoolWaiting {
			return ErrConflict
		}
		current, err := Current(tx, sessionID)
		if err != nil {
			return err
		}
		if current != nil && (current.Status == "claimed" || current.Status == "confirmed" || current.Status == "provisioning") {
			return ErrConflict
		}
		if s.AdminDid != "" {
			if s.AdminDid == did && resume && ResumableProvisionStatus(s.Status) {
				return nil
			}
			return ErrConflict
		}
		if !Ready(s) {
			return ErrConflict
		}
		now, err := Now(tx)
		if err != nil {
			return err
		}
		if err := beginProvision(tx, s, did, now); err != nil {
			return err
		}
		return tx.Model(&model.MobileConnection{}).Where("session_id = ? AND status = ?", s.ID, "open").Update("status", "lapsed").Error
	})
}

func CancelPending(ctx context.Context, db *gorm.DB, sessionID uint) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockSession(tx, sessionID); err != nil {
			return err
		}
		return tx.Model(&model.MobileConnection{}).Where("session_id = ? AND status = ?", sessionID, "open").Update("status", "lapsed").Error
	})
}

func ResumableProvisionStatus(status string) bool {
	switch status {
	case "provisioning", "step_import_admin_did", "deploy_vta", "step_vtc_setup_key", "step_vtc_acl_grant", "step_vtc_setup", "deploy_vtc":
		return true
	}
	return false
}

func Stop(ctx context.Context, db *gorm.DB, sessionID uint) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		s, err := lockSession(tx, sessionID)
		if err != nil {
			return err
		}
		if err := tx.Model(s).Update("status", "deleting").Error; err != nil {
			return err
		}
		if err := tx.Model(&model.MobileConnection{}).
			Where("session_id = ? AND status IN ?", sessionID, []string{"open", "claimed"}).
			Updates(map[string]any{"status": "lapsed", "admin_did": ""}).Error; err != nil {
			return err
		}
		return tx.Model(&model.MobileConnection{}).
			Where("session_id = ? AND status IN ?", sessionID, []string{"confirmed", "provisioning"}).
			Updates(map[string]any{"status": "cancelled", "admin_did": ""}).Error
	})
}
