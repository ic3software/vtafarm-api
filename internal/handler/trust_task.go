package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/model"
	farmtrusttask "github.com/ic3software/vtafarm-api/internal/trusttask"
	"github.com/ic3software/vtafarm-api/internal/webvh"
	provisionvtaclaimv0_1 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/provision/vta/claim/v0_1"
	provisionvtaredeemv0_1 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/provision/vta/redeem/v0_1"
	trusttasknextstepv0_1 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/trust-task-next-step/v0_1"
	"github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/trusttasks"
	"gorm.io/gorm"
)

const trustTaskBodyLimit = 64 << 10
const defaultRedeemHold = 25 * time.Second

type TrustTaskHandler struct {
	db         *gorm.DB
	farmDID    string
	signer     *farmtrusttask.Signer
	verifier   *farmtrusttask.WalletVerifier
	validator  farmtrusttask.SchemaValidator
	guard      *farmtrusttask.ReplayGuard
	redeemHold time.Duration
	enabled    bool
}

type farmDocumentResolver interface {
	ResolveDocument(context.Context, string) ([]byte, error)
}

func NewTrustTaskHandler(db *gorm.DB, cfg config.VTAClaimConfig) *TrustTaskHandler {
	return newTrustTaskHandler(db, cfg, webvh.NewResolver(webvh.DefaultTimeout))
}

func newTrustTaskHandler(db *gorm.DB, cfg config.VTAClaimConfig, resolver farmDocumentResolver) *TrustTaskHandler {
	farmDID := strings.TrimSpace(cfg.FarmDID)
	h := &TrustTaskHandler{db: db, farmDID: farmDID, verifier: farmtrusttask.NewWalletVerifier(), guard: farmtrusttask.NewReplayGuard(db), redeemHold: defaultRedeemHold}
	base, baseErr := url.Parse(strings.TrimSpace(cfg.TrustTaskBase))
	signer, signerErr := farmtrusttask.NewSigner(farmDID, cfg.FarmSigningKey)
	if baseErr != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.RawPath != "" || strings.TrimSuffix(base.Path, "/") != "/trust-task" || signerErr != nil || db == nil || resolver == nil {
		return h
	}
	ctx, cancel := context.WithTimeout(context.Background(), webvh.DefaultTimeout)
	defer cancel()
	document, err := resolver.ResolveDocument(ctx, farmDID)
	if err != nil || farmtrusttask.ValidateFarmDIDDocument(document, signer, cfg.TrustTaskBase) != nil {
		return h
	}
	h.signer = signer
	h.enabled = true
	return h
}

func (h *TrustTaskHandler) Enabled() bool { return h != nil && h.enabled }

func (h *TrustTaskHandler) Consume(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !h.enabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Trust Task service is not configured."})
		return
	}
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "Content-Type must be application/json."})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, trustTaskBodyLimit)
	var raw json.RawMessage
	decoder := json.NewDecoder(c.Request.Body)
	if err := decoder.Decode(&raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Malformed Trust Task document."})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Expected one Trust Task document."})
		return
	}
	var envelope struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Type == "" || len(envelope.Payload) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Malformed Trust Task document."})
		return
	}

	switch envelope.Type {
	case provisionvtaclaimv0_1.TypeURI:
		if err := h.validator.Validate(provisionvtaclaimv0_1.PayloadSchemaJSON, envelope.Payload); err != nil {
			h.emitMalformed(c, raw, err)
			return
		}
		h.consumeClaim(c, raw)
	case provisionvtaredeemv0_1.TypeURI:
		if err := h.validator.Validate(provisionvtaredeemv0_1.PayloadSchemaJSON, envelope.Payload); err != nil {
			h.emitMalformed(c, raw, err)
			return
		}
		h.consumeRedeem(c, raw)
	default:
		h.emitUnsupported(c, raw)
	}
}

