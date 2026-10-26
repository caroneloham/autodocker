package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"

	"autodocker-go/config"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

func TestCloudToolsAndCapabilitiesRequireAuthenticatedSessions(t *testing.T) {
	r := gin.New()
	r.Use(sessions.Sessions("cloud_test", cookie.NewStore([]byte("synthetic-cloud-cookie-signing-key"))))
	registerCloudToolRoutes(r.Group("/api"))
	for _, path := range []string{"/api/ai-capabilities", "/api/ai-tools/search", "/api/ai-tools/vision"} {
		method := "POST"
		if path == "/api/ai-capabilities" {
			method = "GET"
		}
		request := httptest.NewRequest(method, path, strings.NewReader(`{"query":"test"}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		r.ServeHTTP(response, request)
		if response.Code != 401 {
			t.Fatalf("anonymous request accepted: %s %d", path, response.Code)
		}
	}
}

func TestCloudToolRateLimitSeparatesAccounts(t *testing.T) {
	oldBypass, oldDemo := config.AuthBypass, config.DemoMode
	config.AuthBypass, config.DemoMode = false, false
	t.Cleanup(func() { config.AuthBypass, config.DemoMode = oldBypass, oldDemo })
	r := gin.New()
	r.Use(sessions.Sessions("cloud_rate_test", cookie.NewStore([]byte("synthetic-cloud-rate-cookie-key"))))
	r.Use(func(c *gin.Context) {
		sessions.Default(c).Set("user", c.GetHeader("X-Test-User"))
		c.Next()
	})
	registerCloudToolRoutes(r.Group("/api"))
	for index, user := range []string{"cloud-rate-a", "cloud-rate-a", "cloud-rate-b"} {
		req := httptest.NewRequest("POST", "/api/ai-tools/search", strings.NewReader(`{"query":""}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-User", user)
		response := httptest.NewRecorder()
		r.ServeHTTP(response, req)
		wanted := 400
		if index == 1 {
			wanted = 429
		}
		if response.Code != wanted {
			t.Fatalf("account rate limit differs: %s %d", user, response.Code)
		}
	}
}
