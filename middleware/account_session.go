package middleware

import (
	"database/sql"
	"net/http"
	"os"
	"strings"

	"autodocker-go/config"
	"autodocker-go/db"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

func AccountOwnerUsername() string {
	if config.LocalBootstrapMode() {
		return "admin"
	}
	if owner := strings.TrimSpace(os.Getenv("AUTODOCKER_ACCOUNT_OWNER")); owner != "" {
		return owner
	}
	if config.AuthBypass {
		return config.AuthBypassUser
	}
	return "admin"
}

func IsAccountOwner(c *gin.Context) bool {
	user, _ := GetSessionUser(c)
	return !config.DemoMode && user != "" && user == AccountOwnerUsername()
}

func AccountOwnerRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !IsAccountOwner(c) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Seul le propriétaire peut gérer les utilisateurs."})
			return
		}
		c.Next()
	}
}

// Read the account on each authenticated request: disabling an account or
// rotating its password revokes its existing cookies, including queued-job access.
func LiveAccountSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		if config.AuthBypass || config.DemoMode {
			c.Next()
			return
		}
		session := sessions.Default(c)
		user, _ := session.Get("user").(string)
		if user == "" {
			c.Next()
			return
		}
		var role string
		var disabled, version int
		err := db.DB.QueryRow("SELECT role,disabled,auth_version FROM users WHERE username=?", user).Scan(&role, &disabled, &version)
		if err != nil && err != sql.ErrNoRows {
			c.AbortWithStatusJSON(503, gin.H{"error": "Vérification du compte indisponible."})
			return
		}
		stored, _ := session.Get("auth_version").(int)
		if err == sql.ErrNoRows || disabled != 0 || stored != version {
			session.Clear()
			session.Options(sessions.Options{Path: "/", MaxAge: -1})
			_ = session.Save()
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				c.AbortWithStatusJSON(401, gin.H{"error": "Session expirée. Reconnectez-vous."})
				return
			}
		} else if cached, _ := session.Get("role").(string); cached != role {
			session.Set("role", role)
			_ = session.Save()
		}
		c.Next()
	}
}
