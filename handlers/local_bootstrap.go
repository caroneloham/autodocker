package handlers

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// The local owner setup is explicit opt-in from the bundled launcher. A fresh
// deployment behind a reverse proxy cannot silently enter this flow.
func localBootstrapEnabled() bool {
	return config.LocalBootstrapMode() && validBootstrapToken(os.Getenv("AUTODOCKER_BOOTSTRAP_TOKEN"))
}

func validBootstrapToken(token string) bool {
	if len(token) != 64 || strings.ToLower(token) != token {
		return false
	}
	decoded, err := hex.DecodeString(token)
	return err == nil && len(decoded) == 32
}

func bootstrapGrant(c *gin.Context) bool {
	session := sessions.Default(c)
	stored, _ := session.Get("bootstrap_token_hash").(string)
	issued, _ := session.Get("bootstrap_issued_at").(int64)
	if len(stored) != 64 || issued <= 0 || time.Since(time.Unix(issued, 0)) > 30*time.Minute || time.Until(time.Unix(issued, 0)) > time.Minute {
		return false
	}
	digest := sha256.Sum256([]byte(os.Getenv("AUTODOCKER_BOOTSTRAP_TOKEN")))
	return subtle.ConstantTimeCompare([]byte(stored), []byte(hex.EncodeToString(digest[:]))) == 1
}

func localBootstrapPending() bool {
	if !localBootstrapEnabled() {
		return false
	}
	var count int
	return db.DB.QueryRow("SELECT count(*) FROM users").Scan(&count) == nil && count == 0
}

func localBootstrapRequest(c *gin.Context) bool {
	if !localBootstrapEnabled() {
		return false
	}
	ip, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil || net.ParseIP(ip) == nil || !net.ParseIP(ip).IsLoopback() {
		return false
	}
	host := c.Request.Host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	if !strings.EqualFold(host, "localhost") && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		return false
	}
	return c.GetHeader("Forwarded") == "" && c.GetHeader("X-Forwarded-For") == "" && c.GetHeader("X-Real-IP") == ""
}

func showLocalLauncherHelp(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.HTML(http.StatusForbidden, "setup.html", gin.H{"LaunchRequired": true})
}

func registerLocalBootstrapRoutes(r *gin.Engine) {
	r.GET("/setup", func(c *gin.Context) {
		if !localBootstrapRequest(c) {
			c.Status(http.StatusNotFound)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		if !localBootstrapPending() {
			c.Redirect(http.StatusSeeOther, "/login")
			return
		}
		if token := c.Query("token"); token != "" {
			if !validBootstrapToken(token) || subtle.ConstantTimeCompare([]byte(token), []byte(os.Getenv("AUTODOCKER_BOOTSTRAP_TOKEN"))) != 1 {
				showLocalLauncherHelp(c)
				return
			}
			digest := sha256.Sum256([]byte(token))
			session := sessions.Default(c)
			session.Set("bootstrap_token_hash", hex.EncodeToString(digest[:]))
			session.Set("bootstrap_issued_at", time.Now().Unix())
			// SessionRecovery retains anonymous sessions carrying a CSRF token.
			// Mint it before the redirect so the bootstrap grant survives.
			middleware.GenerateCSRFToken(c)
			if err := session.Save(); err != nil {
				c.Status(http.StatusServiceUnavailable)
				return
			}
			c.Redirect(http.StatusSeeOther, "/setup")
			return
		}
		if !bootstrapGrant(c) {
			showLocalLauncherHelp(c)
			return
		}
		// The token has been removed from the URL. Keep the form's same-origin
		// Origin header: no-referrer makes browser form POSTs send Origin: null,
		// which LocalHostOriginProtection correctly rejects.
		c.Header("Referrer-Policy", "same-origin")
		c.HTML(http.StatusOK, "setup.html", gin.H{"CSRF": middleware.GenerateCSRFToken(c), "Error": ""})
	})
	r.POST("/setup", middleware.RateLimitLogin(), func(c *gin.Context) {
		if !localBootstrapRequest(c) {
			c.Status(http.StatusNotFound)
			return
		}
		if !localBootstrapPending() {
			c.Redirect(http.StatusSeeOther, "/login")
			return
		}
		if !bootstrapGrant(c) {
			showLocalLauncherHelp(c)
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
		password, confirmation := c.PostForm("password"), c.PostForm("confirmation")
		if password != confirmation || len(password) > 72 {
			c.HTML(http.StatusBadRequest, "setup.html", gin.H{"CSRF": middleware.GenerateCSRFToken(c), "Error": "Vérifiez les deux mots de passe (72 octets maximum)."})
			return
		}
		if valid, message := ValidatePassword(password); !valid {
			c.HTML(http.StatusBadRequest, "setup.html", gin.H{"CSRF": middleware.GenerateCSRFToken(c), "Error": message})
			return
		}
		hash, err := hashPassword(password)
		if err != nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		result, err := db.DB.Exec("INSERT INTO users(username,password_hash,role) SELECT 'admin',?,'super_admin' WHERE NOT EXISTS (SELECT 1 FROM users)", hash)
		if err != nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		inserted, err := result.RowsAffected()
		if err != nil || inserted != 1 {
			c.Redirect(http.StatusSeeOther, "/login")
			return
		}
		var version int
		if err := db.DB.QueryRow("SELECT auth_version FROM users WHERE username='admin'").Scan(&version); err != nil && err != sql.ErrNoRows {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		session := sessions.Default(c)
		session.Clear()
		session.Set("user", "admin")
		session.Set("role", "super_admin")
		session.Set("auth_version", version)
		if err := session.Save(); err != nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		c.Redirect(http.StatusSeeOther, "/compte")
	})
}
