package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/publicid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrConflict       = errors.New("pool state changed or session is protected")
	ErrEmpty          = errors.New("no waiting VTA is available in this batch")
	ErrCapacity       = errors.New("pool inventory limit reached")
	ErrPublicDisabled = errors.New("public signup has no enabled pool")
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
	AdminID                     uint
	Key, Digest, Label, Purpose string
	Count                       int
	Capacity                    int
	Session                     model.SetupSession
	MemoryRequest               string
	MemoryLimit                 string
}

type AdmissionResult struct {
	Batch    *model.PoolBatch
	Sessions []model.SetupSession
	Created  bool
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
func Admit(ctx context.Context, db *gorm.DB, input Admission) (*AdmissionResult, error) {
	if input.Purpose == "" {
		input.Purpose = model.PoolPurposeAdminStation
	}
	if input.Count < 1 || input.Key == "" || len(input.Key) > 128 || input.Session.VtaImage == "" {
		return nil, errors.New("invalid pool admission")
	}
	if input.Purpose != model.PoolPurposeAdminStation && input.Purpose != model.PoolPurposePublicSignup {
		return nil, errors.New("invalid pool purpose")
	}
	result := &AdmissionResult{}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lock(tx); err != nil {
			return err
		}
		batch, created, err := admitBatchRow(ctx, tx, input)
		if err != nil {
			return err
		}
		if !created {
			result.Batch = batch
			return nil
		}
		users, err := createUsers(tx, input.Count)
		if err != nil {
			return err
		}
		sessions, err := createSetupSessions(tx, users, input.Session)
		if err != nil {
			return err
		}
		if err := createPoolMembershipsAndResources(tx, batch.ID, sessions, input.MemoryRequest, input.MemoryLimit); err != nil {
			return err
		}
		result.Batch, result.Sessions, result.Created = batch, sessions, true
		return nil
	})
	return result, err
}

func admitBatchRow(ctx context.Context, tx *gorm.DB, input Admission) (*model.PoolBatch, bool, error) {
	var inserted []model.PoolBatch
	err := tx.Raw(`WITH reserved AS (
			SELECT count(*)::integer AS count
			FROM pool_sessions ps
			JOIN setup_sessions s ON s.id = ps.session_id
			WHERE s.status <> 'running'
		)
		INSERT INTO pool_batches (
			created_by, label, purpose, requested_count, public_signup_enabled_at,
			idempotency_key, request_digest
		)
		SELECT ?, ?, ?, ?, CASE WHEN ? = ? THEN clock_timestamp() END, ?, ? FROM reserved
		WHERE ? <= GREATEST(? - reserved.count, 0)
		ON CONFLICT (created_by, idempotency_key) DO NOTHING
		RETURNING *`, input.AdminID, input.Label, input.Purpose, input.Count,
		input.Purpose, model.PoolPurposePublicSignup, input.Key, input.Digest, input.Count, input.Capacity).
		Scan(&inserted).Error
	if err != nil {
		return nil, false, err
	}
	if len(inserted) == 1 {
		return &inserted[0], true, nil
	}
	previous, err := LookupBatch(ctx, tx, input.AdminID, input.Key, input.Digest)
	if err == nil {
		return previous, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}
	reserved, err := nonRunningSessionCount(tx)
	if err != nil {
		return nil, false, err
	}
	return nil, false, &CapacityError{Available: max(input.Capacity-reserved, 0)}
}

type Addition struct {
	AdminID       uint
	BatchID       uint
	Key, Digest   string
	Count         int
	Capacity      int
	Session       model.SetupSession
	MemoryRequest string
	MemoryLimit   string
}

type AdditionResult struct {
	Batch    *model.PoolBatch
	Sessions []model.SetupSession
	Created  bool
}

func lookupAddition(ctx context.Context, db *gorm.DB, adminID uint, key, digest string, batchID uint) (*model.PoolBatchAddition, error) {
	var addition model.PoolBatchAddition
	err := db.WithContext(ctx).Where("created_by = ? AND idempotency_key = ?", adminID, key).First(&addition).Error
	if err == nil && (addition.RequestDigest != digest || addition.BatchID != batchID) {
		return nil, ErrConflict
	}
	return &addition, err
}

