package handler

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	farmtrusttask "github.com/ic3software/vtafarm-api/internal/trusttask"
	"github.com/ic3software/vtafarm-api/internal/walletauth"
	authoobcancelv0_1 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/cancel/v0_1"
	authoobclaimv0_2 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/claim/v0_2"
	authoobgrantv0_3 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/grant/v0_3"
	authoobidentifyv0_1 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/identify/v0_1"
	authoobprovev0_3 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/prove/v0_3"
	authoobredeemv0_2 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/redeem/v0_2"
	authoobrequestv0_2 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/request/v0_2"
	authoobrespondv0_3 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/respond/v0_3"
	"github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/trusttasks"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	oobRequestType = authoobrequestv0_2.TypeURI
	oobClaimType   = authoobclaimv0_2.TypeURI
	oobProveType   = authoobprovev0_3.TypeURI
	oobRespondType = authoobrespondv0_3.TypeURI
	oobRedeemType  = authoobredeemv0_2.TypeURI
	oobCancelType  = authoobcancelv0_1.TypeURI

	oobClaimWindow    = 2 * time.Minute
	oobDecisionWindow = 2 * time.Minute
	oobSessionLimit   = 24 * time.Hour
)

func (h *TrustTaskHandler) consumeOOB(c *gin.Context, typeURI string, payload, raw json.RawMessage) {
	switch typeURI {
	case oobRequestType:
		consumeOOBTyped(h, c, raw, authoobrequestv0_2.Spec, payload, func(ctx context.Context, request *trusttasks.Document[authoobrequestv0_2.Payload], parties trusttasks.ResolvedParties) (*trusttasks.Document[authoobrequestv0_2.Response], error) {
			return h.oobRequest(ctx, c, request, parties)
		})
	case oobClaimType:
		consumeOOBTyped(h, c, raw, authoobclaimv0_2.Spec, payload, h.oobClaim)
	case oobProveType:
		consumeOOBTyped(h, c, raw, authoobprovev0_3.Spec, payload, func(ctx context.Context, request *trusttasks.Document[authoobprovev0_3.Payload], parties trusttasks.ResolvedParties) (*trusttasks.Document[authoobprovev0_3.Response], error) {
			return h.oobProve(ctx, c, request, parties)
		})
	case oobRespondType:
		consumeOOBTyped(h, c, raw, authoobrespondv0_3.Spec, payload, h.oobRespond)
	case oobRedeemType:
		consumeOOBTyped(h, c, raw, authoobredeemv0_2.Spec, payload, func(ctx context.Context, request *trusttasks.Document[authoobredeemv0_2.Payload], parties trusttasks.ResolvedParties) (*trusttasks.Document[authoobredeemv0_2.Response], error) {
			return h.oobRedeem(ctx, c, request, parties)
		})
	case oobCancelType:
		consumeOOBTyped(h, c, raw, authoobcancelv0_1.Spec, payload, h.oobCancel)
	}
}

