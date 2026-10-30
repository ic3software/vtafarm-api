package walletauth

import (
	"context"
	"errors"
	"time"

	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func AcquireRedeemPoll(ctx context.Context, db *gorm.DB, requestID, pollID string, lease time.Duration) (bool, error) {
	if db == nil || requestID == "" || pollID == "" || lease <= 0 {
		return false, errors.New("invalid wallet redeem poll lease")
	}
	now := time.Now().UTC()
	result := db.WithContext(ctx).Model(&model.WalletLoginRequest{}).
		Where("request_id = ? AND state IN ? AND (redeem_poll_until IS NULL OR redeem_poll_until <= ?)", requestID, []string{"pending", "claimed", "identified"}, now).
		Updates(map[string]any{"redeem_poll_id": pollID, "redeem_poll_until": now.Add(lease)})
	return result.RowsAffected == 1, result.Error
}

func ReleaseRedeemPoll(ctx context.Context, db *gorm.DB, requestID, pollID string) error {
	return db.WithContext(ctx).Model(&model.WalletLoginRequest{}).
		Where("request_id = ? AND redeem_poll_id = ?", requestID, pollID).
		Updates(map[string]any{"redeem_poll_id": nil, "redeem_poll_until": nil}).Error
}

// ActiveBindings resolves an already-authorized claimant DID. Released claims
// that predate wallet login are materialized first; the caller must have
// verified a fresh identify proof before calling this function.
func ActiveBindings(ctx context.Context, db *gorm.DB, loginDID string) ([]model.WalletLoginBinding, error) {
	if err := materializeReleasedClaims(ctx, db, loginDID); err != nil {
		return nil, err
	}
	var bindings []model.WalletLoginBinding
	if err := db.WithContext(ctx).
		Where("login_did = ? AND revoked_at IS NULL", loginDID).
		Order("id ASC").
		Find(&bindings).Error; err != nil {
		return nil, err
	}
	return bindings, nil
}

func materializeReleasedClaims(ctx context.Context, db *gorm.DB, loginDID string) error {
	type source struct {
		ClaimID   string
		SessionID uint
		UserID    uint
		AdminDID  string `gorm:"column:admin_did"`
		Digest    *string
		Released  time.Time
	}
	var sources []source
	if err := db.WithContext(ctx).Table("mobile_connections AS mc").
		Select("mc.id AS claim_id, ss.id AS session_id, ss.user_id, mc.admin_did, mc.claimant_digest AS digest, mc.released_at AS released").
		Joins("JOIN setup_sessions AS ss ON ss.id = mc.session_id").
		Where("mc.status = 'released' AND mc.operation IN ? AND mc.admin_did = ?", []string{connection.OperationProvisionVTA, connection.OperationGrantACL}, loginDID).
		Find(&sources).Error; err != nil {
		return err
	}
	for _, source := range sources {
		if source.Digest == nil || connection.ClaimantDigest(source.AdminDID) != *source.Digest {
			continue
		}
		binding := model.WalletLoginBinding{
			UserID:          source.UserID,
			LoginDID:        source.AdminDID,
			SourceSessionID: source.SessionID,
			SourceClaimID:   source.ClaimID,
			CreatedAt:       source.Released,
		}
		if err := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&binding).Error; err != nil {
			return err
		}
	}
	return nil
}

func RevokeBinding(ctx context.Context, db *gorm.DB, userID uint, bindingID uint64) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var binding model.WalletLoginBinding
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&binding, "id = ? AND user_id = ? AND revoked_at IS NULL", bindingID, userID).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := tx.Model(&binding).Update("revoked_at", now).Error; err != nil {
			return err
		}
		return tx.Model(&model.WalletLoginSession{}).
			Where("binding_id = ? AND revoked_at IS NULL", binding.ID).
			Update("revoked_at", now).Error
	})
}
