package connection

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/siop"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const QRLifetime = 5 * time.Minute
const RetryLifetime = 24 * time.Hour
const ProgressLifetime = time.Hour

// Operation describes the server-side work required after a phone claims a
// request. It is selected from the locked session state, never by the client.
const (
	OperationProvisionVTA = "provision_vta"
	OperationGrantACL     = "grant_acl"
)

var (
	ErrConflict   = errors.New("connection already claimed or VTA not ready")
	ErrExpired    = errors.New("connection request expired or cancelled; scan a new QR code")
	ErrInvalidDID = errors.New("admin_did must be a valid Ed25519 did:key")
	ErrCooldown   = errors.New("wait a few seconds before generating another QR code")
)

func ValidateAdminDID(value string) (string, error) {
	did := strings.TrimSpace(value)
	if len(did) > 128 || !strings.HasPrefix(did, "did:key:z") {
		return "", ErrInvalidDID
	}
	_, err := (siop.DIDKeyResolver{}).ResolveAuthenticationKey(context.Background(), did, did+"#"+strings.TrimPrefix(did, "did:key:"))
	if err != nil {
		return "", ErrInvalidDID
	}
	return did, nil
}

func Ready(s *model.SetupSession) bool {
	if s.IsFullStack() {
		return s.Status == "awaiting_admin_did"
	}
	return s.Status == "vta_setup_complete"
}

func lockSession(tx *gorm.DB, id uint) (*model.SetupSession, error) {
	var s model.SetupSession
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&s, id).Error
	return &s, err
}

// Read the database clock after acquiring the lock: waiting for a competing
// transaction must not extend the five-minute acceptance window.
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

func connectionReady(r *model.MobileConnection, s *model.SetupSession) bool {
	switch r.Operation {
	case OperationProvisionVTA:
		return s.Status == "running"
	case OperationGrantACL:
		return s.Status == "running" && r.ProvisionedAt != nil
	default:
		return false
	}
}

// Change always locks the VTA first, as do acceptance and manual provisioning.
// expectedID prevents an old tab from cancelling or replacing a newer request.
func Change(ctx context.Context, db *gorm.DB, sessionID uint, action, expectedID string) (*model.MobileConnection, error) {
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
		revocableAccepted := false
		if current != nil && current.Status == "accepted" {
			switch {
			case current.ConnectedAt != nil:
				if action == "cancel" {
					result = current
					return nil
				}
			case !connectionReady(current, s) || action == "create":
				result = current
				return nil
			default:
				revocableAccepted = true
			}
		}
		if expectedID != "" && (current == nil || current.ID != expectedID) {
			return ErrConflict
		}
		if action == "cancel" {
			if current == nil {
				return ErrConflict
			}
			if current.Status == "pending" || revocableAccepted {
				if err := tx.Model(current).Update("status", "cancelled").Error; err != nil {
					return err
				}
				current.Status = "cancelled"
			}
			result = current
			return nil
		}
		if ((!Ready(s) || s.AdminDid != "") && s.Status != "running") || s.VtaDid == "" {
			return ErrConflict
		}
		if current != nil && current.Status == "pending" && now.Before(current.ExpiresAt) && action == "create" {
			result = current
			return nil
		}
		if current != nil && current.ConnectedAt == nil && !revocableAccepted && now.Sub(current.CreatedAt) < 5*time.Second {
			return ErrCooldown
		}
		if current != nil && (current.Status == "pending" || revocableAccepted) {
			status := "cancelled"
			if current.Status == "pending" && !now.Before(current.ExpiresAt) {
				status = "expired"
			}
			if err := tx.Model(current).Update("status", status).Error; err != nil {
				return err
			}
		}
		result = &model.MobileConnection{ID: uuid.NewString(), SessionID: sessionID, VtaDid: s.VtaDid, Status: "pending", CreatedAt: now, ExpiresAt: now.Add(QRLifetime)}
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

// AcceptManual claims the setup gate directly. The setup session is the
// durable provisioning record; its status and AdminDid recover interrupted work.
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
		return tx.Model(&model.MobileConnection{}).Where("session_id = ? AND status = ?", s.ID, "pending").Update("status", "cancelled").Error
	})
}

