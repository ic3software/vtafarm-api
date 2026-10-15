package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"

	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrConflict = errors.New("pool state changed or session is protected")
	ErrEmpty    = errors.New("no waiting VTA is available in this batch")
	ErrCapacity = errors.New("pool inventory limit reached")
)

type CapacityError struct {
	Available int
}

func (e *CapacityError) Error() string {
	return ErrCapacity.Error()
}

func (e *CapacityError) Unwrap() error {
	return ErrCapacity
}

// All pool mutations use one transaction lock so two operators cannot select
// the same waiting session.
func lock(tx *gorm.DB) error {
	return tx.Exec("SELECT pg_advisory_xact_lock(565443, 1)").Error
}

type Admission struct {
	AdminID            uint
	Key, Digest, Label string
	Count              int
	Available          func(context.Context) (int, error)
	Session            model.SetupSession
	MemoryRequest      string
	MemoryLimit        string
}

func LookupBatch(ctx context.Context, db *gorm.DB, adminID uint, key, digest string) (*model.PoolBatch, error) {
	var batch model.PoolBatch
	err := db.WithContext(ctx).Where("created_by = ? AND idempotency_key = ?", adminID, key).First(&batch).Error
	if err == nil && batch.RequestDigest != digest {
		return nil, ErrConflict
	}
	return &batch, err
}

// Admit creates real users and setup sessions. pool_sessions only groups those
// sessions; it does not duplicate their ownership, infrastructure, or status.
func Admit(ctx context.Context, db *gorm.DB, input Admission) (*model.PoolBatch, bool, error) {
	if input.Count < 1 || input.Key == "" || len(input.Key) > 128 || input.Session.VtaImage == "" || input.Available == nil {
		return nil, false, errors.New("invalid pool admission")
	}
	var batch *model.PoolBatch
	created := false
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lock(tx); err != nil {
			return err
		}
		previous, err := LookupBatch(ctx, tx, input.AdminID, input.Key, input.Digest)
		if err == nil {
			batch = previous
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		available, err := input.Available(ctx)
		if err != nil {
			return err
		}
		if input.Count > available {
			return &CapacityError{Available: max(available, 0)}
		}
		batch = &model.PoolBatch{
			CreatedBy: input.AdminID, Label: input.Label, RequestedCount: input.Count,
			IdempotencyKey: input.Key, RequestDigest: input.Digest,
		}
		if err := tx.Create(batch).Error; err != nil {
			return err
		}
		for range input.Count {
			user, err := createUser(tx)
			if err != nil {
				return err
			}
			name, err := randomName()
			if err != nil {
				return err
			}
			session := input.Session
			session.ID, session.UserID, session.Status = 0, user.ID, model.StatusPoolCreating
			session.VtaName, session.AdminDid = name, ""
			session.Subdomain = input.Session.Subdomain + name
			session.VtaDidUrl = session.DidHostingServerURL + "/" + name + "-vta"
			if err := tx.Create(&session).Error; err != nil {
				return err
			}
			if err := tx.Create(&model.PoolSession{BatchID: batch.ID, SessionID: session.ID}).Error; err != nil {
				return err
			}
			resource := model.WorkloadResource{
				SetupSessionID: session.ID, Component: "vta",
				MemoryRequest: input.MemoryRequest, MemoryLimit: input.MemoryLimit,
			}
			if err := tx.Create(&resource).Error; err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	return batch, created, err
}

func createUser(tx *gorm.DB) (*model.User, error) {
	for range 5 {
		name, err := randomName()
		if err != nil {
			return nil, err
		}
		user := &model.User{UniqueId: name}
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "unique_id"}}, DoNothing: true}).Create(user)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 1 {
			return user, nil
		}
	}
	return nil, errors.New("could not allocate user identifier")
}

func randomName() (string, error) {
	var value [6]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

// Reserve chooses one waiting session and creates its ordinary mobile claim.
// The QR lifecycle remains entirely in mobile_connections.
func Reserve(ctx context.Context, db *gorm.DB, batchID uint) (*model.MobileConnection, error) {
	var result *model.MobileConnection
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lock(tx); err != nil {
			return err
		}
		var candidates []model.SetupSession
		if err := tx.Table("setup_sessions s").
			Select("s.*").
			Joins("JOIN pool_sessions ps ON ps.session_id = s.id").
			Where("ps.batch_id = ? AND s.status = ? AND s.admin_did = ''", batchID, model.StatusPoolWaiting).
			Where(`NOT EXISTS (
				SELECT 1 FROM mobile_connections mc
				WHERE mc.session_id = s.id
				  AND (
					(mc.status = 'open' AND mc.expires_at > clock_timestamp())
					OR (mc.status = 'claimed' AND mc.confirm_by > clock_timestamp())
					OR mc.status IN ('confirmed', 'provisioning')
				  )
			)`).
			Order("s.id").Find(&candidates).Error; err != nil {
			return err
		}
		for i := range candidates {
			var session model.SetupSession
			query := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).First(&session, candidates[i].ID)
			if query.Error != nil {
				if errors.Is(query.Error, gorm.ErrRecordNotFound) {
					continue
				}
				return query.Error
			}
			if session.Status != model.StatusPoolWaiting || session.AdminDid != "" {
				continue
			}
			mobile, err := connection.ChangePool(ctx, tx, session.ID)
			if err != nil {
				if errors.Is(err, connection.ErrConflict) || errors.Is(err, connection.ErrCooldown) {
					continue
				}
				return err
			}
			result = mobile
			return nil
		}
		return ErrEmpty
	})
	return result, err
}

