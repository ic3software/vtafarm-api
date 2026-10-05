package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/ic3software/vtafarm-api/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const defaultVTALimit = 2

var (
	errVTALimitReached         = errors.New("You’ve reached your limit of 2 VTAs. Delete an existing VTA to create another.")
	errFullstackAccessRequired = errors.New("Full Stack creation requires Fullstack Access.")
)

func checkSessionAccess(db *gorm.DB, user model.User, mode string) error {
	if user.FullstackAccess {
		return nil
	}
	if mode == model.ModeFullStack {
		return errFullstackAccessRequired
	}
	var count int64
	if err := db.Model(&model.SetupSession{}).Where("user_id = ?", user.ID).Count(&count).Error; err != nil {
		return err
	}
	if count >= defaultVTALimit {
		return errVTALimitReached
	}
	return nil
}

func (h *SetupHandler) persistUserSession(ctx context.Context, session *model.SetupSession) error {
	return h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user model.User
		// Serialize inserts across API instances, and recheck grants/revocations
		// made while DNS provisioning was in progress. Commit before starting jobs.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, session.UserID).Error; err != nil {
			return err
		}
		if err := checkSessionAccess(tx, user, session.Mode); err != nil {
			return err
		}
		return tx.Create(session).Error
	})
}

func writeSessionAccessError(c *gin.Context, err error) bool {
	var reason string
	switch {
	case errors.Is(err, errVTALimitReached):
		reason = "vta_limit_reached"
	case errors.Is(err, errFullstackAccessRequired):
		reason = "fullstack_access_required"
	default:
		return false
	}
	c.JSON(http.StatusForbidden, gin.H{"error": err.Error(), "reason": reason})
	return true
}