// AddInventory appends real users and setup sessions to an existing batch.
// The immutable addition row makes retries safe while requested_count remains
// the cumulative amount requested for the operator-facing pool.
func AddInventory(ctx context.Context, db *gorm.DB, input Addition) (*AdditionResult, error) {
	if input.BatchID == 0 || input.Count < 1 || input.Key == "" || len(input.Key) > 128 || input.Session.VtaImage == "" {
		return nil, errors.New("invalid pool addition")
	}
	result := &AdditionResult{}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lock(tx); err != nil {
			return err
		}
		if _, err := lookupAddition(ctx, tx, input.AdminID, input.Key, input.Digest, input.BatchID); err == nil {
			var batch model.PoolBatch
			if err := tx.First(&batch, input.BatchID).Error; err != nil {
				return err
			}
			result.Batch = &batch
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var batch model.PoolBatch
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&batch, input.BatchID).Error; err != nil {
			return err
		}
		if batch.CleanupRequestedAt != nil {
			return ErrConflict
		}
		reserved, err := nonRunningSessionCount(tx)
		if err != nil {
			return err
		}
		if input.Count > max(input.Capacity-reserved, 0) {
			return &CapacityError{Available: max(input.Capacity-reserved, 0)}
		}

		addition := model.PoolBatchAddition{
			BatchID: input.BatchID, CreatedBy: input.AdminID, RequestedCount: input.Count,
			IdempotencyKey: input.Key, RequestDigest: input.Digest,
		}
		if err := tx.Create(&addition).Error; err != nil {
			return err
		}
		users, err := createUsers(tx, input.Count)
		if err != nil {
			return err
		}
		sessions, err := createSetupSessions(tx, users, input.Session)
		if err != nil {
			return err
		}
		if err := createPoolMembershipsAndResources(tx, batch.ID, sessions, input.MemoryRequest, input.MemoryLimit); err != nil {
			return err
		}
		if err := tx.Model(&batch).Update("requested_count", gorm.Expr("requested_count + ?", input.Count)).Error; err != nil {
			return err
		}
		batch.RequestedCount += input.Count
		result.Batch, result.Sessions, result.Created = &batch, sessions, true
		return nil
	})
	return result, err
}

func nonRunningSessionCount(tx *gorm.DB) (int, error) {
	var count int64
	err := tx.Table("pool_sessions ps").
		Joins("JOIN setup_sessions s ON s.id = ps.session_id").
		Where("s.status <> ?", "running").Count(&count).Error
	return int(count), err
}

var newAccountID = publicid.NewAccount
var newVTAName = randomName

func createUsers(tx *gorm.DB, count int) ([]model.User, error) {
	users := make([]model.User, 0, count)
	generated := make(map[string]struct{}, count)
	for attempt := 0; len(users) < count && attempt < 5; attempt++ {
		names, err := generateUniqueIDs(count-len(users), generated, newAccountID)
		if err != nil {
			return nil, err
		}
		placeholders := strings.TrimSuffix(strings.Repeat("(?),", len(names)), ",")
		args := make([]any, len(names))
		for i := range names {
			args[i] = names[i]
		}
		var inserted []model.User
		if err := tx.Raw(`INSERT INTO users (unique_id) VALUES `+placeholders+`
			ON CONFLICT (unique_id) DO NOTHING
			RETURNING id, unique_id, created_at, updated_at`, args...).Scan(&inserted).Error; err != nil {
			return nil, err
		}
		users = append(users, inserted...)
	}
	if len(users) != count {
		return nil, errors.New("could not allocate user identifiers")
	}
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	return users, nil
}

