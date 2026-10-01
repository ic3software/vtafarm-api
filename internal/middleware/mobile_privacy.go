package middleware

import (
	"log"

	"github.com/gin-gonic/gin"
)

// Gin's default recovery dumps request headers and paths. Mobile credentials
// must not enter that dump, even on a panic. Access logs skip these routes too.
func MobilePrivacy() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recover() != nil {
				log.Printf("mobile connection request failed unexpectedly")
				c.AbortWithStatusJSON(500, gin.H{"error": "Unable to process mobile connection."})
			}
		}()
		c.Next()
	}
}