func consumeOOBTyped[P, R any](
	h *TrustTaskHandler,
	c *gin.Context,
	raw json.RawMessage,
	spec trusttasks.SpecPolicy,
	payload json.RawMessage,
	handle func(context.Context, *trusttasks.Document[P], trusttasks.ResolvedParties) (*trusttasks.Document[R], error),
) {
	if err := h.validator.Validate(spec.PayloadSchema, payload); err != nil {
		h.emitMalformed(c, raw, err)
		return
	}
	var document trusttasks.Document[P]
	if err := json.Unmarshal(raw, &document); err != nil {
		h.emitMalformed(c, raw, err)
		return
	}
	outcome, err := trusttasks.ConsumeInbound(c.Request.Context(), trusttasks.ConsumeOptions[P, R]{
		Transport:     trusttasks.UnauthenticatedTransport{URI: "https://trusttasks.org/bindings/https/0.2"},
		Spec:          spec,
		ProofPolicy:   trusttasks.ProofPolicy{Kind: trusttasks.ProofVerify, Verifier: h.verifier},
		PayloadPolicy: trusttasks.PayloadPolicy{Kind: trusttasks.PayloadValidate, Validator: h.validator},
		Checks:        trusttasks.ConsequentialChecks(h.guard),
		Doc:           &document,
		MyVID:         h.farmDID,
		Now:           time.Now(),
		NewErrorID:    uuid.NewString,
		Handler:       handle,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Unable to process wallet sign-in."})
		return
	}
	emitTrustTaskOutcome(h, c, outcome)
}

func signedOOBResponse[P, R any](h *TrustTaskHandler, request *trusttasks.Document[P], payload R, schema string) (*trusttasks.Document[R], error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if err := h.validator.Validate(schema, encoded); err != nil {
		return nil, err
	}
	response := trusttasks.RespondWith(request, uuid.NewString(), payload, nil)
	if err := farmtrusttask.SignDocument(h.signer, response); err != nil {
		return nil, err
	}
	return response, nil
}

func (h *TrustTaskHandler) oobRequest(ctx context.Context, c *gin.Context, request *trusttasks.Document[authoobrequestv0_2.Payload], parties trusttasks.ResolvedParties) (*trusttasks.Document[authoobrequestv0_2.Response], error) {
	if c.GetHeader("Origin") != h.portalOrigin {
		return nil, trustTaskRefuse(h, request, trusttasks.CodePermissionDenied, "sign-in requests are accepted only from the configured Farm Portal origin", false, nil)
	}
	if parties.Issuer == nil {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:keyUnsupported"), "starter key is unsupported", false, nil)
	}
	now := time.Now().UTC()
	network := sha256.Sum256([]byte(c.ClientIP()))
	var open int64
	if err := h.db.WithContext(ctx).Model(&model.WalletLoginRequest{}).
		Where("starter_network_digest = ? AND state IN ? AND claim_closes_at > ?", network[:], []string{"pending", "claimed", "identified", "approved"}, now).
		Count(&open).Error; err != nil {
		return nil, err
	}
	if open >= 5 {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob/request:rateLimited"), "too many sign-in requests are already open", true, nil)
	}
	requestID, err := randomRequestID()
	if err != nil {
		return nil, err
	}
	browser, osName := requesterAgent(c.Request.UserAgent())
	record := model.WalletLoginRequest{
		RequestID:            requestID,
		State:                "pending",
		StarterDID:           *parties.Issuer,
		StarterNetworkDigest: network[:],
		Origin:               h.portalOrigin,
		RequesterBrowser:     browser,
		RequesterOS:          osName,
		CreatedAt:            now,
		ClaimClosesAt:        now.Add(oobClaimWindow),
	}
	if err := h.db.WithContext(ctx).Create(&record).Error; err != nil {
		return nil, err
	}
	response := authoobrequestv0_2.Response{RequestID: requestID, ClaimClosesAt: record.ClaimClosesAt.Format(time.RFC3339)}
	return signedOOBResponse(h, request, response, authoobrequestv0_2.ResponsePayloadSchemaJSON)
}

func (h *TrustTaskHandler) oobClaim(ctx context.Context, request *trusttasks.Document[authoobclaimv0_2.Payload], parties trusttasks.ResolvedParties) (*trusttasks.Document[authoobclaimv0_2.Response], error) {
	if parties.Issuer == nil || request.ParentThreadID == nil || *request.ParentThreadID != request.Payload.RequestID {
		return nil, trustTaskRefuse(h, request, trusttasks.CodeMalformedRequest, "parentThreadId must equal payload.requestId", false, nil)
	}
	now := time.Now().UTC()
	var record model.WalletLoginRequest
	var response *trusttasks.Document[authoobclaimv0_2.Response]
	err := h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&record, "request_id = ?", request.Payload.RequestID).Error; err != nil {
			return err
		}
		if record.State != "pending" {
			return walletStateError("alreadyClaimed")
		}
		if !now.Before(record.ClaimClosesAt) {
			if err := tx.Model(&record).Updates(map[string]any{"state": "expired", "ended_at": now}).Error; err != nil {
				return err
			}
			return walletStateError("requestExpired")
		}
		matchNumber, err := randomMatchNumber()
		if err != nil {
			return err
		}
		decisionClosesAt := now.Add(oobDecisionWindow)
		payload := authoobclaimv0_2.Response{
			RequestID:        record.RequestID,
			Service:          authoobclaimv0_2.Service{DID: h.farmDID, Name: h.farmName},
			Origin:           record.Origin,
			Purpose:          authoobclaimv0_2.PurposeLogin,
			DecisionClosesAt: decisionClosesAt.Format(time.RFC3339),
		}
		response, err = signedOOBResponse(h, request, payload, authoobclaimv0_2.ResponsePayloadSchemaJSON)
		if err != nil {
			return err
		}
		step1, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return tx.Model(&record).Updates(map[string]any{
			"state": "claimed", "approver_did": *parties.Issuer, "match_number": matchNumber,
			"decision_closes_at": decisionClosesAt, "step1": step1,
		}).Error
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:requestNotFound"), "no such sign-in request", false, nil)
		}
		if state, ok := err.(walletStateError); ok {
			return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob/claim:"+state.Error()), "sign-in request cannot be claimed", false, nil)
		}
		return nil, err
	}
	return response, nil
}