func createSetupSessions(tx *gorm.DB, users []model.User, template model.SetupSession) ([]model.SetupSession, error) {
	pending := make(map[uint]model.User, len(users))
	for i := range users {
		pending[users[i].ID] = users[i]
	}
	sessions := make([]model.SetupSession, 0, len(users))
	generated := make(map[string]struct{}, len(users))
	for attempt := 0; len(pending) > 0 && attempt < 5; attempt++ {
		pendingUsers := make([]model.User, 0, len(pending))
		for _, user := range pending {
			pendingUsers = append(pendingUsers, user)
		}
		sort.Slice(pendingUsers, func(i, j int) bool { return pendingUsers[i].ID < pendingUsers[j].ID })
		names, err := generateUniqueIDs(len(pendingUsers), generated, newVTAName)
		if err != nil {
			return nil, err
		}
		candidates := make([]model.SetupSession, len(pendingUsers))
		userIDs := make([]uint, len(pendingUsers))
		for i := range pendingUsers {
			session := template
			session.ID, session.UserID, session.Status = 0, pendingUsers[i].ID, model.StatusPoolCreating
			session.VtaName, session.AdminDid = names[i], ""
			session.Subdomain = template.Subdomain + names[i]
			session.VtaDidUrl = session.DidHostingServerURL + "/" + names[i] + "-vta"
			session.CreatedAt, session.UpdatedAt = time.Time{}, time.Time{}
			candidates[i], userIDs[i] = session, pendingUsers[i].ID
		}
		created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&candidates)
		if created.Error != nil {
			return nil, created.Error
		}
		if created.RowsAffected == int64(len(candidates)) {
			sessions = append(sessions, candidates...)
			for i := range pendingUsers {
				delete(pending, pendingUsers[i].ID)
			}
			continue
		}
		var inserted []model.SetupSession
		if err := tx.Where("user_id IN ?", userIDs).Order("id").Find(&inserted).Error; err != nil {
			return nil, err
		}
		for i := range inserted {
			if _, ok := pending[inserted[i].UserID]; !ok {
				continue
			}
			sessions = append(sessions, inserted[i])
			delete(pending, inserted[i].UserID)
		}
	}
	if len(pending) != 0 {
		return nil, errors.New("could not allocate VTA identifiers")
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	return sessions, nil
}

func createPoolMembershipsAndResources(tx *gorm.DB, batchID uint, sessions []model.SetupSession, memoryRequest, memoryLimit string) error {
	values := strings.TrimSuffix(strings.Repeat("(CAST(? AS bigint),CAST(? AS bigint)),", len(sessions)), ",")
	args := make([]any, 0, len(sessions)*2+2)
	for i := range sessions {
		args = append(args, sessions[i].ID, batchID)
	}
	args = append(args, memoryRequest, memoryLimit)
	result := tx.Exec(`WITH admitted(session_id, batch_id) AS (VALUES `+values+`),
		memberships AS (
			INSERT INTO pool_sessions (session_id, batch_id)
			SELECT session_id, batch_id FROM admitted
			RETURNING session_id
		)
		INSERT INTO workload_resources (setup_session_id, component, memory_request, memory_limit)
		SELECT session_id, 'vta', ?, ? FROM memberships`, args...)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != int64(len(sessions)) {
		return errors.New("pool membership bulk insert was incomplete")
	}
	return nil
}

