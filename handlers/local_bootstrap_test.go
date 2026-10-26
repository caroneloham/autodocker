package handlers

import (
	"database/sql"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	_ "modernc.org/sqlite"
)

func TestLocalBootstrapRequiresLoopbackAndCSRF(t *testing.T) {
	oldDB, oldBind, oldBypass, oldDemo := db.DB, config.BindHost, config.AuthBypass, config.DemoMode
	defer func() {
		db.DB, config.BindHost, config.AuthBypass, config.DemoMode = oldDB, oldBind, oldBypass, oldDemo
	}()
	t.Setenv("AUTODOCKER_LOCAL_BOOTSTRAP", "1")
	t.Setenv("AUTODOCKER_BOOTSTRAP_TOKEN", strings.Repeat("a", 64))
	config.BindHost, config.AuthBypass, config.DemoMode = "127.0.0.1", false, false
	conn, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	db.DB = conn
	if _, err := conn.Exec("CREATE TABLE users(username TEXT PRIMARY KEY,password_hash TEXT NOT NULL,role TEXT NOT NULL,auth_version INTEGER NOT NULL DEFAULT 0)"); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.SetHTMLTemplate(template.Must(template.New("setup.html").Parse("csrf:{{.CSRF}} {{.Error}}")))
	r.Use(sessions.Sessions("session", cookie.NewStore([]byte("bootstrap-signing-key-32-bytes-long"))))
	r.Use(middleware.LocalHostOriginProtection())
	r.Use(middleware.SessionRecovery())
	r.Use(middleware.LiveAccountSession())
	r.Use(middleware.CSRFProtection())
	r.GET("/csrf-test", func(c *gin.Context) { c.String(http.StatusOK, middleware.GenerateCSRFToken(c)) })
	registerLocalBootstrapRoutes(r)

	request := func(method, path string, body url.Values, cookie *http.Cookie, forwarded bool) *httptest.ResponseRecorder {
		var data string
		if body != nil {
			data = body.Encode()
		}
		req := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(data))
		req.RemoteAddr = "127.0.0.1:47999"
		if body != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if forwarded {
			req.Header.Set("X-Forwarded-For", "203.0.113.10")
		}
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		return out
	}
	if response := request("GET", "/setup", nil, nil, true); response.Code != http.StatusForbidden {
		t.Fatalf("proxy bootstrap exposed: %d", response.Code)
	}
	if response := request("GET", "/setup", nil, nil, false); response.Code != http.StatusForbidden {
		t.Fatalf("setup without token exposed: %d", response.Code)
	}
	if response := request("GET", "/setup?token="+strings.Repeat("b", 64), nil, nil, false); response.Code != http.StatusForbidden {
		t.Fatalf("setup with wrong token exposed: %d", response.Code)
	}
	t.Setenv("AUTODOCKER_BOOTSTRAP_TOKEN", "")
	if response := request("GET", "/setup?token="+strings.Repeat("a", 64), nil, nil, false); response.Code != http.StatusNotFound {
		t.Fatalf("setup without configured token exposed: %d", response.Code)
	}
	t.Setenv("AUTODOCKER_BOOTSTRAP_TOKEN", strings.Repeat("a", 64))
	csrfOnly := request("GET", "/csrf-test", nil, nil, false)
	if csrfOnly.Code != 200 || len(csrfOnly.Result().Cookies()) == 0 {
		t.Fatal("test CSRF cookie missing")
	}
	noGrantForm := url.Values{"password": {"Example-Strong-42!"}, "confirmation": {"Example-Strong-42!"}, "csrf_token": {csrfOnly.Body.String()}}
	if response := request("POST", "/setup", noGrantForm, csrfOnly.Result().Cookies()[0], false); response.Code != http.StatusForbidden {
		t.Fatalf("setup POST without grant accepted: %d", response.Code)
	}
	grant := request("GET", "/setup?token="+strings.Repeat("a", 64), nil, nil, false)
	if grant.Code != http.StatusSeeOther || grant.Header().Get("Location") != "/setup" || len(grant.Result().Cookies()) == 0 {
		t.Fatalf("bootstrap grant failed: %d %s", grant.Code, grant.Header().Get("Location"))
	}
	page := request("GET", "/setup", nil, grant.Result().Cookies()[0], false)
	if page.Code != 200 {
		t.Fatalf("setup unavailable: %d", page.Code)
	}
	csrf := regexp.MustCompile(`csrf:([a-f0-9]+)`).FindStringSubmatch(page.Body.String())
	if len(csrf) != 2 {
		t.Fatal("setup CSRF session missing")
	}
	cookie := grant.Result().Cookies()[0]
	if len(page.Result().Cookies()) > 0 {
		cookie = page.Result().Cookies()[0]
	}
	form := url.Values{"password": {"Example-Strong-42!"}, "confirmation": {"Example-Strong-42!"}}
	if response := request("POST", "/setup", form, cookie, false); response.Code != 403 {
		t.Fatalf("missing CSRF accepted: %d", response.Code)
	}
	form.Set("csrf_token", csrf[1])
	if response := request("POST", "/setup", form, cookie, false); response.Code != 303 {
		t.Fatalf("owner creation failed: %d %s", response.Code, response.Body.String())
	}
	var role string
	if err := conn.QueryRow("SELECT role FROM users WHERE username='admin'").Scan(&role); err != nil || role != "super_admin" {
		t.Fatalf("owner missing: %v %s", err, role)
	}
	if response := request("GET", "/setup", nil, nil, false); response.Code != 303 {
		t.Fatalf("setup remained open: %d", response.Code)
	}
}