func (h *TrustTaskHandler) oobProve(ctx context.Context, c *gin.Context, request *trusttasks.Document[authoobprovev0_3.Payload], parties trusttasks.ResolvedParties) (*trusttasks.Document[authoobprovev0_3.Response], error) {
	identifyRaw, err := json.Marshal(request.Payload.Identify)
	if err != nil {
		return nil, err
	}
	var identify trusttasks.Document[authoobidentifyv0_1.Payload]
	if err := json.Unmarshal(identifyRaw, &identify); err != nil || identify.Issuer == nil {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
	}
	var record model.WalletLoginRequest
	if err := h.db.WithContext(ctx).First(&record, "request_id = ?", identify.Payload.RequestID).Error; err != nil {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:requestNotFound"), "no such sign-in request", false, nil)
	}
	if parties.Issuer == nil || record.ApproverDID == nil || *record.ApproverDID != *parties.Issuer || record.State != "claimed" || record.DecisionClosesAt == nil || !time.Now().UTC().Before(*record.DecisionClosesAt) {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
	}
	if err := validateCarried(h, ctx, identifyRaw, &identify, authoobidentifyv0_1.TypeURI, authoobidentifyv0_1.PayloadSchemaJSON, "authentication"); err != nil || identify.Payload.ApproverKey != *parties.Issuer || identify.Payload.RequestID != record.RequestID {
		h.declineWalletRequest(ctx, record.RequestID, "claimed")
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
	}
	bindings, bindErr := walletauth.ActiveBindings(ctx, h.db, *identify.Issuer)
	if bindErr != nil || len(bindings) == 0 {
		h.declineWalletRequest(ctx, record.RequestID, "claimed")
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
	}
	if record.MatchNumber == nil || identify.Payload.EnteredNumber != *record.MatchNumber {
		h.declineWalletRequest(ctx, record.RequestID, "claimed")
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob/prove:numberMismatch"), "the number does not match", false, nil)
	}
	var step1 authoobclaimv0_2.Response
	if err := json.Unmarshal(record.Step1, &step1); err != nil {
		return nil, err
	}
	sameNetwork := json.RawMessage(`"unknown"`)
	if digest := sha256.Sum256([]byte(c.ClientIP())); len(record.StarterNetworkDigest) > 0 && c.ClientIP() != "" {
		if string(digest[:]) == string(record.StarterNetworkDigest) {
			sameNetwork = json.RawMessage(`true`)
		} else {
			sameNetwork = json.RawMessage(`false`)
		}
	}
	payload := authoobprovev0_3.Response{
		RequestID:        record.RequestID,
		Service:          authoobprovev0_3.Service{DID: step1.Service.DID, Name: step1.Service.Name},
		Origin:           record.Origin,
		Purpose:          authoobprovev0_3.PurposeLogin,
		DecisionClosesAt: record.DecisionClosesAt.UTC().Format(time.RFC3339),
		SessionKey:       record.StarterDID,
		Requester: authoobprovev0_3.Requester{
			Location: "unknown", Browser: record.RequesterBrowser, Os: record.RequesterOS,
			CreatedAt: record.CreatedAt.UTC().Format(time.RFC3339), SameNetwork: sameNetwork,
		},
		IdentifiedAs: *identify.Issuer,
	}
	response, err := signedOOBResponse(h, request, payload, authoobprovev0_3.ResponsePayloadSchemaJSON)
	if err != nil {
		return nil, err
	}
	digest, err := farmtrusttask.DigestMultibase(response)
	if err != nil {
		return nil, err
	}
	identified := *identify.Issuer
	updates := map[string]any{"state": "identified", "identified_did": identified, "step2_digest": digest}
	if len(bindings) == 1 {
		updates["binding_id"] = bindings[0].ID
		updates["user_id"] = bindings[0].UserID
	}
	updated := h.db.WithContext(ctx).Model(&model.WalletLoginRequest{}).
		Where("request_id = ? AND state = 'claimed' AND approver_did = ?", record.RequestID, *parties.Issuer).
		Updates(updates)
	if updated.Error != nil {
		return nil, updated.Error
	}
	if updated.RowsAffected != 1 {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:requestExpired"), "sign-in request has expired", false, nil)
	}
	return response, nil
}