func (h *TrustTaskHandler) consumeClaim(c *gin.Context, raw json.RawMessage) {
	var document trusttasks.Document[provisionvtaclaimv0_1.Payload]
	if err := json.Unmarshal(raw, &document); err != nil {
		h.emitMalformed(c, raw, err)
		return
	}
	outcome, err := trusttasks.ConsumeInbound(c.Request.Context(), trusttasks.ConsumeOptions[provisionvtaclaimv0_1.Payload, trusttasknextstepv0_1.Payload]{
		Transport:     trusttasks.UnauthenticatedTransport{URI: "https://trusttasks.org/bindings/https/0.2"},
		Spec:          provisionvtaclaimv0_1.Spec,
		ProofPolicy:   trusttasks.ProofPolicy{Kind: trusttasks.ProofVerify, Verifier: h.verifier},
		PayloadPolicy: trusttasks.PayloadPolicy{Kind: trusttasks.PayloadValidate, Validator: h.validator},
		Checks:        trusttasks.ConsequentialChecks(h.guard),
		Doc:           &document,
		MyVID:         h.farmDID,
		Now:           time.Now(),
		NewErrorID:    uuid.NewString,
		Handler: func(ctx context.Context, request *trusttasks.Document[provisionvtaclaimv0_1.Payload], parties trusttasks.ResolvedParties) (*trusttasks.Document[trusttasknextstepv0_1.Payload], error) {
			if request.ParentThreadID == nil || *request.ParentThreadID != request.Payload.Handle {
				return nil, trustTaskRefuse(h, request, trusttasks.CodeMalformedRequest, "parentThreadId must equal payload.handle", false, nil)
			}
			if parties.Issuer == nil {
				return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("provision/vta:keyUnsupported"), "claimant key is unsupported", false, nil)
			}
			nextStep, err := h.nextStep(request)
			if err != nil {
				return nil, err
			}
			responseBytes, err := json.Marshal(nextStep)
			if err != nil {
				return nil, err
			}
			_, err = connection.ClaimAndRecord(ctx, h.db, request.Payload.Handle, *parties.Issuer, request.ID, responseBytes)
			if err != nil {
				switch {
				case errors.Is(err, gorm.ErrRecordNotFound):
					return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("provision/vta:handleNotFound"), "reservation handle was not found", false, nil)
				case errors.Is(err, connection.ErrExpired):
					return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("provision/vta:handleExpired"), "reservation handle has expired", false, nil)
				case errors.Is(err, connection.ErrAlreadyClaimed), errors.Is(err, connection.ErrConflict):
					return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("provision/vta/claim:alreadyClaimed"), "reservation handle has already been claimed", false, nil)
				case errors.Is(err, connection.ErrInvalidDID):
					return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("provision/vta:keyUnsupported"), "claimant key is unsupported", false, nil)
				default:
					return nil, err
				}
			}
			return nextStep, nil
		},
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Unable to process Trust Task."})
		return
	}
	emitTrustTaskOutcome(h, c, outcome)
}

func (h *TrustTaskHandler) consumeRedeem(c *gin.Context, raw json.RawMessage) {
	var document trusttasks.Document[provisionvtaredeemv0_1.Payload]
	if err := json.Unmarshal(raw, &document); err != nil {
		h.emitMalformed(c, raw, err)
		return
	}
	outcome, err := trusttasks.ConsumeInbound(c.Request.Context(), trusttasks.ConsumeOptions[provisionvtaredeemv0_1.Payload, provisionvtaredeemv0_1.Response]{
		Transport:     trusttasks.UnauthenticatedTransport{URI: "https://trusttasks.org/bindings/https/0.2"},
		Spec:          provisionvtaredeemv0_1.Spec,
		ProofPolicy:   trusttasks.ProofPolicy{Kind: trusttasks.ProofVerify, Verifier: h.verifier},
		PayloadPolicy: trusttasks.PayloadPolicy{Kind: trusttasks.PayloadValidate, Validator: h.validator},
		Checks:        trusttasks.ConsequentialChecks(h.guard),
		Doc:           &document,
		MyVID:         h.farmDID,
		Now:           time.Now(),
		NewErrorID:    uuid.NewString,
		Handler: func(ctx context.Context, request *trusttasks.Document[provisionvtaredeemv0_1.Payload], parties trusttasks.ResolvedParties) (*trusttasks.Document[provisionvtaredeemv0_1.Response], error) {
			return h.redeem(ctx, request, parties)
		},
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Unable to process Trust Task."})
		return
	}
	emitTrustTaskOutcome(h, c, outcome)
}

func (h *TrustTaskHandler) redeem(ctx context.Context, request *trusttasks.Document[provisionvtaredeemv0_1.Payload], parties trusttasks.ResolvedParties) (*trusttasks.Document[provisionvtaredeemv0_1.Response], error) {
	reservation, session, err := h.loadRedeemState(ctx, request.Payload.Handle)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("provision/vta:handleNotFound"), "reservation handle was not found", false, nil)
		}
		return nil, err
	}
	if parties.Issuer == nil || reservation.ClaimantDigest == nil || connection.ClaimantDigest(*parties.Issuer) != *reservation.ClaimantDigest {
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("provision/vta:notClaimant"), "redeem issuer is not the claimant", false, nil)
	}
	if err := connection.Reconcile(ctx, h.db, reservation, session); err != nil {
		return nil, err
	}
	if !redeemPending(reservation.Status) || h.redeemHold <= 0 {
		return h.redeemOutcome(request, reservation, session)
	}

	acquired, err := connection.AcquireRedeemPoll(ctx, h.db, reservation.ID, request.ID, h.redeemHold+5*time.Second)
	if err != nil {
		return nil, err
	}
	if !acquired {
		reservation, session, err = h.loadRedeemState(ctx, request.Payload.Handle)
		if err != nil {
			return nil, err
		}
		if err := connection.Reconcile(ctx, h.db, reservation, session); err != nil {
			return nil, err
		}
		if redeemPending(reservation.Status) {
			return nil, trustTaskRefuse(h, request, trusttasks.CodeUnavailable, "another redeem poll is already open", true, map[string]json.RawMessage{"retryAfter": mustJSON(1)})
		}
		return h.redeemOutcome(request, reservation, session)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = connection.ReleaseRedeemPoll(releaseCtx, h.db, reservation.ID, request.ID)
	}()

	timer := time.NewTimer(h.redeemHold)
	defer timer.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return h.redeemOutcome(request, reservation, session)
		case <-ticker.C:
			reservation, session, err = h.loadRedeemState(ctx, request.Payload.Handle)
			if err != nil {
				return nil, err
			}
			if err := connection.Reconcile(ctx, h.db, reservation, session); err != nil {
				return nil, err
			}
			if !redeemPending(reservation.Status) {
				return h.redeemOutcome(request, reservation, session)
			}
		}
	}
}

