package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimit allows at most max requests per window per client IP. In-memory
// sliding window — per replica, which is fine for the single-replica API; it
// exists to blunt bulk abuse of public endpoints, not to be exact accounting.
func RateLimit(max int, window time.Duration) gin.HandlerFunc {
	var mu sync.Mutex
	hits := make(map[string][]time.Time)

	return func(c *gin.Context) {
		now := time.Now()
		ip := c.ClientIP()

		mu.Lock()
		recent := hits[ip][:0]
		for _, t := range hits[ip] {
			if now.Sub(t) < window {
				recent = append(recent, t)
			}
		}
		if len(recent) >= max {
			hits[ip] = recent
			mu.Unlock()
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests — try again later"})
			return
		}
		hits[ip] = append(recent, now)

		// Sweep other IPs' expired entries once the map grows — keeps memory
		// bounded without a background goroutine.
		if len(hits) > 4096 {
			for k, ts := range hits {
				live := ts[:0]
				for _, t := range ts {
					if now.Sub(t) < window {
						live = append(live, t)
					}
				}
				if len(live) == 0 {
					delete(hits, k)
				} else {
					hits[k] = live
				}
			}
		}
		mu.Unlock()

		c.Next()
	}
}
