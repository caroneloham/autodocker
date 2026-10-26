package middleware

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"autodocker-go/config"
	"github.com/gin-gonic/gin"
)

// LocalHostOriginProtection prevents DNS rebinding or proxy access to the
// bundled HTTP application, including routes exempted from the CSRF checker.
func LocalHostOriginProtection() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !config.LocalBootstrapMode() {
			c.Next()
			return
		}
		host := c.Request.Host
		if parsed, _, err := net.SplitHostPort(host); err == nil {
			host = parsed
		}
		if !strings.EqualFold(host, "localhost") && host != "127.0.0.1" && host != "::1" {
			c.AbortWithStatus(http.StatusMisdirectedRequest)
			return
		}
		remote, _, err := net.SplitHostPort(c.Request.RemoteAddr)
		if err != nil || net.ParseIP(remote) == nil || !net.ParseIP(remote).IsLoopback() ||
			c.GetHeader("Forwarded") != "" || c.GetHeader("X-Forwarded-For") != "" || c.GetHeader("X-Real-IP") != "" {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		origin := c.GetHeader("Origin")
		if origin == "" && c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead && c.Request.Method != http.MethodOptions {
			origin = c.GetHeader("Referer")
		}
		if origin != "" {
			u, err := url.Parse(origin)
			scheme := "https"
			if config.NoSSL {
				scheme = "http"
			}
			if err != nil || u.Scheme != scheme || !strings.EqualFold(u.Host, c.Request.Host) || u.User != nil {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
		}
		c.Next()
	}
}
