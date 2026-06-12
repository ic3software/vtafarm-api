package handler

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/passkey"
)

type PasskeyHandler struct {
	db           *gorm.DB
	wa           *webauthn.WebAuthn
	sessions     *passkey.SessionStore
	jwtSecret    string
	cookieDomain string
	cookieSecure bool
}

func NewPasskeyHandler(
	db *gorm.DB,
	wa *webauthn.WebAuthn,
	sessions *passkey.SessionStore,
	jwtSecret, cookieDomain string,
	cookieSecure bool,
) *PasskeyHandler {
	return &PasskeyHandler{
		db: db, wa: wa, sessions: sessions,
		jwtSecret: jwtSecret, cookieDomain: cookieDomain, cookieSecure: cookieSecure,
	}
}

// webAuthnUser adapts both admin and user accounts to the webauthn.User interface.
type webAuthnUser struct {
	id       uint
	role     string
	uniqueId string
	creds    []webauthn.Credential
}

// WebAuthnID encodes role (top bit) + id into 8 bytes to prevent ID collision
// between the admin and user tables (both start numbering from 1).
func (u *webAuthnUser) WebAuthnID() []byte {
	val := uint64(u.id)
	if u.role == model.RoleAdmin {
		val |= 1 << 63
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, val)
	return b
}
func (u *webAuthnUser) WebAuthnName() string                       { return u.uniqueId }
func (u *webAuthnUser) WebAuthnDisplayName() string                { return u.uniqueId }
func (u *webAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func decodeWebAuthnID(handle []byte) (role string, id uint, err error) {
	if len(handle) != 8 {
		return "", 0, fmt.Errorf("invalid user handle length %d", len(handle))
	}
	val := binary.BigEndian.Uint64(handle)
	if val&(1<<63) != 0 {
		return model.RoleAdmin, uint(val &^ (uint64(1) << 63)), nil
	}
	return model.RoleUser, uint(val), nil
}

func (h *PasskeyHandler) loadWebAuthnUser(role string, id uint) (*webAuthnUser, error) {
	if role == model.RoleAdmin {
		var admin model.Admin
		if err := h.db.First(&admin, id).Error; err != nil {
			return nil, err
		}
		var pks []model.AdminPasskey
		h.db.Where("admin_id = ?", id).Find(&pks)
		creds := make([]webauthn.Credential, 0, len(pks))
		for _, pk := range pks {
			var cred webauthn.Credential
			if json.Unmarshal(pk.Credential, &cred) == nil {
				creds = append(creds, cred)
			}
		}
		return &webAuthnUser{id: id, role: role, uniqueId: admin.UniqueId, creds: creds}, nil
	}
	var user model.User
	if err := h.db.First(&user, id).Error; err != nil {
		return nil, err
	}
	var pks []model.UserPasskey
	h.db.Where("user_id = ?", id).Find(&pks)
	creds := make([]webauthn.Credential, 0, len(pks))
	for _, pk := range pks {
		var cred webauthn.Credential
		if json.Unmarshal(pk.Credential, &cred) == nil {
			creds = append(creds, cred)
		}
	}
	return &webAuthnUser{id: id, role: role, uniqueId: user.UniqueId, creds: creds}, nil
}

func contextIDAndRole(c *gin.Context) (uint, string) {
	uid, _ := c.Get(middleware.ContextUserID)
	role, _ := c.Get(middleware.ContextRole)
	return uid.(uint), role.(string)
}

// RegisterBegin — POST /api/v1/user/passkeys/register/begin
//               — POST /api/v1/admin/passkeys/register/begin
func (h *PasskeyHandler) RegisterBegin(c *gin.Context) {
	uid, role := contextIDAndRole(c)

	waUser, err := h.loadWebAuthnUser(role, uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "account not found"})
		return
	}

	exclusions := make([]protocol.CredentialDescriptor, 0, len(waUser.creds))
	for _, cred := range waUser.creds {
		exclusions = append(exclusions, protocol.CredentialDescriptor{
			Type:         protocol.PublicKeyCredentialType,
			CredentialID: cred.ID,
		})
	}

	opts, session, err := h.wa.BeginRegistration(waUser, webauthn.WithExclusions(exclusions))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not begin registration"})
		return
	}

	h.sessions.Set(fmt.Sprintf("reg:%s:%d", role, uid), session)
	c.JSON(http.StatusOK, opts)
}

