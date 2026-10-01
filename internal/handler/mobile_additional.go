package handler

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/model"
)

const additionalMobileFailure = "Unable to add this administrator. Generate a new QR code and try again."

type additionalMobileWorker struct {
	mu   sync.Mutex
	ctx  context.Context
	work map[string]struct{}
}

func (h *SetupHandler) StartAdditionalMobileWorker(ctx context.Context, resume bool) {
	if h.mobileWorker == nil {
		h.mobileWorker = &additionalMobileWorker{work: make(map[string]struct{})}
	}
	h.mobileWorker.mu.Lock()
	h.mobileWorker.ctx = ctx
	h.mobileWorker.mu.Unlock()
	if resume {
		go h.runAdditionalMobileQueue(ctx)
	}
}

func (h *SetupHandler) runAdditionalMobileQueue(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		var requests []model.MobileConnection
		if err := h.db.WithContext(ctx).
			Where("operation = ? AND status = ? AND provisioned_at IS NULL", connection.OperationGrantACL, "accepted").
			Find(&requests).Error; err == nil {
			for i := range requests {
				h.KickAdditionalMobile(requests[i].ID)
			}
		} else if ctx.Err() == nil {
			log.Printf("[mobile-connection] recovery query failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *SetupHandler) KickAdditionalMobile(requestID string) {
	if h.mobileWorker == nil {
		return
	}
	h.mobileWorker.mu.Lock()
	ctx := h.mobileWorker.ctx
	if ctx == nil || ctx.Err() != nil {
		h.mobileWorker.mu.Unlock()
		return
	}
	if _, running := h.mobileWorker.work[requestID]; running {
		h.mobileWorker.mu.Unlock()
		return
	}
	h.mobileWorker.work[requestID] = struct{}{}
	h.mobileWorker.mu.Unlock()

	go func() {
		defer func() {
			h.mobileWorker.mu.Lock()
			delete(h.mobileWorker.work, requestID)
			h.mobileWorker.mu.Unlock()
		}()
		h.runAdditionalMobile(ctx, requestID)
	}()
}

func (h *SetupHandler) runAdditionalMobile(ctx context.Context, requestID string) {
	var request model.MobileConnection
	if err := h.db.WithContext(ctx).First(&request, "id = ?", requestID).Error; err != nil ||
		request.Operation != connection.OperationGrantACL || request.Status != "accepted" || request.ProvisionedAt != nil {
		return
	}
	var session model.SetupSession
	if err := h.db.WithContext(ctx).First(&session, request.SessionID).Error; err != nil || session.Status != "running" {
		h.failAdditionalMobile(ctx, requestID, errors.New("session is not running"))
		return
	}

	result, err := h.performVtaAdminGrant(ctx, &session, request.AdminDid, connection.MobileAdminLabel)
	if errors.Is(err, errAclJobBusy) {
		return
	}
	if err != nil {
		h.failAdditionalMobile(ctx, requestID, err)
		return
	}
	if result.restartErr != nil {
		h.failAdditionalMobile(ctx, requestID, result.restartErr)
		return
	}
	if result.warning != "" {
		log.Printf("[mobile-connection] additional ACL warning for request %s: %s", requestID, result.warning)
	}
	now := time.Now()
	if err := h.db.WithContext(ctx).Model(&model.MobileConnection{}).
		Where("id = ? AND operation = ? AND status = ? AND provisioned_at IS NULL", requestID, connection.OperationGrantACL, "accepted").
		Update("provisioned_at", now).Error; err != nil {
		log.Printf("[mobile-connection] could not finish additional request %s: %v", requestID, err)
	}
}

func (h *SetupHandler) failAdditionalMobile(ctx context.Context, requestID string, err error) {
	log.Printf("[mobile-connection] additional request %s failed: %v", requestID, err)
	if updateErr := h.db.WithContext(ctx).Model(&model.MobileConnection{}).
		Where("id = ? AND operation = ? AND status = ?", requestID, connection.OperationGrantACL, "accepted").
		Updates(map[string]any{"status": "failed", "provision_error": additionalMobileFailure}).Error; updateErr != nil {
		log.Printf("[mobile-connection] could not record failure for request %s: %v", requestID, updateErr)
	}
}
