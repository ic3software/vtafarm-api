package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/siop"
)

const maxPendingSIOPChallengesPerDID = 5

type SIOPHandlerConfig struct {
	RPDID        string
	ChallengeTTL time.Duration
	ClockSkew    time.Duration
	MaxBodyBytes int64
}

type SIOPHandler struct {
	db           *gorm.DB
	resolver     siop.AuthenticationKeyResolver
	rpDID        string
	challengeTTL time.Duration
	clockSkew    time.Duration
	maxBodyBytes int64
	jwtSecret    string
	cookieSecure bool
	now          func() time.Time
}

func NewSIOPHandler(
	db *gorm.DB,
	resolver siop.AuthenticationKeyResolver,
	cfg SIOPHandlerConfig,
	jwtSecret string,
	cookieSecure bool,
) *SIOPHandler {
	return &SIOPHandler{
		db: db, resolver: resolver, rpDID: strings.TrimSpace(cfg.RPDID),
		challengeTTL: cfg.ChallengeTTL, clockSkew: cfg.ClockSkew,
		maxBodyBytes: cfg.MaxBodyBytes, jwtSecret: jwtSecret,
		cookieSecure: cookieSecure, now: time.Now,
	}
}

func (h *SIOPHandler) enabled() bool {
	return h != nil && h.db != nil && h.resolver != nil && h.rpDID != "" &&
		h.challengeTTL > 0 && h.clockSkew >= 0 && h.maxBodyBytes > 0
}