func (h *TrustTaskHandler) oobRespond(ctx context.Context, request *trusttasks.Document[authoobrespondv0_3.Payload], parties trusttasks.ResolvedParties) (*trusttasks.Document[authoobrespondv0_3.Response], error) {
	grantRaw, err := json.Marshal(request.Payload.Grant)
	if err != nil {
		return nil, err
	}
	var grant trusttasks.Document[authoobgrantv0_3.Payload]
	if err := json.Unmarshal(grantRaw, &grant); err != nil || grant.Issuer == nil {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
	}
	var record model.WalletLoginRequest
	if err := h.db.WithContext(ctx).First(&record, "request_id = ?", grant.Payload.RequestID).Error; err != nil {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:requestNotFound"), "no such sign-in request", false, nil)
	}
	if parties.Issuer == nil || record.ApproverDID == nil || *record.ApproverDID != *parties.Issuer || record.IdentifiedDID == nil || *record.IdentifiedDID != *grant.Issuer || record.State != "identified" || record.DecisionClosesAt == nil || !time.Now().UTC().Before(*record.DecisionClosesAt) {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
	}
	if err := validateCarried(h, ctx, grantRaw, &grant, authoobgrantv0_3.TypeURI, authoobgrantv0_3.PayloadSchemaJSON, "assertionMethod"); err != nil {
		h.declineWalletRequest(ctx, record.RequestID, "identified")
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
	}
	contextOK := grant.Payload.RequestID == record.RequestID && grant.Payload.ApproverKey == *parties.Issuer && grant.Payload.SessionKey == record.StarterDID && grant.Payload.Origin == record.Origin && record.Step2Digest != nil && grant.Payload.ContextDigest == *record.Step2Digest && grant.Payload.SigningScope == nil
	if !contextOK {
		h.declineWalletRequest(ctx, record.RequestID, "identified")
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob/respond:contextMismatch"), "grant does not match this sign-in request", false, nil)
	}
	sessionEndsAt, err := time.Parse(time.RFC3339, grant.Payload.SessionEndsAt)
	if err != nil || !sessionEndsAt.After(time.Now().UTC()) {
		h.declineWalletRequest(ctx, record.RequestID, "identified")
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
	}
	state := "declined"
	status := authoobrespondv0_3.ResponseStatusDeclined
	updates := map[string]any{"state": state, "ended_at": time.Now().UTC(), "grant_document": grantRaw}
	if grant.Payload.Decision == authoobgrantv0_3.PayloadDecisionApprove {
		var count int64
		query := h.db.WithContext(ctx).Model(&model.WalletLoginBinding{}).Where("login_did = ? AND revoked_at IS NULL", *grant.Issuer)
		if record.BindingID != nil && record.UserID != nil {
			query = query.Where("id = ? AND user_id = ?", *record.BindingID, *record.UserID)
		}
		if query.Count(&count).Error != nil || count == 0 {
			h.declineWalletRequest(ctx, record.RequestID, "identified")
			return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
		}
		state = "approved"
		status = authoobrespondv0_3.ResponseStatusApproved
		updates = map[string]any{"state": state, "grant_document": grantRaw, "session_ends_at": sessionEndsAt}
	}
	updated := h.db.WithContext(ctx).Model(&model.WalletLoginRequest{}).
		Where("request_id = ? AND state = 'identified'", record.RequestID).Updates(updates)
	if updated.Error != nil {
		return nil, updated.Error
	}
	if updated.RowsAffected != 1 {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:alreadyDecided"), "sign-in request has already been decided", false, nil)
	}
	return signedOOBResponse(h, request, authoobrespondv0_3.Response{Status: status}, authoobrespondv0_3.ResponsePayloadSchemaJSON)
}