func (h *TrustTaskHandler) loadRedeemState(ctx context.Context, handle string) (*model.MobileConnection, *model.SetupSession, error) {
	var reservation model.MobileConnection
	if err := h.db.WithContext(ctx).First(&reservation, "claim_handle = ?", handle).Error; err != nil {
		return nil, nil, err
	}
	var session model.SetupSession
	if err := h.db.WithContext(ctx).First(&session, reservation.SessionID).Error; err != nil {
		return nil, nil, err
	}
	return &reservation, &session, nil
}

func redeemPending(status string) bool {
	return status == "claimed" || status == "confirmed" || status == "provisioning"
}

func (h *TrustTaskHandler) redeemOutcome(request *trusttasks.Document[provisionvtaredeemv0_1.Payload], reservation *model.MobileConnection, session *model.SetupSession) (*trusttasks.Document[provisionvtaredeemv0_1.Response], error) {
	switch reservation.Status {
	case "claimed", "confirmed", "provisioning":
		details := map[string]json.RawMessage{"state": mustJSON(reservation.Status)}
		if reservation.Status == "claimed" && reservation.ConfirmBy != nil {
			details["confirmBy"] = mustJSON(reservation.ConfirmBy.Unix())
		}
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("provision/vta/redeem:pending"), "claim is not ready for hand-over", true, details)
	case "rejected":
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("provision/vta/redeem:rejected"), "claim was rejected", false, nil)
	case "lapsed", "expired", "cancelled":
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("provision/vta:handleExpired"), "reservation handle has expired", false, nil)
	case "released":
		operation := provisionvtaredeemv0_1.ResponseOperationProvisionVta
		if reservation.Operation == connection.OperationGrantACL {
			operation = provisionvtaredeemv0_1.ResponseOperationGrantACL
		}
		payload := provisionvtaredeemv0_1.Response{Handle: request.Payload.Handle, VtaDID: reservation.VtaDid, Operation: operation}
		if session.MediatorDid != "" {
			payload.MediatorDID = &session.MediatorDid
		}
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		if err := h.validator.Validate(provisionvtaredeemv0_1.ResponsePayloadSchemaJSON, payloadBytes); err != nil {
			return nil, err
		}
		response := trusttasks.RespondWith(request, uuid.NewString(), payload, nil)
		if err := farmtrusttask.SignDocument(h.signer, response); err != nil {
			return nil, err
		}
		return response, nil
	default:
		return nil, trustTaskRefuse(h, request, trusttasks.StandardCode("provision/vta:handleExpired"), "reservation is no longer redeemable", false, nil)
	}
}

func (h *TrustTaskHandler) nextStep(request *trusttasks.Document[provisionvtaclaimv0_1.Payload]) (*trusttasks.Document[trusttasknextstepv0_1.Payload], error) {
	hint := map[string]json.RawMessage{"handle": mustJSON(request.Payload.Handle)}
	continuation := json.RawMessage(`"proceed"`)
	issuedAt := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	threadID := request.ThreadID
	if threadID == nil {
		threadID = stringPointer(request.ID)
	}
	response := &trusttasks.Document[trusttasknextstepv0_1.Payload]{
		ID:             uuid.NewString(),
		ThreadID:       threadID,
		ParentThreadID: request.ParentThreadID,
		Type:           trusttasknextstepv0_1.TypeURI,
		Issuer:         stringPointer(h.farmDID),
		Recipient:      request.Issuer,
		IssuedAt:       &issuedAt,
		Payload: trusttasknextstepv0_1.Payload{
			Expects:      []trusttasknextstepv0_1.PayloadExpectsItem{{TypeURI: provisionvtaredeemv0_1.TypeURI, Hint: &hint}},
			Continuation: continuation,
			InResponseTo: &trusttasknextstepv0_1.PayloadInResponseTo{TypeURI: request.Type, ID: &request.ID},
		},
	}
	payloadBytes, err := json.Marshal(response.Payload)
	if err != nil {
		return nil, err
	}
	if err := h.validator.Validate(trusttasknextstepv0_1.PayloadSchemaJSON, payloadBytes); err != nil {
		return nil, err
	}
	if err := farmtrusttask.SignDocument(h.signer, response); err != nil {
		return nil, err
	}
	return response, nil
}