func CancelPending(ctx context.Context, db *gorm.DB, sessionID uint) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockSession(tx, sessionID); err != nil {
			return err
		}
		return tx.Model(&model.MobileConnection{}).Where("session_id = ? AND status = ?", sessionID, "pending").Update("status", "cancelled").Error
	})
}

func ResumableProvisionStatus(status string) bool {
	switch status {
	case "provisioning", "step_import_admin_did", "deploy_vta", "step_vtc_setup_key", "step_vtc_acl_grant", "step_vtc_setup", "deploy_vtc":
		return true
	}
	return false
}

func AcceptMobile(ctx context.Context, db *gorm.DB, requestID, value string) (*model.MobileConnection, error) {
	did, err := ValidateAdminDID(value)
	if err != nil {
		return nil, err
	}
	var result model.MobileConnection
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&result, "id = ?", requestID).Error; err != nil {
			return err
		}
		s, err := lockSession(tx, result.SessionID)
		if err != nil {
			return err
		}
		if err := tx.First(&result, "id = ?", requestID).Error; err != nil {
			return err
		}
		now, err := Now(tx)
		if err != nil {
			return err
		}
		if result.Status == "accepted" {
			if result.AcceptedAt == nil || !now.Before(result.AcceptedAt.Add(RetryLifetime)) {
				return ErrExpired
			}
			if result.AdminDid != did {
				return ErrConflict
			}
			return nil
		}
		if result.Status != "pending" || !now.Before(result.ExpiresAt) {
			return ErrExpired
		}
		operation := ""
		switch {
		case Ready(s) && s.AdminDid == "" && s.VtaDid == result.VtaDid:
			operation = OperationProvisionVTA
			if err := beginProvision(tx, s, did, now); err != nil {
				return err
			}
		case s.Status == "running" && s.VtaDid == result.VtaDid:
			operation = OperationGrantACL
		default:
			return ErrConflict
		}
		result.Status, result.Operation, result.AdminDid, result.AcceptedAt = "accepted", operation, did, &now
		return tx.Model(&result).Updates(map[string]any{"status": "accepted", "operation": operation, "admin_did": did, "accepted_at": now}).Error
	})
	return &result, err
}

func Complete(ctx context.Context, db *gorm.DB, requestID string) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r model.MobileConnection
		if err := tx.First(&r, "id = ?", requestID).Error; err != nil {
			return err
		}
		s, err := lockSession(tx, r.SessionID)
		if err != nil {
			return err
		}
		if err := tx.First(&r, "id = ?", requestID).Error; err != nil {
			return err
		}
		now, err := Now(tx)
		if err != nil {
			return err
		}
		if r.Status != "accepted" || r.AcceptedAt == nil || !now.Before(r.AcceptedAt.Add(ProgressLifetime)) {
			return ErrExpired
		}
		if !connectionReady(&r, s) {
			return ErrConflict
		}
		return tx.Model(&r).Where("connected_at IS NULL").Update("connected_at", now).Error
	})
}

// Stop fences provisioning before teardown, including callbacks waiting for
// the VTA lock. The deleting state also stops workers on other replicas.
func Stop(ctx context.Context, db *gorm.DB, sessionID uint) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		s, err := lockSession(tx, sessionID)
		if err != nil {
			return err
		}
		if err := tx.Model(s).Update("status", "deleting").Error; err != nil {
			return err
		}
		return tx.Model(&model.MobileConnection{}).
			Where("session_id = ? AND (status = ? OR (status = ? AND connected_at IS NULL))", sessionID, "pending", "accepted").
			Update("status", "cancelled").Error
	})
}