func (h *TrustTaskHandler) oobRedeem(ctx context.Context, c *gin.Context, request *trusttasks.Document[authoobredeemv0_2.Payload], parties trusttasks.ResolvedParties) (*trusttasks.Document[authoobredeemv0_2.Response], error) {
	if parties.Issuer == nil {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notStarter"), "only the starter can redeem this request", false, nil)
	}
	var record model.WalletLoginRequest
	if err := h.db.WithContext(ctx).First(&record, "request_id = ?", request.Payload.RequestID).Error; err != nil {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:requestNotFound"), "no such sign-in request", false, nil)
	}
	if record.StarterDID != *parties.Issuer {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notStarter"), "only the starter can redeem this request", false, nil)
	}
	selectedBinding, err := walletSelectedBinding(request.Payload.Ext)
	if err != nil {
		return nil, trustTaskRefuse(h, request, trusttasks.CodeMalformedRequest, "invalid Farm account selection", false, nil)
	}
	if selectedBinding != nil {
		if record.State == "consumed" && record.BindingID != nil && *record.BindingID == *selectedBinding {
			// A retry after a lost success response must be able to recover the
			// same cookie without changing the already-consumed account choice.
		} else if record.IdentifiedDID == nil || (record.State != "identified" && record.State != "approved") {
			return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob/redeem:contextMismatch"), "account selection is not available for this request", false, nil)
		} else {
			var binding model.WalletLoginBinding
			if err := h.db.WithContext(ctx).First(&binding, "id = ? AND login_did = ? AND revoked_at IS NULL", *selectedBinding, *record.IdentifiedDID).Error; err != nil {
				return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
			}
			updated := h.db.WithContext(ctx).Model(&model.WalletLoginRequest{}).
				Where("request_id = ? AND state IN ?", record.RequestID, []string{"identified", "approved"}).
				Updates(map[string]any{"binding_id": binding.ID, "user_id": binding.UserID})
			if updated.Error != nil || updated.RowsAffected != 1 {
				return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:alreadyDecided"), "sign-in request has already ended", false, nil)
			}
			record.BindingID = &binding.ID
			record.UserID = &binding.UserID
		}
	}
	now := time.Now().UTC()
	if (record.State == "pending" && !now.Before(record.ClaimClosesAt)) || ((record.State == "claimed" || record.State == "identified") && (record.DecisionClosesAt == nil || !now.Before(*record.DecisionClosesAt))) {
		h.db.WithContext(ctx).Model(&record).Where("state IN ?", []string{"pending", "claimed", "identified"}).Updates(map[string]any{"state": "expired", "ended_at": now})
		record.State = "expired"
	}
	if walletRedeemPending(record.State) && !(record.State == "identified" && record.BindingID == nil) && h.redeemHold > 0 && (record.MatchNumber == nil || record.MatchNumberDelivered) {
		initialState := record.State
		acquired, err := walletauth.AcquireRedeemPoll(ctx, h.db, record.RequestID, request.ID, h.redeemHold+5*time.Second)
		if err != nil {
			return nil, err
		}
		if !acquired {
			if err := h.db.WithContext(ctx).First(&record, "request_id = ?", record.RequestID).Error; err != nil {
				return nil, err
			}
			if walletRedeemPending(record.State) {
				return nil, trustTaskRefuse(h, request, trusttasks.CodeUnavailable, "another redeem poll is already open", true, map[string]json.RawMessage{"retryAfter": mustJSON(1)})
			}
		} else {
			defer func() {
				releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = walletauth.ReleaseRedeemPoll(releaseCtx, h.db, record.RequestID, request.ID)
			}()
			timer := time.NewTimer(h.redeemHold)
			defer timer.Stop()
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
		poll:
			for {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-timer.C:
					break poll
				case <-ticker.C:
					if err := h.db.WithContext(ctx).First(&record, "request_id = ?", record.RequestID).Error; err != nil {
						return nil, err
					}
					now = time.Now().UTC()
					if (record.State == "pending" && !now.Before(record.ClaimClosesAt)) || ((record.State == "claimed" || record.State == "identified") && (record.DecisionClosesAt == nil || !now.Before(*record.DecisionClosesAt))) {
						h.db.WithContext(ctx).Model(&record).Where("state IN ?", []string{"pending", "claimed", "identified"}).Updates(map[string]any{"state": "expired", "ended_at": now})
						record.State = "expired"
					}
					if record.State != initialState || !walletRedeemPending(record.State) {
						break poll
					}
				}
			}
		}
	}
	switch record.State {
	case "pending", "claimed", "identified":
		details := map[string]json.RawMessage{"state": mustJSON(record.State)}
		if record.MatchNumber != nil {
			details["matchNumber"] = mustJSON(*record.MatchNumber)
			if !record.MatchNumberDelivered {
				h.db.WithContext(ctx).Model(&record).Update("match_number_delivered", true)
			}
		}
		if record.State == "identified" && record.BindingID == nil && record.IdentifiedDID != nil {
			accounts, err := h.walletAccountChoices(ctx, *record.IdentifiedDID)
			if err != nil {
				return nil, err
			}
			if len(accounts) == 0 {
				return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
			}
			details["accounts"] = mustJSON(accounts)
		}
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob/redeem:pending"), "waiting for Keyring approval", true, details)
	case "declined", "cancelled":
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob/redeem:declined"), "sign-in was declined", false, map[string]json.RawMessage{"state": mustJSON(record.State)})
	case "expired":
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:requestExpired"), "sign-in request has expired", false, nil)
	case "approved", "consumed":
		if record.State == "approved" && record.BindingID == nil && record.IdentifiedDID != nil {
			accounts, err := h.walletAccountChoices(ctx, *record.IdentifiedDID)
			if err != nil {
				return nil, err
			}
			if len(accounts) == 0 {
				return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
			}
			return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob/redeem:accountSelectionRequired"), "select the Farm account to continue", true, map[string]json.RawMessage{"state": mustJSON(record.State), "accounts": mustJSON(accounts)})
		}
		return h.redeemApproved(ctx, c, request, &record)
	default:
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:alreadyDecided"), "sign-in request has already ended", false, nil)
	}
}