func trustTaskRefuse[P any](h *TrustTaskHandler, request *trusttasks.Document[P], code trusttasks.StandardCode, message string, retryable bool, details map[string]json.RawMessage) error {
	refusal := trusttasks.Refuse(request, uuid.NewString(), trusttasks.RejectReason{Code: code, Message: message, Retryable: retryable, Details: details}, nil)
	if refusal.Response != nil {
		if err := farmtrusttask.SignDocument(h.signer, refusal.Response); err != nil {
			return err
		}
	}
	return refusal
}

func emitTrustTaskOutcome[R any](h *TrustTaskHandler, c *gin.Context, outcome trusttasks.ConsumeOutcome[R]) {
	switch outcome.Kind {
	case trusttasks.OutcomeHandled:
		h.emit(c, http.StatusOK, outcome.Response)
	case trusttasks.OutcomeRejected:
		status := errorHTTPStatus(outcome.Error.Payload.Code)
		if outcome.Error.Proof == nil {
			if err := farmtrusttask.SignDocument(h.signer, outcome.Error); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Unable to sign Trust Task error."})
				return
			}
		}
		h.emit(c, status, outcome.Error)
	case trusttasks.OutcomeDuplicate:
		if len(outcome.PriorResponse) != 0 {
			c.Data(retainedHTTPStatus(outcome.PriorResponse), "application/json", outcome.PriorResponse)
		} else if outcome.InFlight {
			c.Status(http.StatusAccepted)
		} else {
			c.Status(http.StatusNoContent)
		}
	case trusttasks.OutcomeAccepted:
		c.Status(http.StatusNoContent)
	case trusttasks.OutcomeSuppressed:
		c.Status(http.StatusUnprocessableEntity)
	default:
		c.Status(http.StatusInternalServerError)
	}
}

func (h *TrustTaskHandler) emit(c *gin.Context, status int, document any) {
	encoded, err := json.Marshal(document)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Unable to encode Trust Task reply."})
		return
	}
	c.Data(status, "application/json", encoded)
}

func (h *TrustTaskHandler) emitMalformed(c *gin.Context, raw json.RawMessage, _ error) {
	h.emitGenericError(c, raw, trusttasks.CodeMalformedRequest, "payload does not conform to its schema", http.StatusBadRequest)
}

func (h *TrustTaskHandler) emitUnsupported(c *gin.Context, raw json.RawMessage) {
	h.emitGenericError(c, raw, trusttasks.CodeUnsupportedType, "Trust Task type is not supported", http.StatusUnprocessableEntity)
}

func (h *TrustTaskHandler) emitGenericError(c *gin.Context, raw json.RawMessage, code trusttasks.StandardCode, message string, status int) {
	var request trusttasks.Document[json.RawMessage]
	if err := json.Unmarshal(raw, &request); err != nil || request.ID == "" {
		c.JSON(status, gin.H{"error": message})
		return
	}
	response := trusttasks.RejectWith(&request, uuid.NewString(), trusttasks.ToErrorPayload(trusttasks.RejectReason{Code: code, Message: message}), nil)
	response.Issuer = stringPointer(h.farmDID)
	if err := farmtrusttask.SignDocument(h.signer, response); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Unable to sign Trust Task error."})
		return
	}
	h.emit(c, status, response)
}

func errorHTTPStatus(code string) int {
	switch trusttasks.NormalizeCode(code) {
	case string(trusttasks.CodeMalformedRequest):
		return http.StatusBadRequest
	case string(trusttasks.CodeIDConflict):
		return http.StatusConflict
	case string(trusttasks.CodeUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusUnprocessableEntity
	}
}

func retainedHTTPStatus(document json.RawMessage) int {
	var envelope struct {
		Type    string `json:"type"`
		Payload struct {
			Code string `json:"code"`
		} `json:"payload"`
	}
	if json.Unmarshal(document, &envelope) == nil && envelope.Type == trusttasks.TrustTaskErrorTypeURI {
		return errorHTTPStatus(envelope.Payload.Code)
	}
	return http.StatusOK
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func stringPointer(value string) *string { return &value }
