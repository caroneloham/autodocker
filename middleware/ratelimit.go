package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type rateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	maxTries int
	window   time.Duration
}

var loginLimiter = &rateLimiter{
	attempts: make(map[string][]time.Time),
	maxTries: 5,
	window:   5 * time.Minute,
}

// RateLimitLogin limits login attempts per IP
func RateLimitLogin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != "POST" {
			c.Next()
			return
		}

		ip := c.ClientIP()

		// Check rate limit — lock only for the map check, NOT during handler execution.
		// Previous bug: c.Next() was called while holding the mutex, meaning the
		// entire login handler (bcrypt, DB queries, session save) ran under lock.
		// If any request timed out or blocked, ALL subsequent login POSTs would deadlock.
		loginLimiter.mu.Lock()
		now := time.Now()
		cutoff := now.Add(-loginLimiter.window)

		// Clean old attempts
		valid := make([]time.Time, 0)
		for _, t := range loginLimiter.attempts[ip] {
			if t.After(cutoff) {
				valid = append(valid, t)
			}
		}
		loginLimiter.attempts[ip] = valid

		tooMany := len(valid) >= loginLimiter.maxTries
		loginLimiter.mu.Unlock() // release BEFORE calling handler

		if tooMany {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "Trop de tentatives. Reessayez dans 5 minutes.",
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// RecordLoginAttempt records a failed login
func RecordLoginAttempt(ip string) {
	loginLimiter.mu.Lock()
	defer loginLimiter.mu.Unlock()
	loginLimiter.attempts[ip] = append(loginLimiter.attempts[ip], time.Now())
}
