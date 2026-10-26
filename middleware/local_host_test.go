package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"autodocker-go/config"
	"github.com/gin-gonic/gin"
)

func TestLocalHostOriginProtectsGetAndCSRFExemptPost(t *testing.T) {
	oldBind, oldNoSSL, oldDemo, oldBypass := config.BindHost, config.NoSSL, config.DemoMode, config.AuthBypass
	defer func() {
		config.BindHost, config.NoSSL, config.DemoMode, config.AuthBypass = oldBind, oldNoSSL, oldDemo, oldBypass
	}()
	t.Setenv("AUTODOCKER_LOCAL_BOOTSTRAP", "1")
	config.BindHost, config.NoSSL, config.DemoMode, config.AuthBypass = "127.0.0.1", true, false, false
	router := gin.New()
	router.Use(LocalHostOriginProtection())
	router.GET("/", func(c *gin.Context) { c.Status(200) })
	router.POST("/login", func(c *gin.Context) { c.Status(200) })
	request := func(method, path, host, origin, forwarded string) int {
		req := httptest.NewRequest(method, "http://127.0.0.1:8000"+path, nil)
		req.RemoteAddr, req.Host = "127.0.0.1:45234", host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if forwarded != "" {
			req.Header.Set("X-Forwarded-For", forwarded)
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out.Code
	}
	if got := request(http.MethodGet, "/", "evil.example:8000", "", ""); got != 421 {
		t.Fatalf("foreign Host accepted: %d", got)
	}
	if got := request(http.MethodGet, "/", "127.0.0.1:8000", "https://evil.example", ""); got != 403 {
		t.Fatalf("foreign GET Origin accepted: %d", got)
	}
	if got := request(http.MethodGet, "/", "127.0.0.1:8000", "", "203.0.113.10"); got != 403 {
		t.Fatalf("proxy accepted: %d", got)
	}
	if got := request(http.MethodPost, "/login", "127.0.0.1:8000", "https://evil.example", ""); got != 403 {
		t.Fatalf("foreign login Origin accepted: %d", got)
	}
	if got := request(http.MethodPost, "/login", "127.0.0.1:8000", "http://127.0.0.1:8000", ""); got != 200 {
		t.Fatalf("local login blocked: %d", got)
	}
}
