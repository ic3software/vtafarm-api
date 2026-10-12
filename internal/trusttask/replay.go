package trusttask

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/trusttasks"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ic3software/vtafarm-api/internal/model"
)

const defaultExecutionLease = 30 * time.Second

type ReplayGuard struct {
	db             *gorm.DB
	executionLease time.Duration
}

func NewReplayGuard(db *gorm.DB) *ReplayGuard {
	return &ReplayGuard{db: db, executionLease: defaultExecutionLease}
}

func (g *ReplayGuard) Claim(
	ctx context.Context,
	id string,
	digest string,
	retainUntil time.Time,
	now time.Time,
) (trusttasks.ReplayVerdict, error) {
	if g == nil || g.db == nil || g.executionLease <= 0 {
		return trusttasks.ReplayVerdict{}, errors.New("trust task replay guard is not configured")
	}

	verdict := trusttasks.ReplayVerdict{}
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		candidate := model.TrustTaskExecution{
			DocumentID:     id,
			DocumentDigest: digest,
			State:          "executing",
			RetainUntil:    retainUntil,
			LeaseExpiresAt: now.Add(g.executionLease),
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&candidate)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			verdict.Kind = trusttasks.VerdictFresh
			return nil
		}

		var existing model.TrustTaskExecution
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&existing, "document_id = ?", id).Error; err != nil {
			return err
		}
		if !now.Before(existing.RetainUntil) {
			if err := tx.Model(&existing).Updates(map[string]any{
				"document_digest":  digest,
				"state":            "executing",
				"response_bytes":   nil,
				"retain_until":     retainUntil,
				"lease_expires_at": now.Add(g.executionLease),
				"created_at":       now,
				"updated_at":       now,
			}).Error; err != nil {
				return err
			}
			verdict.Kind = trusttasks.VerdictFresh
			return nil
		}
		if existing.DocumentDigest != digest {
			verdict.Kind = trusttasks.VerdictConflict
			return nil
		}
		if existing.State == "executing" && !now.Before(existing.LeaseExpiresAt) {
			if err := tx.Model(&existing).Updates(map[string]any{
				"lease_expires_at": now.Add(g.executionLease),
				"updated_at":       now,
			}).Error; err != nil {
				return err
			}
			verdict.Kind = trusttasks.VerdictFresh
			return nil
		}

		verdict.Kind = trusttasks.VerdictDuplicate
		verdict.InFlight = existing.State == "executing"
		if len(existing.ResponseBytes) != 0 {
			verdict.PriorResponse = append([]byte(nil), existing.ResponseBytes...)
		}
		return nil
	})
	return verdict, err
}

func (g *ReplayGuard) RecordResponse(ctx context.Context, id string, response json.RawMessage) error {
	if g == nil || g.db == nil {
		return errors.New("trust task replay guard is not configured")
	}
	result := g.db.WithContext(ctx).Model(&model.TrustTaskExecution{}).
		Where("document_id = ? AND state = ?", id, "executing").
		Updates(map[string]any{
			"state":          "completed",
			"response_bytes": append([]byte(nil), response...),
			"updated_at":     time.Now().UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("trust task execution is not active")
	}
	return nil
}

func (g *ReplayGuard) Release(ctx context.Context, id, digest string) error {
	if g == nil || g.db == nil {
		return errors.New("trust task replay guard is not configured")
	}
	return g.db.WithContext(ctx).
		Where("document_id = ? AND document_digest = ? AND state = ?", id, digest, "executing").
		Delete(&model.TrustTaskExecution{}).Error
}

var _ trusttasks.ReplayGuard = (*ReplayGuard)(nil)