func generateUniqueIDs(count int, generated map[string]struct{}, next func() (string, error)) ([]string, error) {
	values := make([]string, 0, count)
	for draws := 0; len(values) < count && draws < count*20+100; draws++ {
		value, err := next()
		if err != nil {
			return nil, err
		}
		if _, exists := generated[value]; exists {
			continue
		}
		generated[value] = struct{}{}
		values = append(values, value)
	}
	if len(values) != count {
		return nil, errors.New("random identifier source produced too many duplicates")
	}
	return values, nil
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

// ReservePublic idempotently reserves one waiting VTA from the enabled public
// signup inventory. The caller supplies only the digest of its browser
// capability; the plaintext token never reaches persistence.
func ReservePublic(ctx context.Context, db *gorm.DB, accessDigest []byte) (*model.MobileConnection, bool, error) {
	if len(accessDigest) != 32 {
		return nil, false, errors.New("invalid public claim capability digest")
	}
	var result *model.MobileConnection
	created := false
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lock(tx); err != nil {
			return err
		}
		var existing model.MobileConnection
		lookup := tx.Where("public_access_digest = ?", accessDigest).First(&existing)
		if lookup.Error == nil {
			result = &existing
			return nil
		}
		if !errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
			return lookup.Error
		}
		var enabled int64
		if err := tx.Model(&model.PoolBatch{}).
			Where("purpose = ? AND public_signup_enabled_at IS NOT NULL", model.PoolPurposePublicSignup).
			Count(&enabled).Error; err != nil {
			return err
		}
		if enabled == 0 {
			return ErrPublicDisabled
		}

		var candidates []model.SetupSession
		if err := tx.Table("setup_sessions s").
			Select("s.*").
			Joins("JOIN pool_sessions ps ON ps.session_id = s.id").
			Joins("JOIN pool_batches pb ON pb.id = ps.batch_id").
			Where("pb.purpose = ? AND pb.public_signup_enabled_at IS NOT NULL", model.PoolPurposePublicSignup).
			Where("s.status = ? AND s.admin_did = ''", model.StatusPoolWaiting).
			Where(`NOT EXISTS (
				SELECT 1 FROM mobile_connections mc
				WHERE mc.session_id = s.id
				  AND (
					(mc.status = 'open' AND mc.expires_at > clock_timestamp())
					OR (mc.status = 'claimed' AND mc.confirm_by > clock_timestamp())
					OR mc.status IN ('confirmed', 'provisioning')
				  )
			)`).
			Order("pb.public_signup_enabled_at, pb.id, s.id").Find(&candidates).Error; err != nil {
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
			if err := tx.Model(mobile).Update("public_access_digest", append([]byte(nil), accessDigest...)).Error; err != nil {
				return err
			}
			mobile.PublicAccessDigest = append([]byte(nil), accessDigest...)
			result, created = mobile, true
			return nil
		}
		return ErrEmpty
	})
	return result, created, err
}

func SetPublicSignup(ctx context.Context, db *gorm.DB, batchID uint, enabled bool) (*model.PoolBatch, error) {
	var batch model.PoolBatch
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lock(tx); err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&batch, batchID).Error; err != nil {
			return err
		}
		if batch.Purpose != model.PoolPurposePublicSignup || batch.CleanupRequestedAt != nil {
			return ErrConflict
		}
		if enabled {
			if batch.PublicSignupEnabledAt != nil {
				return nil
			}
			now, err := connection.Now(tx)
			if err != nil {
				return err
			}
			if err := tx.Model(&batch).Update("public_signup_enabled_at", now).Error; err != nil {
				return err
			}
			batch.PublicSignupEnabledAt = &now
			return nil
		}
		if err := tx.Model(&batch).Update("public_signup_enabled_at", nil).Error; err != nil {
			return err
		}
		batch.PublicSignupEnabledAt = nil
		return nil
	})
	return &batch, err
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
		if err := tx.Select("session_id", "status").First(&candidate, "id = ?", requestID).Error; err != nil {
			return err
		}
		if !IsSession(tx, candidate.SessionID) {
			return ErrConflict
		}
		switch candidate.Status {
		case "open":
			_, err := connection.Change(ctx, tx, candidate.SessionID, "cancel", requestID)
			return err
		case "claimed":
			_, err := connection.Reject(ctx, tx, candidate.SessionID, requestID)
			return err
		default:
			return ErrConflict
		}
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
	for i := 1; i < len(sessionIDs); i++ {
		if sessionIDs[i] == sessionIDs[i-1] {
			return ErrConflict
		}
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lock(tx); err != nil {
			return err
		}
		return queueCleanupBulk(tx, sessionIDs)
	})
}