func walletRedeemPending(state string) bool {
	return state == "pending" || state == "claimed" || state == "identified"
}

type walletAccountChoice struct {
	BindingID   uint64 `json:"bindingId"`
	DisplayName string `json:"displayName"`
}

func (h *TrustTaskHandler) walletAccountChoices(ctx context.Context, loginDID string) ([]walletAccountChoice, error) {
	var choices []walletAccountChoice
	err := h.db.WithContext(ctx).Table("wallet_login_bindings AS wb").
		Select("wb.id AS binding_id, u.unique_id AS display_name").
		Joins("JOIN users AS u ON u.id = wb.user_id").
		Where("wb.login_did = ? AND wb.revoked_at IS NULL", loginDID).
		Order("wb.id ASC").Scan(&choices).Error
	return choices, err
}

func walletSelectedBinding(ext *authoobredeemv0_2.Ext) (*uint64, error) {
	if ext == nil {
		return nil, nil
	}
	raw, ok := (*ext)["com.ic3software.vtafarm"]
	if !ok {
		return nil, nil
	}
	var selection struct {
		BindingID uint64 `json:"bindingId"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&selection); err != nil || selection.BindingID == 0 {
		return nil, errors.New("invalid binding selection")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid binding selection")
	}
	return &selection.BindingID, nil
}

func (h *TrustTaskHandler) redeemApproved(ctx context.Context, c *gin.Context, request *trusttasks.Document[authoobredeemv0_2.Payload], record *model.WalletLoginRequest) (*trusttasks.Document[authoobredeemv0_2.Response], error) {
	if record.BindingID == nil || record.UserID == nil || record.IdentifiedDID == nil || record.SessionEndsAt == nil {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
	}
	var user model.User
	var walletSession model.WalletLoginSession
	var token string
	var sessionEnd time.Time
	err := h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current model.WalletLoginRequest
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "request_id = ?", record.RequestID).Error; err != nil {
			return err
		}
		var binding model.WalletLoginBinding
		if err := tx.First(&binding, "id = ? AND user_id = ? AND login_did = ? AND revoked_at IS NULL", *record.BindingID, *record.UserID, *record.IdentifiedDID).Error; err != nil {
			return err
		}
		if err := tx.First(&user, binding.UserID).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		sessionEnd = *record.SessionEndsAt
		if limit := now.Add(oobSessionLimit); sessionEnd.After(limit) {
			sessionEnd = limit
		}
		if !sessionEnd.After(now) {
			return walletStateError("requestExpired")
		}
		if current.State == "approved" {
			walletSession = model.WalletLoginSession{ID: uuid.NewString(), BindingID: binding.ID, UserID: binding.UserID, ExpiresAt: sessionEnd, CreatedAt: now}
			if err := tx.Create(&walletSession).Error; err != nil {
				return err
			}
			if err := tx.Model(&current).Updates(map[string]any{"state": "consumed", "ended_at": now, "wallet_session_id": walletSession.ID}).Error; err != nil {
				return err
			}
		} else if current.State == "consumed" && current.WalletSessionID != nil {
			if err := tx.First(&walletSession, "id = ? AND revoked_at IS NULL AND expires_at > ?", *current.WalletSessionID, now).Error; err != nil {
				return err
			}
			sessionEnd = walletSession.ExpiresAt
		} else {
			return walletStateError("alreadyDecided")
		}
		var err error
		token, err = middleware.GenerateWalletToken(user.ID, h.jwtSecret, walletSession.ID, sessionEnd)
		return err
	})
	if err != nil {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
	}
	maxAge := int(time.Until(sessionEnd).Seconds())
	if maxAge < 1 {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:requestExpired"), "sign-in request has expired", false, nil)
	}
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(middleware.CookieUser, token, maxAge, "/", "", h.cookieSecure, true)
	ext := authoobredeemv0_2.Ext{
		"com.ic3software.vtafarm": mustJSON(gin.H{"user": gin.H{"id": user.ID, "unique_id": user.UniqueId, "role": model.RoleUser}}),
	}
	response := authoobredeemv0_2.Response{
		Subject: *record.IdentifiedDID, DisplayName: user.UniqueId,
		SessionEndsAt: sessionEnd.UTC().Format(time.RFC3339), Amr: []string{"did", "oob", "uv"}, Ext: &ext,
	}
	return signedOOBResponse(h, request, response, authoobredeemv0_2.ResponsePayloadSchemaJSON)
}

func (h *TrustTaskHandler) oobCancel(ctx context.Context, request *trusttasks.Document[authoobcancelv0_1.Payload], parties trusttasks.ResolvedParties) (*trusttasks.Document[authoobcancelv0_1.Response], error) {
	if parties.Issuer == nil {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
	}
	var record model.WalletLoginRequest
	if err := h.db.WithContext(ctx).First(&record, "request_id = ?", request.Payload.RequestID).Error; err != nil {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:requestNotFound"), "no such sign-in request", false, nil)
	}
	if record.StarterDID != *parties.Issuer && (record.ApproverDID == nil || *record.ApproverDID != *parties.Issuer) {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:notAuthorized"), "sign-in was not authorized", false, nil)
	}
	if record.State != "cancelled" {
		updated := h.db.WithContext(ctx).Model(&record).Where("state IN ?", []string{"pending", "claimed", "identified", "approved"}).Updates(map[string]any{"state": "cancelled", "ended_at": time.Now().UTC()})
		if updated.Error != nil {
			return nil, updated.Error
		}
		if updated.RowsAffected != 1 {
			return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("auth/oob:alreadyDecided"), "sign-in request has already ended", false, nil)
		}
	}
	return signedOOBResponse(h, request, authoobcancelv0_1.Response{Status: authoobcancelv0_1.ResponseStatusCancelled}, authoobcancelv0_1.ResponsePayloadSchemaJSON)
}

func validateCarried[P any](h *TrustTaskHandler, ctx context.Context, raw json.RawMessage, document *trusttasks.Document[P], expectedType, schema, purpose string) error {
	if document.Type != expectedType || document.Issuer == nil || document.Recipient == nil || *document.Recipient != h.farmDID || document.IssuedAt == nil || document.ID == "" {
		return errors.New("invalid carried Trust Task envelope")
	}
	payload, err := json.Marshal(document.Payload)
	if err != nil || h.validator.Validate(schema, payload) != nil {
		return errors.New("invalid carried Trust Task payload")
	}
	issuedAt, err := time.Parse(time.RFC3339, *document.IssuedAt)
	if err != nil || issuedAt.Before(time.Now().Add(-10*time.Minute)) || issuedAt.After(time.Now().Add(time.Minute)) {
		return errors.New("carried Trust Task is stale")
	}
	if document.ExpiresAt != nil {
		expiresAt, err := time.Parse(time.RFC3339, *document.ExpiresAt)
		if err != nil || !time.Now().Before(expiresAt) {
			return errors.New("carried Trust Task has expired")
		}
	}
	return h.verifier.VerifyDIDKey(ctx, raw, *document.Issuer, purpose)
}

func (h *TrustTaskHandler) declineWalletRequest(ctx context.Context, requestID, from string) {
	now := time.Now().UTC()
	h.db.WithContext(ctx).Model(&model.WalletLoginRequest{}).
		Where("request_id = ? AND state = ?", requestID, from).
		Updates(map[string]any{"state": "declined", "ended_at": now})
}

type walletStateError string

func (e walletStateError) Error() string { return string(e) }

func randomRequestID() (string, error) {
	raw := make([]byte, 16)
	if _, err := cryptorand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func randomMatchNumber() (string, error) {
	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(100))
	if err != nil {
		return "", err
	}
	value := n.Int64()
	return string([]byte{'0' + byte(value/10), '0' + byte(value%10)}), nil
}

func requesterAgent(userAgent string) (string, string) {
	ua := strings.ToLower(userAgent)
	browser := "unknown"
	switch {
	case strings.Contains(ua, "edg/"):
		browser = "Edge"
	case strings.Contains(ua, "chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "safari/"):
		browser = "Safari"
	}
	osName := "unknown"
	switch {
	case strings.Contains(ua, "iphone"), strings.Contains(ua, "ipad"):
		osName = "iOS"
	case strings.Contains(ua, "android"):
		osName = "Android"
	case strings.Contains(ua, "mac os"):
		osName = "macOS"
	case strings.Contains(ua, "windows"):
		osName = "Windows"
	case strings.Contains(ua, "linux"):
		osName = "Linux"
	}
	return browser, osName
}
