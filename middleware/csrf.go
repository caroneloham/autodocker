package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// CSRFProtection generates and validates CSRF tokens
func CSRFProtection() gin.HandlerFunc {
	exemptPaths := []string{"/login", "/logout", "/api/proxy/", "/api/agents/report"}

	return func(c *gin.Context) {
		// Skip non-mutating methods
		method := c.Request.Method
		if method == "GET" || method == "HEAD" || method == "OPTIONS" {
			c.Next()
			return
		}

		// Check exempt paths
		path := c.Request.URL.Path
		for _, exempt := range exemptPaths {
			if strings.HasPrefix(path, exempt) || path == exempt {
				c.Next()
				return
			}
		}

		// Validate CSRF token
		session := sessions.Default(c)
		sessionToken, _ := session.Get("csrf_token").(string)
		if sessionToken == "" {
			c.JSON(http.StatusForbidden, gin.H{"error": "Token CSRF requis"})
			c.Abort()
			return
		}

		// Check all possible header names (Go HTTP canonicalizes differently:
		// "X-CSRFToken" → "X-Csrftoken", "X-CSRF-Token" → "X-Csrf-Token")
		headerToken := c.GetHeader("X-CSRFToken")
		if headerToken == "" {
			headerToken = c.GetHeader("X-CSRF-Token")
		}
		if headerToken == "" {
			headerToken = c.PostForm("csrf_token")
		}

		if headerToken != sessionToken {
			c.JSON(http.StatusForbidden, gin.H{"error": "Token CSRF invalide"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// GenerateCSRFToken creates a new CSRF token and stores it in session
func GenerateCSRFToken(c *gin.Context) string {
	session := sessions.Default(c)
	token, _ := session.Get("csrf_token").(string)
	if token == "" {
		b := make([]byte, 32)
		rand.Read(b)
		token = hex.EncodeToString(b)
		session.Set("csrf_token", token)
		session.Save()
	}
	return token
}
