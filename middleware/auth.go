package middleware

import (
	"encoding/hex"
	"net/http"
	"strings"

	"autodocker-go/config"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// SessionRecovery silently clears corrupted/stale session cookies so the user
// always gets a clean login prompt instead of being stuck in a broken state.
func SessionRecovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		cookieHeader := c.GetHeader("Cookie")
		cookieName := config.SessionCookieName()
		if !strings.Contains(cookieHeader, cookieName+"=") {
			c.Next()
			return
		}

		session := sessions.Default(c)
		user := session.Get("user")

		// Cookie present but unreadable → silently nuke it on ALL paths including /login.
		// This ensures the login page always starts with a clean slate.
		if user == nil && session.Get("_totp_pending_user") == nil && session.Get("csrf_token") == nil {
			session.Clear()
			session.Options(sessions.Options{MaxAge: -1, Path: "/"})
			if err := session.Save(); err != nil {
				http.SetCookie(c.Writer, &http.Cookie{
					Name: cookieName, Value: "", Path: "/",
					MaxAge: -1, HttpOnly: true,
				})
			}
		}

		c.Next()
	}
}

// DemoSession injects a fake super_admin session in demo mode
func DemoSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		if config.DemoMode {
			session := sessions.Default(c)
			jobClient, _ := session.Get("infrastructure_job_client").(string)
			// Always force demo identity (avoids securecookie errors from stale cookies)
			session.Clear()
			if len(jobClient) == 32 {
				if _, err := hex.DecodeString(jobClient); err == nil {
					session.Set("infrastructure_job_client", jobClient)
				}
			}
			session.Set("user", "demo")
			session.Set("role", "super_admin")
			session.Save()
		}
		c.Next()
	}
}

// AuthBypassSession injects a local admin session while auth bypass is enabled.
func AuthBypassSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		if config.AuthBypass {
			session := sessions.Default(c)
			changed := false

			if session.Get("_totp_pending_user") != nil {
				session.Delete("_totp_pending_user")
				changed = true
			}
			if session.Get("_totp_setup_required") != nil {
				session.Delete("_totp_setup_required")
				changed = true
			}
			if session.Get("user") != config.AuthBypassUser {
				session.Set("user", config.AuthBypassUser)
				changed = true
			}
			if session.Get("role") != config.AuthBypassRole {
				session.Set("role", config.AuthBypassRole)
				changed = true
			}
			if changed {
				_ = session.Save()
			}
		}
		c.Next()
	}
}

// LoginRequired checks if user is authenticated
func LoginRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		if config.AuthBypass {
			c.Next()
			return
		}
		// Demo mode: skip auth entirely
		if config.DemoMode {
			c.Next()
			return
		}
		session := sessions.Default(c)
		user := session.Get("user")
		if user == nil {
			if isAPIRequest(c) {
				c.JSON(http.StatusUnauthorized, gin.H{"status": "error", "error": "Authentification requise"})
				c.Abort()
				return
			}
			c.Redirect(http.StatusFound, "/login")
			c.Abort()
			return
		}
		c.Next()
	}
}

// TOTPSetupRequired enforces MFA for admins: while the session carries the
// _totp_setup_required flag (admin logged in without TOTP enabled), every
// mutating request is blocked except the TOTP setup endpoints and logout.
// Read-only requests stay allowed so the UI can guide the user to the setup.
func TOTPSetupRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		if config.AuthBypass || config.DemoMode || config.LocalBootstrapMode() {
			c.Next()
			return
		}
		session := sessions.Default(c)
		if session.Get("_totp_setup_required") == nil {
			c.Next()
			return
		}
		method := c.Request.Method
		path := c.Request.URL.Path
		allowed := method == "GET" || method == "HEAD" || method == "OPTIONS" ||
			strings.HasPrefix(path, "/api/totp/") || path == "/logout" || path == "/api/check-session"
		if !allowed {
			c.JSON(http.StatusForbidden, gin.H{
				"error":               "MFA obligatoire : activez la double authentification (TOTP) dans Securite avant toute modification.",
				"totp_setup_required": true,
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

// AdminRequired checks if user has admin or super_admin role
func AdminRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		if config.AuthBypass {
			c.Next()
			return
		}
		if config.DemoMode {
			c.JSON(http.StatusForbidden, gin.H{
				"status": "error",
				"error":  "Indisponible en mode demonstration",
				"demo":   true,
			})
			c.Abort()
			return
		}
		session := sessions.Default(c)
		user := session.Get("user")
		role, _ := session.Get("role").(string)
		if user == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Non authentifie"})
			c.Abort()
			return
		}
		if !IsAdminOrAbove(role) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Acces admin requis"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// IsAdminOrAbove checks role hierarchy
func IsAdminOrAbove(role string) bool {
	return role == "admin" || role == "super_admin"
}

// IsSuperAdmin checks for super_admin role
func IsSuperAdmin(role string) bool {
	return role == "super_admin"
}

func isAPIRequest(c *gin.Context) bool {
	return len(c.Request.URL.Path) > 4 && c.Request.URL.Path[:5] == "/api/"
}

// GetSessionUser returns the current user from session
func GetSessionUser(c *gin.Context) (string, string) {
	session := sessions.Default(c)
	user, _ := session.Get("user").(string)
	role, _ := session.Get("role").(string)
	return user, role
}