func (h *SIOPHandler) Metadata(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !h.enabled() {
		c.JSON(http.StatusOK, gin.H{"enabled": false, "rp_did": ""})
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": true, "rp_did": h.rpDID})
}

func (h *SIOPHandler) UserLoginChallenge(c *gin.Context) {
	h.createChallenge(c, model.RoleUser, model.SIOPPurposeLogin)
}

func (h *SIOPHandler) AdminLoginChallenge(c *gin.Context) {
	h.createChallenge(c, model.RoleAdmin, model.SIOPPurposeLogin)
}

func (h *SIOPHandler) UserLinkChallenge(c *gin.Context) {
	h.createChallenge(c, model.RoleUser, model.SIOPPurposeLink)
}

func (h *SIOPHandler) AdminLinkChallenge(c *gin.Context) {
	h.createChallenge(c, model.RoleAdmin, model.SIOPPurposeLink)
}

type siopChallengeRequest struct {
	DID string `json:"did"`
}

func (h *SIOPHandler) createChallenge(c *gin.Context, role, purpose string) {
	if !h.prepare(c) {
		return
	}
	var request siopChallengeRequest
	if err := h.decodeJSON(c, &request); err != nil || !validSIOPDID(request.DID) {
		h.respondError(c, http.StatusBadRequest, "invalid DID")
		return
	}

	challenge := model.SIOPChallenge{
		Purpose: purpose, AccountRole: role, ExpectedDID: request.DID,
		ExpiresAt: h.now().Add(h.challengeTTL),
	}
	if purpose == model.SIOPPurposeLink {
		accountID, contextRole := contextIDAndRole(c)
		if contextRole != role {
			h.respondError(c, http.StatusForbidden, "forbidden")
			return
		}
		if !h.accountHasPasskey(accountID, role) {
			h.respondError(c, http.StatusConflict, "register a passkey before linking a VTA Wallet identity")
			return
		}
		if role == model.RoleAdmin {
			challenge.AdminID = &accountID
		} else {
			challenge.UserID = &accountID
		}
	}

	nonce, err := randomOpaqueValue(32)
	if err != nil {
		h.respondError(c, http.StatusInternalServerError, "could not create challenge")
		return
	}
	sessionID, err := randomOpaqueValue(32)
	if err != nil {
		h.respondError(c, http.StatusInternalServerError, "could not create challenge")
		return
	}
	digest := sha256.Sum256([]byte(nonce))
	challenge.ID = sessionID
	challenge.NonceSHA256 = digest[:]

	err = h.db.Transaction(func(tx *gorm.DB) error {
		now := h.now()
		if err := tx.Where("expires_at <= ?", now).Delete(&model.SIOPChallenge{}).Error; err != nil {
			return err
		}
		var pending int64
		if err := tx.Model(&model.SIOPChallenge{}).
			Where("account_role = ? AND expected_did = ? AND expires_at > ?", role, request.DID, now).
			Count(&pending).Error; err != nil {
			return err
		}
		if pending >= maxPendingSIOPChallengesPerDID {
			return errTooManyPendingChallenges
		}
		return tx.Create(&challenge).Error
	})
	if errors.Is(err, errTooManyPendingChallenges) {
		h.respondError(c, http.StatusTooManyRequests, "too many pending challenges")
		return
	}
	if err != nil {
		h.respondError(c, http.StatusInternalServerError, "could not create challenge")
		return
	}

	log.Printf("siop challenge issued role=%s purpose=%s did_hash=%s", role, purpose, didAuditHash(request.DID))
	c.JSON(http.StatusOK, gin.H{
		"challenge":  nonce,
		"session_id": sessionID,
		"expires_at": challenge.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

var errTooManyPendingChallenges = errors.New("too many pending SIOP challenges")

type siopAuthenticateRequest struct {
	IDToken   string `json:"id_token"`
	SessionID string `json:"session_id"`
	Label     string `json:"label,omitempty"`
}

func (h *SIOPHandler) UserLoginAuthenticate(c *gin.Context) {
	h.authenticate(c, model.RoleUser, model.SIOPPurposeLogin)
}

func (h *SIOPHandler) AdminLoginAuthenticate(c *gin.Context) {
	h.authenticate(c, model.RoleAdmin, model.SIOPPurposeLogin)
}

func (h *SIOPHandler) UserLinkAuthenticate(c *gin.Context) {
	h.authenticate(c, model.RoleUser, model.SIOPPurposeLink)
}

func (h *SIOPHandler) AdminLinkAuthenticate(c *gin.Context) {
	h.authenticate(c, model.RoleAdmin, model.SIOPPurposeLink)
}

func (h *SIOPHandler) authenticate(c *gin.Context, role, purpose string) {
	if !h.prepare(c) {
		return
	}
	var request siopAuthenticateRequest
	if err := h.decodeJSON(c, &request); err != nil || request.IDToken == "" || request.SessionID == "" || len(request.Label) > 80 {
		h.respondError(c, http.StatusBadRequest, "invalid authentication request")
		return
	}

	challenge, err := h.consumeChallenge(request.SessionID)
	if err != nil || challenge.AccountRole != role || challenge.Purpose != purpose {
		h.respondError(c, http.StatusUnauthorized, "SIOP challenge is invalid or expired")
		return
	}
	if purpose == model.SIOPPurposeLink {
		accountID, contextRole := contextIDAndRole(c)
		if contextRole != role || !challengeBelongsTo(challenge, accountID, role) {
			h.respondError(c, http.StatusUnauthorized, "SIOP challenge is invalid or expired")
			return
		}
	}

	nonce, err := siop.UnverifiedNonce(request.IDToken)
	if err != nil || !nonceMatchesDigest(nonce, challenge.NonceSHA256) {
		h.auditFailure(role, purpose, challenge.ExpectedDID, siop.ErrorNonceMismatch)
		h.respondError(c, http.StatusUnauthorized, "SIOP verification failed")
		return
	}
	verified, err := siop.VerifyIDTokenAt(
		c.Request.Context(), request.IDToken, challenge.ExpectedDID, h.rpDID,
		nonce, h.resolver, h.now(), h.clockSkew,
	)
	if err != nil {
		h.auditFailure(role, purpose, challenge.ExpectedDID, siop.ErrorCodeOf(err))
		h.respondError(c, http.StatusUnauthorized, "SIOP verification failed")
		return
	}

	if purpose == model.SIOPPurposeLink {
		h.linkIdentity(c, challenge, verified, strings.TrimSpace(request.Label))
		return
	}
	h.loginIdentity(c, role, verified)
}

func (h *SIOPHandler) consumeChallenge(id string) (model.SIOPChallenge, error) {
	var challenge model.SIOPChallenge
	result := h.db.Raw(
		"DELETE FROM siop_challenges WHERE id = ? AND expires_at > ? RETURNING *",
		id, h.now(),
	).Scan(&challenge)
	if result.Error != nil {
		return challenge, result.Error
	}
	if result.RowsAffected != 1 || challenge.ID == "" {
		return challenge, gorm.ErrRecordNotFound
	}
	return challenge, nil
}

func (h *SIOPHandler) loginIdentity(c *gin.Context, role string, verified siop.VerifiedIDToken) {
	now := h.now()
	var accountID uint
	var uniqueID string
	if role == model.RoleAdmin {
		var identity model.AdminSIOPIdentity
		if err := h.db.Where("did = ?", verified.Subject).First(&identity).Error; err != nil {
			h.respondError(c, http.StatusUnauthorized, "this VTA Wallet identity is not linked to an admin account")
			return
		}
		var account model.Admin
		if err := h.db.First(&account, identity.AdminID).Error; err != nil {
			h.respondError(c, http.StatusUnauthorized, "SIOP authentication failed")
			return
		}
		accountID, uniqueID = account.ID, account.UniqueId
		h.db.Model(&identity).Updates(map[string]any{"last_authenticated_at": now, "last_kid": verified.Kid})
	} else {
		var identity model.UserSIOPIdentity
		if err := h.db.Where("did = ?", verified.Subject).First(&identity).Error; err != nil {
			h.respondError(c, http.StatusUnauthorized, "this VTA Wallet identity is not linked to a user account")
			return
		}
		var account model.User
		if err := h.db.First(&account, identity.UserID).Error; err != nil {
			h.respondError(c, http.StatusUnauthorized, "SIOP authentication failed")
			return
		}
		accountID, uniqueID = account.ID, account.UniqueId
		h.db.Model(&identity).Updates(map[string]any{"last_authenticated_at": now, "last_kid": verified.Kid})
	}

	token, err := middleware.GenerateToken(accountID, role, h.jwtSecret)
	if err != nil {
		h.respondError(c, http.StatusInternalServerError, "could not create login session")
		return
	}
	h.setCookie(c, role, token)
	log.Printf("siop authentication succeeded role=%s did_hash=%s", role, didAuditHash(verified.Subject))
	c.JSON(http.StatusOK, gin.H{"user": gin.H{"id": accountID, "unique_id": uniqueID, "role": role}})
}

func (h *SIOPHandler) linkIdentity(c *gin.Context, challenge model.SIOPChallenge, verified siop.VerifiedIDToken, label string) {
	accountID, _ := contextIDAndRole(c)
	if !h.accountHasPasskey(accountID, challenge.AccountRole) {
		h.respondError(c, http.StatusConflict, "register a passkey before linking a VTA Wallet identity")
		return
	}
	if label == "" {
		label = "VTA Wallet"
	}

	var value any
	if challenge.AccountRole == model.RoleAdmin {
		value = &model.AdminSIOPIdentity{AdminID: accountID, DID: verified.Subject, Label: label, LastKID: verified.Kid}
	} else {
		value = &model.UserSIOPIdentity{UserID: accountID, DID: verified.Subject, Label: label, LastKID: verified.Kid}
	}
	if err := h.db.Create(value).Error; err != nil {
		constraint := "user_siop_identities_did_unique"
		if challenge.AccountRole == model.RoleAdmin {
			constraint = "admin_siop_identities_did_unique"
		}
		if isUniqueViolation(err, constraint) {
			h.respondError(c, http.StatusConflict, "this VTA Wallet identity is already linked")
		} else {
			h.respondError(c, http.StatusInternalServerError, "could not link VTA Wallet identity")
		}
		return
	}
	log.Printf("siop identity linked role=%s did_hash=%s", challenge.AccountRole, didAuditHash(verified.Subject))
	c.JSON(http.StatusCreated, value)
}

func (h *SIOPHandler) UserIdentities(c *gin.Context)  { h.listIdentities(c, model.RoleUser) }
func (h *SIOPHandler) AdminIdentities(c *gin.Context) { h.listIdentities(c, model.RoleAdmin) }

func (h *SIOPHandler) listIdentities(c *gin.Context, role string) {
	if !h.prepare(c) {
		return
	}
	accountID, contextRole := contextIDAndRole(c)
	if contextRole != role {
		h.respondError(c, http.StatusForbidden, "forbidden")
		return
	}
	if role == model.RoleAdmin {
		var identities []model.AdminSIOPIdentity
		if err := h.db.Where("admin_id = ?", accountID).Order("created_at asc").Find(&identities).Error; err != nil {
			h.respondError(c, http.StatusInternalServerError, "could not fetch VTA Wallet identities")
			return
		}
		c.JSON(http.StatusOK, identities)
		return
	}
	var identities []model.UserSIOPIdentity
	if err := h.db.Where("user_id = ?", accountID).Order("created_at asc").Find(&identities).Error; err != nil {
		h.respondError(c, http.StatusInternalServerError, "could not fetch VTA Wallet identities")
		return
	}
	c.JSON(http.StatusOK, identities)
}

func (h *SIOPHandler) DeleteUserIdentity(c *gin.Context) {
	h.deleteIdentity(c, model.RoleUser)
}

func (h *SIOPHandler) DeleteAdminIdentity(c *gin.Context) {
	h.deleteIdentity(c, model.RoleAdmin)
}

func (h *SIOPHandler) deleteIdentity(c *gin.Context, role string) {
	if !h.prepare(c) {
		return
	}
	identityID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		h.respondError(c, http.StatusBadRequest, "invalid identity id")
		return
	}
	accountID, contextRole := contextIDAndRole(c)
	if contextRole != role {
		h.respondError(c, http.StatusForbidden, "forbidden")
		return
	}
	var result *gorm.DB
	if role == model.RoleAdmin {
		result = h.db.Where("id = ? AND admin_id = ?", identityID, accountID).Delete(&model.AdminSIOPIdentity{})
	} else {
		result = h.db.Where("id = ? AND user_id = ?", identityID, accountID).Delete(&model.UserSIOPIdentity{})
	}
	if result.Error != nil {
		h.respondError(c, http.StatusInternalServerError, "could not unlink VTA Wallet identity")
		return
	}
	if result.RowsAffected == 0 {
		h.respondError(c, http.StatusNotFound, "VTA Wallet identity not found")
		return
	}
	log.Printf("siop identity unlinked role=%s identity_id=%d", role, identityID)
	c.Status(http.StatusNoContent)
}

func (h *SIOPHandler) accountHasPasskey(accountID uint, role string) bool {
	var count int64
	query := h.db
	if role == model.RoleAdmin {
		query = query.Model(&model.AdminPasskey{}).Where("admin_id = ?", accountID)
	} else {
		query = query.Model(&model.UserPasskey{}).Where("user_id = ?", accountID)
	}
	return query.Count(&count).Error == nil && count > 0
}

func (h *SIOPHandler) prepare(c *gin.Context) bool {
	c.Header("Cache-Control", "no-store")
	if !h.enabled() {
		h.respondError(c, http.StatusServiceUnavailable, "VTA Wallet login is not enabled")
		return false
	}
	return true
}

func (h *SIOPHandler) decodeJSON(c *gin.Context, destination any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func (h *SIOPHandler) respondError(c *gin.Context, status int, message string) {
	c.Header("Cache-Control", "no-store")
	c.JSON(status, gin.H{"error": message})
}

func (h *SIOPHandler) setCookie(c *gin.Context, role, token string) {
	c.SetSameSite(http.SameSiteStrictMode)
	name := middleware.CookieUser
	if role == model.RoleAdmin {
		name = middleware.CookieAdmin
	}
	c.SetCookie(name, token, cookieMaxAge, "/", "", h.cookieSecure, true)
}

func (h *SIOPHandler) auditFailure(role, purpose, did string, code siop.ErrorCode) {
	log.Printf("siop verification failed role=%s purpose=%s did_hash=%s reason=%s", role, purpose, didAuditHash(did), code)
}

func challengeBelongsTo(challenge model.SIOPChallenge, accountID uint, role string) bool {
	if role == model.RoleAdmin {
		return challenge.AdminID != nil && *challenge.AdminID == accountID && challenge.UserID == nil
	}
	return challenge.UserID != nil && *challenge.UserID == accountID && challenge.AdminID == nil
}

func nonceMatchesDigest(nonce string, expected []byte) bool {
	if nonce == "" || len(expected) != sha256.Size {
		return false
	}
	digest := sha256.Sum256([]byte(nonce))
	return subtle.ConstantTimeCompare(digest[:], expected) == 1
}

func randomOpaqueValue(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func validSIOPDID(did string) bool {
	if did == "" || len(did) > 2048 || strings.TrimSpace(did) != did || strings.ContainsAny(did, " \t\r\n?#") {
		return false
	}
	return strings.HasPrefix(did, "did:key:") || strings.HasPrefix(did, "did:webvh:")
}

func didAuditHash(did string) string {
	digest := sha256.Sum256([]byte(did))
	return hex.EncodeToString(digest[:6])
}