func Confirm(ctx context.Context, db *gorm.DB, requestID string) (*model.MobileConnection, error) {
	var result *model.MobileConnection
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lock(tx); err != nil {
			return err
		}
		var candidate model.MobileConnection
		if err := tx.Select("session_id").First(&candidate, "id = ?", requestID).Error; err != nil {
			return err
		}
		if !IsSession(tx, candidate.SessionID) {
			return ErrConflict
		}
		confirmed, err := connection.Confirm(ctx, tx, candidate.SessionID, requestID)
		if err != nil {
			var current model.MobileConnection
			if loadErr := tx.First(&current, "id = ?", requestID).Error; loadErr == nil &&
				(current.Status == "confirmed" || current.Status == "provisioning" || current.Status == "released") {
				result = &current
				return nil
			}
			return err
		}
		result, err = connection.StartProvisioning(ctx, tx, confirmed.ID)
		return err
	})
	return result, err
}

func Reject(ctx context.Context, db *gorm.DB, requestID string) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lock(tx); err != nil {
			return err
		}
		var candidate model.MobileConnection
		if err := tx.Select("session_id").First(&candidate, "id = ?", requestID).Error; err != nil {
			return err
		}
		if !IsSession(tx, candidate.SessionID) {
			return ErrConflict
		}
		_, err := connection.Reject(ctx, tx, candidate.SessionID, requestID)
		return err
	})
}

func IsSession(db *gorm.DB, sessionID uint) bool {
	var count int64
	return db.Model(&model.PoolSession{}).Where("session_id = ?", sessionID).Count(&count).Error == nil && count == 1
}

// RetryBuild returns an unclaimed failed member to the durable build queue.
func RetryBuild(ctx context.Context, db *gorm.DB, sessionID uint) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lock(tx); err != nil {
			return err
		}
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?, ?)", workerLockClass, int64(sessionID)).Error; err != nil {
			return err
		}
		if !IsSession(tx, sessionID) {
			return ErrConflict
		}
		var session model.SetupSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, sessionID).Error; err != nil {
			return err
		}
		if session.Status != "failed" || session.AdminDid != "" {
			return ErrConflict
		}
		var protected int64
		if err := tx.Model(&model.MobileConnection{}).
			Where("session_id = ? AND confirmed_at IS NOT NULL", sessionID).
			Count(&protected).Error; err != nil {
			return err
		}
		if protected != 0 {
			return ErrConflict
		}
		return tx.Model(&session).Updates(map[string]any{
			"status": model.StatusPoolCreating, "error_msg": "", "failed_stage": "",
		}).Error
	})
}

// QueueCleanup accepts setup-session IDs. Confirmed claims remain protected
// even if activation failed and the setup session is no longer running.
func QueueCleanup(ctx context.Context, db *gorm.DB, sessionIDs []uint) error {
	if len(sessionIDs) < 1 {
		return ErrConflict
	}
	sessionIDs = append([]uint(nil), sessionIDs...)
	sort.Slice(sessionIDs, func(i, j int) bool { return sessionIDs[i] < sessionIDs[j] })
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lock(tx); err != nil {
			return err
		}
		for _, sessionID := range sessionIDs {
			// Wait for preparation/build to finish before changing the session to
			// deleting, otherwise a late build status update could revive it.
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?, ?)", workerLockClass, int64(sessionID)).Error; err != nil {
				return err
			}
			var membership model.PoolSession
			if err := tx.First(&membership, "session_id = ?", sessionID).Error; err != nil {
				return err
			}
			var session model.SetupSession
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, sessionID).Error; err != nil {
				return err
			}
			if session.AdminDid != "" {
				return ErrConflict
			}
			now, err := connection.Now(tx)
			if err != nil {
				return err
			}
			if err := tx.Model(&model.MobileConnection{}).
				Where("session_id = ? AND status = 'open' AND expires_at <= ?", sessionID, now).
				Update("status", "lapsed").Error; err != nil {
				return err
			}
			if err := tx.Model(&model.MobileConnection{}).
				Where("session_id = ? AND status = 'claimed' AND confirm_by <= ?", sessionID, now).
				Updates(map[string]any{"status": "lapsed", "admin_did": ""}).Error; err != nil {
				return err
			}
			var claims int64
			if err := tx.Model(&model.MobileConnection{}).
				Where("session_id = ? AND (confirmed_at IS NOT NULL OR status IN ?)", sessionID, []string{"open", "claimed", "confirmed", "provisioning", "released"}).
				Count(&claims).Error; err != nil {
				return err
			}
			if claims != 0 {
				return ErrConflict
			}
			var user model.User
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, session.UserID).Error; err != nil {
				return err
			}
			var passkeys int64
			if err := tx.Model(&model.UserPasskey{}).Where("user_id = ?", user.ID).Count(&passkeys).Error; err != nil {
				return err
			}
			if user.Email != nil || passkeys != 0 || user.FullstackAccess {
				return ErrConflict
			}
			if err := connection.Stop(ctx, tx, session.ID); err != nil {
				return err
			}
		}
		return nil
	})
}