// RegisterComplete — POST /api/v1/user/passkeys/register/complete?name=...
//                  — POST /api/v1/admin/passkeys/register/complete?name=...
func (h *PasskeyHandler) RegisterComplete(c *gin.Context) {
	uid, role := contextIDAndRole(c)

	session, ok := h.sessions.Get(fmt.Sprintf("reg:%s:%d", role, uid))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "registration session expired"})
		return
	}

	waUser, err := h.loadWebAuthnUser(role, uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "account not found"})
		return
	}

	cred, err := h.wa.FinishRegistration(waUser, *session, c.Request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "registration verification failed: " + err.Error()})
		return
	}

	credJSON, err := json.Marshal(cred)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not encode credential"})
		return
	}

	name := c.Query("name")
	if name == "" {
		name = "Passkey"
	}

	if role == model.RoleAdmin {
		pk := model.AdminPasskey{AdminID: uid, CredentialID: cred.ID, Credential: credJSON, Name: name}
		if err := h.db.Create(&pk).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not save passkey"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"id": pk.ID, "name": pk.Name})
	} else {
		pk := model.UserPasskey{UserID: uid, CredentialID: cred.ID, Credential: credJSON, Name: name}
		if err := h.db.Create(&pk).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not save passkey"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"id": pk.ID, "name": pk.Name})
	}
}

// List — GET /api/v1/user/passkeys
//       — GET /api/v1/admin/passkeys
func (h *PasskeyHandler) List(c *gin.Context) {
	uid, role := contextIDAndRole(c)

	type item struct {
		ID         uint64  `json:"id"`
		Name       string  `json:"name"`
		CreatedAt  string  `json:"created_at"`
		LastUsedAt *string `json:"last_used_at"`
	}
	toItem := func(id uint64, name string, createdAt time.Time, lastUsedAt *time.Time) item {
		it := item{ID: id, Name: name, CreatedAt: createdAt.UTC().Format(time.RFC3339)}
		if lastUsedAt != nil {
			s := lastUsedAt.UTC().Format(time.RFC3339)
			it.LastUsedAt = &s
		}
		return it
	}

	if role == model.RoleAdmin {
		var pks []model.AdminPasskey
		if err := h.db.Where("admin_id = ?", uid).Order("created_at asc").Find(&pks).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch passkeys"})
			return
		}
		result := make([]item, len(pks))
		for i, pk := range pks {
			result[i] = toItem(pk.ID, pk.Name, pk.CreatedAt, pk.LastUsedAt)
		}
		c.JSON(http.StatusOK, result)
	} else {
		var pks []model.UserPasskey
		if err := h.db.Where("user_id = ?", uid).Order("created_at asc").Find(&pks).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not fetch passkeys"})
			return
		}
		result := make([]item, len(pks))
		for i, pk := range pks {
			result[i] = toItem(pk.ID, pk.Name, pk.CreatedAt, pk.LastUsedAt)
		}
		c.JSON(http.StatusOK, result)
	}
}

