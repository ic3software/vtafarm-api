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

var (
	ErrConflict   = errors.New("initial connection already claimed or VTA not ready")
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
	err := db.Where("session_id = ?", sessionID).Order("created_at DESC, id DESC").First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &r, err
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
		if current != nil && current.Status == "accepted" {
			result = current
			return nil
		}
		if expectedID != "" && (current == nil || current.ID != expectedID) {
			return ErrConflict
		}
		if action == "cancel" {
			if current == nil {
				return ErrConflict
			}
			if current.Status == "pending" {
				if err := tx.Model(current).Update("status", "cancelled").Error; err != nil {
					return err
				}
				current.Status = "cancelled"
			}
			result = current
			return nil
		}
		if !Ready(s) || s.AdminDid != "" || s.VtaDid == "" {
			return ErrConflict
		}
		var claimed int64
		if err := tx.Model(&model.InitialProvision{}).Where("session_id = ?", s.ID).Count(&claimed).Error; err != nil {
			return err
		}
		if claimed != 0 {
			return ErrConflict
		}
		if current != nil && current.Status == "pending" && now.Before(current.ExpiresAt) && action == "create" {
			result = current
			return nil
		}
		if current != nil && now.Sub(current.CreatedAt) < 5*time.Second {
			return ErrCooldown
		}
		if current != nil && current.Status == "pending" {
			status := "cancelled"
			if !now.Before(current.ExpiresAt) {
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

func recordProvision(tx *gorm.DB, s *model.SetupSession, did string, now time.Time) (*model.InitialProvision, error) {
	op := &model.InitialProvision{SessionID: s.ID, AdminDid: did, CreatedAt: now}
	insert := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(op)
	if insert.Error != nil {
		return nil, insert.Error
	}
	if insert.RowsAffected != 1 {
		return nil, ErrConflict
	}
	status := "provisioning"
	if s.IsFullStack() {
		status = "step_import_admin_did"
	}
	if err := tx.Model(s).Updates(map[string]any{"admin_did": did, "status": status, "updated_at": now}).Error; err != nil {
		return nil, err
	}
	if err := tx.Model(&model.MobileConnection{}).Where("session_id = ? AND status = ?", s.ID, "pending").Update("status", "cancelled").Error; err != nil {
		return nil, err
	}
	return op, nil
}

// AcceptManual also imports pre-existing, interrupted provisioning into the
// durable queue when called by the orchestrator with resume=true.
func AcceptManual(ctx context.Context, db *gorm.DB, sessionID uint, value string, resume bool) (*model.InitialProvision, error) {
	did, err := ValidateAdminDID(value)
	if err != nil {
		return nil, err
	}
	var result *model.InitialProvision
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		s, err := lockSession(tx, sessionID)
		if err != nil {
			return err
		}
		var existing model.InitialProvision
		err = tx.First(&existing, "session_id = ?", sessionID).Error
		if err == nil {
			if existing.AdminDid != did {
				return ErrConflict
			}
			result = &existing
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if s.AdminDid != "" && s.AdminDid != did {
			return ErrConflict
		}
		if !Ready(s) && !(resume && s.AdminDid == did && resumableStatus(s.Status)) {
			return ErrConflict
		}
		now, err := Now(tx)
		if err != nil {
			return err
		}
		result, err = recordProvision(tx, s, did, now)
		return err
	})
	return result, err
}

func resumableStatus(status string) bool {
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
		// A concurrent cancellation/replacement may have changed the request
		// while this transaction was waiting for the VTA lock.
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
			var op model.InitialProvision
			if err := tx.First(&op, "session_id = ?", s.ID).Error; err != nil {
				return err
			}
			if op.AdminDid != did {
				return ErrConflict
			}
			return nil
		}
		if result.Status != "pending" || !now.Before(result.ExpiresAt) {
			return ErrExpired
		}
		if !Ready(s) || s.AdminDid != "" || s.VtaDid != result.VtaDid {
			return ErrConflict
		}
		if _, err := recordProvision(tx, s, did, now); err != nil {
			return err
		}
		result.Status, result.AcceptedAt = "accepted", &now
		return tx.Model(&result).Updates(map[string]any{"status": "accepted", "accepted_at": now}).Error
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
		now, err := Now(tx)
		if err != nil {
			return err
		}
		if r.Status != "accepted" || r.AcceptedAt == nil || !now.Before(r.AcceptedAt.Add(ProgressLifetime)) {
			return ErrExpired
		}
		if s.Status != "running" {
			return ErrConflict
		}
		return tx.Model(&r).Where("connected_at IS NULL").Update("connected_at", now).Error
	})
}

// Stop fences initial provisioning before teardown, including callbacks that
// were waiting for the VTA lock when deletion began. Keep the finished marker
// even when no Admin DID has been accepted yet.
func Stop(ctx context.Context, db *gorm.DB, sessionID uint) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		s, err := lockSession(tx, sessionID)
		if err != nil {
			return err
		}
		now, err := Now(tx)
		if err != nil {
			return err
		}
		op := model.InitialProvision{SessionID: sessionID, AdminDid: s.AdminDid, CreatedAt: now, FinishedAt: &now}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "session_id"}}, DoUpdates: clause.Assignments(map[string]any{"finished_at": now})}).Create(&op).Error; err != nil {
			return err
		}
		return tx.Model(&model.MobileConnection{}).Where("session_id = ? AND status = ?", sessionID, "pending").Update("status", "cancelled").Error
	})
}