// QueueBatchCleanup atomically protects every member from new claims and marks
// it for durable teardown. The batch row is removed after its final member.
func QueueBatchCleanup(ctx context.Context, db *gorm.DB, batchID uint) ([]uint, error) {
	var sessionIDs []uint
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lock(tx); err != nil {
			return err
		}
		var batch model.PoolBatch
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&batch, batchID).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.PoolSession{}).Where("batch_id = ?", batchID).
			Order("session_id").Pluck("session_id", &sessionIDs).Error; err != nil {
			return err
		}
		if batch.CleanupRequestedAt != nil {
			return nil
		}
		if len(sessionIDs) == 0 {
			return tx.Delete(&batch).Error
		}
		if err := queueCleanupBulk(tx, sessionIDs); err != nil {
			return err
		}
		return tx.Model(&batch).Update("cleanup_requested_at", gorm.Expr("clock_timestamp()")).Error
	})
	return sessionIDs, err
}

func queueCleanupBulk(tx *gorm.DB, sessionIDs []uint) error {
	var memberships int64
	if err := tx.Model(&model.PoolSession{}).
		Where("session_id IN ?", sessionIDs).Count(&memberships).Error; err != nil {
		return err
	}
	if memberships != int64(len(sessionIDs)) {
		return ErrConflict
	}

	var sessions []model.SetupSession
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", sessionIDs).Order("id").Find(&sessions).Error; err != nil {
		return err
	}
	if len(sessions) != len(sessionIDs) {
		return ErrConflict
	}
	userIDs := make([]uint, len(sessions))
	for i := range sessions {
		if sessions[i].AdminDid != "" ||
			(sessions[i].Status != model.StatusPoolWaiting && sessions[i].Status != "failed") {
			return ErrConflict
		}
		userIDs[i] = sessions[i].UserID
	}

	var users []model.User
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", userIDs).Order("id").Find(&users).Error; err != nil {
		return err
	}
	if len(users) != len(userIDs) {
		return ErrConflict
	}
	for i := range users {
		if users[i].Email != nil || users[i].FullstackAccess {
			return ErrConflict
		}
	}
	var passkeys int64
	if err := tx.Model(&model.UserPasskey{}).Where("user_id IN ?", userIDs).Count(&passkeys).Error; err != nil {
		return err
	}
	if passkeys != 0 {
		return ErrConflict
	}

	now, err := connection.Now(tx)
	if err != nil {
		return err
	}
	if err := tx.Model(&model.MobileConnection{}).
		Where("session_id IN ? AND status = 'open' AND expires_at <= ?", sessionIDs, now).
		Update("status", "lapsed").Error; err != nil {
		return err
	}
	if err := tx.Model(&model.MobileConnection{}).
		Where("session_id IN ? AND status = 'claimed' AND confirm_by <= ?", sessionIDs, now).
		Updates(map[string]any{"status": "lapsed", "admin_did": ""}).Error; err != nil {
		return err
	}
	var claims int64
	if err := tx.Model(&model.MobileConnection{}).
		Where("session_id IN ? AND (confirmed_at IS NOT NULL OR status IN ?)", sessionIDs, []string{"open", "claimed", "confirmed", "provisioning", "released"}).
		Count(&claims).Error; err != nil {
		return err
	}
	if claims != 0 {
		return ErrConflict
	}

	updated := tx.Model(&model.SetupSession{}).Where("id IN ?", sessionIDs).Update("status", "deleting")
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != int64(len(sessionIDs)) {
		return ErrConflict
	}
	return nil
}

func FinalizeBatchCleanup(ctx context.Context, db *gorm.DB, batchID uint) error {
	return db.WithContext(ctx).Exec(`
		DELETE FROM pool_batches b
		WHERE b.id = ?
		  AND b.cleanup_requested_at IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM pool_sessions ps WHERE ps.batch_id = b.id)`, batchID).Error
}

func FinalizeEmptyBatchCleanups(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Exec(`
		DELETE FROM pool_batches b
		WHERE b.cleanup_requested_at IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM pool_sessions ps WHERE ps.batch_id = b.id)`).Error
}