// Delete — DELETE /api/v1/user/passkeys/:id
//        — DELETE /api/v1/admin/passkeys/:id
func (h *PasskeyHandler) Delete(c *gin.Context) {
	uid, role := contextIDAndRole(c)

	pkID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var res *gorm.DB
	if role == model.RoleAdmin {
		res = h.db.Where("id = ? AND admin_id = ?", pkID, uid).Delete(&model.AdminPasskey{})
	} else {
		res = h.db.Where("id = ? AND user_id = ?", pkID, uid).Delete(&model.UserPasskey{})
	}
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not delete passkey"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "passkey not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

// loginBegin is the shared implementation for admin and user passkey login begin.
func (h *PasskeyHandler) loginBegin(c *gin.Context) {
	opts, session, err := h.wa.BeginDiscoverableLogin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not begin login"})
		return
	}
	sessionID := uuid.New().String()
	h.sessions.Set(sessionID, session)
	c.JSON(http.StatusOK, gin.H{
		"session_id": sessionID,
		"publicKey":  opts.Response,
	})
}

// AdminLoginBegin — POST /api/v1/auth/admin/passkey/begin
func (h *PasskeyHandler) AdminLoginBegin(c *gin.Context) { h.loginBegin(c) }

// UserLoginBegin — POST /api/v1/auth/user/passkey/begin
func (h *PasskeyHandler) UserLoginBegin(c *gin.Context) { h.loginBegin(c) }

// AdminLoginComplete — POST /api/v1/auth/admin/passkey/complete?session_id=...
func (h *PasskeyHandler) AdminLoginComplete(c *gin.Context) {
	h.loginComplete(c, model.RoleAdmin)
}

// UserLoginComplete — POST /api/v1/auth/user/passkey/complete?session_id=...
func (h *PasskeyHandler) UserLoginComplete(c *gin.Context) {
	h.loginComplete(c, model.RoleUser)
}

// loginComplete is the shared implementation. expectedRole prevents an admin
// passkey from being accepted on the user login endpoint and vice versa.
func (h *PasskeyHandler) loginComplete(c *gin.Context, expectedRole string) {
	sessionID := c.Query("session_id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing session_id"})
		return
	}
	session, ok := h.sessions.Get(sessionID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "login session expired or not found"})
		return
	}

	var authenticatedUser *webAuthnUser

	_, cred, err := h.wa.FinishPasskeyLogin(
		func(rawID, userHandle []byte) (webauthn.User, error) {
			role, uid, err := decodeWebAuthnID(userHandle)
			if err != nil {
				return nil, err
			}
			waUser, err := h.loadWebAuthnUser(role, uid)
			if err != nil {
				return nil, err
			}
			authenticatedUser = waUser
			return waUser, nil
		},
		*session,
		c.Request,
	)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "passkey verification failed"})
		return
	}

	if authenticatedUser.role != expectedRole {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid account type for this endpoint"})
		return
	}

	// Persist updated sign count for replay-attack protection.
	if authenticatedUser.role == model.RoleAdmin {
		var pk model.AdminPasskey
		if h.db.Where("credential_id = ?", cred.ID).First(&pk).Error == nil {
			if credJSON, e := json.Marshal(cred); e == nil {
				h.db.Model(&pk).Updates(map[string]any{"credential": credJSON, "last_used_at": time.Now()})
			}
		}
	} else {
		var pk model.UserPasskey
		if h.db.Where("credential_id = ?", cred.ID).First(&pk).Error == nil {
			if credJSON, e := json.Marshal(cred); e == nil {
				h.db.Model(&pk).Updates(map[string]any{"credential": credJSON, "last_used_at": time.Now()})
			}
		}
	}

	token, err := middleware.GenerateToken(authenticatedUser.id, authenticatedUser.role, h.jwtSecret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not generate token"})
		return
	}

	c.SetSameSite(http.SameSiteStrictMode)
	if authenticatedUser.role == model.RoleAdmin {
		c.SetCookie(middleware.CookieAdmin, token, cookieMaxAge, "/", h.cookieDomain, h.cookieSecure, true)
	} else {
		c.SetCookie(middleware.CookieUser, token, cookieMaxAge, "/", h.cookieDomain, h.cookieSecure, true)
	}

	c.JSON(http.StatusOK, gin.H{
		"token": token,
		"user":  gin.H{"id": authenticatedUser.id, "unique_id": authenticatedUser.uniqueId, "role": authenticatedUser.role},
	})
}
