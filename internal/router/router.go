package router

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/cloudflare"
	"github.com/ic3software/cipherportal-api/internal/handler"
)

func Setup(db *gorm.DB, cfClient *cloudflare.Client, appEnv string) *gin.Engine {
	r := gin.Default()

	r.GET("/health", handler.Health)

	// Public DID log hosting — no auth, served at the path did:webvh resolvers expect.
	sh := handler.NewSetupHandler(db, cfClient, appEnv)
	r.GET("/did/:subdomain/did.jsonl", sh.ServeDidLog)

	v1 := r.Group("/api/v1")
	{
		v1.POST("/setup/validate", sh.Validate)
		v1.POST("/setup", sh.Create)
		v1.DELETE("/setup/:id", sh.Delete)
	}

	return r
}
