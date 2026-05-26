package router

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ic3software/cipherportal-api/internal/handler"
	"github.com/ic3software/cipherportal-api/internal/k8s"
)

func Setup(db *gorm.DB, k8sClient *k8s.Client) *gin.Engine {
	r := gin.Default()

	r.GET("/health", handler.Health)

	v1 := r.Group("/api/v1")
	{
		ph := handler.NewPodHandler(db, k8sClient)
		pods := v1.Group("/pods")
		{
			pods.POST("", ph.Create)
			pods.GET("", ph.List)
			pods.GET("/:name", ph.Get)
			pods.DELETE("/:name", ph.Delete)
		}
	}

	return r
}
