package middleware

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// Proxied apps handle their own forms, so they cannot use AutoDocker's CSRF
// token. Keep their browser requests on the same origin instead.
func ProxyOriginProtection() gin.HandlerFunc {
	return func(c *gin.Context) {
		if site := c.GetHeader("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		if origin := c.GetHeader("Origin"); origin != "" && !requestURLMatchesHost(origin, c.Request.Host, false) {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		if referrer := c.GetHeader("Referer"); referrer != "" && !requestURLMatchesHost(referrer, c.Request.Host, true) {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		c.Next()
	}
}

func requestURLMatchesHost(raw, host string, allowPath bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || !strings.EqualFold(u.Host, host) || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return allowPath || u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}
