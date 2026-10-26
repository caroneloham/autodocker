package middleware

import (
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"testing"
)

func TestHostedCSRFRequiresInitializedToken(t *testing.T) {
	r := gin.New()
	r.Use(sessions.Sessions("csrf", cookie.NewStore([]byte("hosted-csrf-test-key-32bytes-long!"))))
	r.Use(CSRFProtection())
	r.GET("/token", func(c *gin.Context) { c.String(200, GenerateCSRFToken(c)) })
	r.POST("/write", func(c *gin.Context) { c.String(200, "accepted") })
	r.POST("/login", func(c *gin.Context) { c.String(200, "login") })
	noToken := httptest.NewRecorder()
	r.ServeHTTP(noToken, httptest.NewRequest("POST", "/write", nil))
	if noToken.Code != 403 {
		t.Fatal("uninitialized token bypass")
	}
	token := httptest.NewRecorder()
	r.ServeHTTP(token, httptest.NewRequest("GET", "/token", nil))
	if token.Code != 200 {
		t.Fatal("token unavailable")
	}
	for _, valid := range []bool{true, false} {
		req := httptest.NewRequest("POST", "/write", nil)
		for _, c := range token.Result().Cookies() {
			req.AddCookie(c)
		}
		if valid {
			req.Header.Set("X-CSRFToken", token.Body.String())
		} else {
			req.Header.Set("X-CSRFToken", "invalid")
		}
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		expected := 403
		if valid {
			expected = 200
		}
		if out.Code != expected {
			t.Fatalf("valid=%v status=%d", valid, out.Code)
		}
	}
	login := httptest.NewRecorder()
	r.ServeHTTP(login, httptest.NewRequest("POST", "/login", nil))
	if login.Code != 200 {
		t.Fatal("login broken")
	}
}
