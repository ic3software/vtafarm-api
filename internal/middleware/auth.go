package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/ic3software/vtafarm-api/internal/model"
	"gorm.io/gorm"
)

const (
	ContextUserID = "user_id"
	ContextRole   = "user_role"

	CookieUser  = "vtafarm_user"
	CookieAdmin = "vtafarm_admin"
)

type Claims struct {
	UserID          uint   `json:"user_id"`
	Role            string `json:"role"`
	WalletSessionID string `json:"wallet_session_id,omitempty"`
	jwt.RegisteredClaims
}

func GenerateToken(userID uint, role, secret string) (string, error) {
	return generateToken(userID, role, secret, "", time.Now().Add(24*time.Hour))
}

func GenerateWalletToken(userID uint, secret, sessionID string, expiresAt time.Time) (string, error) {
	return generateToken(userID, model.RoleUser, secret, sessionID, expiresAt)
}

func generateToken(userID uint, role, secret, walletSessionID string, expiresAt time.Time) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:          userID,
		Role:            role,
		WalletSessionID: walletSessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

// AuthRequired validates the JWT from the named cookie, falling back to the
// Authorization: Bearer header for non-browser clients and testing.
func AuthRequired(db *gorm.DB, secret, cookieName string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var tokenStr string

		if cookie, err := c.Cookie(cookieName); err == nil && cookie != "" {
			tokenStr = cookie
		}

		if tokenStr == "" {
			if h := c.GetHeader("Authorization"); strings.HasPrefix(h, "Bearer ") {
				tokenStr = strings.TrimPrefix(h, "Bearer ")
			}
		}

		if tokenStr == "" {
			c.Header("Cache-Control", "no-store")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid token"})
			return
		}

		claims := &Claims{}
		token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(secret), nil
		})
		if err != nil || !token.Valid {
			c.Header("Cache-Control", "no-store")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		if claims.WalletSessionID != "" {
			var count int64
			err := db.WithContext(c.Request.Context()).Table("wallet_login_sessions AS ws").
				Joins("JOIN wallet_login_bindings AS wb ON wb.id = ws.binding_id").
				Where("ws.id = ? AND ws.user_id = ? AND ws.revoked_at IS NULL AND wb.revoked_at IS NULL AND ws.expires_at > ?", claims.WalletSessionID, claims.UserID, time.Now().UTC()).
				Count(&count).Error
			if err != nil || count != 1 {
				c.Header("Cache-Control", "no-store")
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "wallet session is no longer active"})
				return
			}
		}

		c.Set(ContextUserID, claims.UserID)
		c.Set(ContextRole, claims.Role)
		c.Next()
	}
}

func RequireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		r, _ := c.Get(ContextRole)
		if r != role {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		c.Next()
	}
}
