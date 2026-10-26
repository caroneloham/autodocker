package handlers

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

type accountTestClient struct {
	router  *gin.Engine
	cookies map[string]*http.Cookie
	token   string
}

func (client *accountTestClient) request(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRFToken", client.token)
	for _, cookie := range client.cookies {
		req.AddCookie(cookie)
	}
	out := httptest.NewRecorder()
	client.router.ServeHTTP(out, req)
	for _, cookie := range out.Result().Cookies() {
		if cookie.MaxAge < 0 {
			delete(client.cookies, cookie.Name)
		} else {
			client.cookies[cookie.Name] = cookie
		}
	}
	return out
}
func accountTestRouter(t *testing.T, completeMode ...bool) (*gin.Engine, func(string) *accountTestClient) {
	t.Helper()
	oldDB := db.DB
	oldBypass, oldDemo, oldGenerator := config.AuthBypass, config.DemoMode, config.GeneratorOnly
	config.AuthBypass, config.DemoMode, config.GeneratorOnly = false, false, !(len(completeMode) > 0 && completeMode[0])
	t.Setenv("AUTODOCKER_ACCOUNT_OWNER", "owner")
	conn, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.DB = conn
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() {
		conn.Close()
		db.DB = oldDB
		config.AuthBypass, config.DemoMode, config.GeneratorOnly = oldBypass, oldDemo, oldGenerator
	})
	_, err = conn.Exec(`CREATE TABLE users(username TEXT PRIMARY KEY,password_hash TEXT NOT NULL,role TEXT DEFAULT 'user',totp_secret TEXT DEFAULT '',totp_enabled INTEGER DEFAULT 0);CREATE TABLE activity_log(id INTEGER PRIMARY KEY,timestamp TEXT,username TEXT,action TEXT,target TEXT,details TEXT);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureAccountColumns(conn); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureAccountColumns(conn); err != nil {
		t.Fatal("migration not idempotent")
	}
	hash, err := hashPassword("Initial-Test-42!")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct{ name, role string }{{"owner", "user"}, {"alice", "user"}, {"bob", "user"}, {"legacy-admin", "super_admin"}} {
		if _, err := conn.Exec("INSERT INTO users(username,password_hash,role) VALUES(?,?,?)", u.name, hash, u.role); err != nil {
			t.Fatal(err)
		}
	}
	r := gin.New()
	r.Use(sessions.Sessions("session", cookie.NewStore([]byte("account-tests-signing-key-32bytes"))))
	r.Use(middleware.LiveAccountSession())
	r.Use(middleware.CSRFProtection())
	r.SetHTMLTemplate(template.Must(template.New("login.html").Parse("{{.Error}}")))
	r.GET("/test-session/:username", func(c *gin.Context) {
		session := sessions.Default(c)
		session.Clear()
		session.Set("user", c.Param("username"))
		session.Set("role", "super_admin")
		session.Set("auth_version", 0)
		if err := session.Save(); err != nil {
			t.Fatal(err)
		}
		c.JSON(200, gin.H{"csrf": middleware.GenerateCSRFToken(c)})
	})
	api := r.Group("/api", middleware.LoginRequired())
	RegisterAuthRoutes(r, nil, api)
	RegisterAccountRoutes(r, api)
	return r, func(username string) *accountTestClient {
		client := &accountTestClient{router: r, cookies: map[string]*http.Cookie{}}
		out := client.request("GET", "/test-session/"+username, "")
		var data struct {
			CSRF string `json:"csrf"`
		}
		if json.Unmarshal(out.Body.Bytes(), &data) != nil || data.CSRF == "" {
			t.Fatal("test session unavailable")
		}
		client.token = data.CSRF
		return client
	}
}

func TestManagedAccountsOnlyOwnerCanCreateOrManage(t *testing.T) {
	r, client := accountTestRouter(t)
	owner, alice, admin := client("owner"), client("alice"), client("legacy-admin")
	for _, outsider := range []*accountTestClient{alice, admin} {
		for _, request := range []struct{ method, path, body string }{{"GET", "/api/accounts", ""}, {"POST", "/api/accounts", `{"username":"intruder","password":"Initial-Test-42!"}`}, {"POST", "/api/accounts/bob/status", `{"disabled":true}`}, {"POST", "/api/accounts/bob/password", `{"password":"Replacement-Test-42!"}`}, {"GET", "/utilisateurs", ""}} {
			if out := outsider.request(request.method, request.path, request.body); out.Code != 403 {
				t.Fatalf("owner-only route accepted an outsider: %s status=%d", request.path, out.Code)
			}
		}
	}
	created := owner.request("POST", "/api/accounts", `{"username":"camille","password":"Initial-Test-42!"}`)
	if created.Code != 201 {
		t.Fatalf("owner create status=%d", created.Code)
	}
	var role, hash string
	if err := db.DB.QueryRow("SELECT role,password_hash FROM users WHERE username='camille'").Scan(&role, &hash); err != nil || role != "user" || !checkPassword(hash, "Initial-Test-42!") {
		t.Fatal("creation did not create a hashed ordinary account")
	}
	if out := owner.request("POST", "/api/accounts", `{"username":"camille","password":"Initial-Test-42!"}`); out.Code != 409 {
		t.Fatal("duplicate accepted")
	}
	if out := owner.request("POST", "/api/accounts", `{"username":"another","password":"Initial-Test-42!","role":"super_admin"}`); out.Code != 400 {
		t.Fatal("role injection accepted")
	}
	for _, path := range []string{"/register", "/signup", "/api/register", "/api/signup", "/api/admin/add-user"} {
		out := alice.request("POST", path, `{"username":"rogue","password":"Initial-Test-42!"}`)
		if out.Code != 404 {
			t.Fatalf("registration/legacy route still exists: %s status=%d", path, out.Code)
		}
	}
	noSession := httptest.NewRecorder()
	r.ServeHTTP(noSession, httptest.NewRequest("GET", "/api/account", nil))
	if noSession.Code != 401 {
		t.Fatal("anonymous profile access")
	}
	for _, body := range []string{`{"disabled":true}`, `{"disabled":false}`} {
		if out := owner.request("POST", "/api/accounts/owner/status", body); out.Code != 403 {
			t.Fatal("owner lockout allowed")
		}
	}
	if out := owner.request("POST", "/api/accounts/owner/password", `{"password":"Replacement-Test-42!"}`); out.Code != 403 {
		t.Fatal("owner password reset bypassed current-password check")
	}
	list := owner.request("GET", "/api/accounts", "")
	if list.Code != 200 || strings.Contains(list.Body.String(), "password_hash") || strings.Contains(list.Body.String(), "Initial-Test-42!") {
		t.Fatal("list leaks credential material")
	}
}

func TestAccountProfileOnlyChangesOwnEmailAndPassword(t *testing.T) {
	_, client := accountTestRouter(t)
	alice, otherAlice := client("alice"), client("alice")
	if out := alice.request("POST", "/api/account/email", `{"email":"alice@example.test"}`); out.Code != 200 {
		t.Fatal("own email update failed")
	}
	for _, body := range []string{`{"email":"stolen@example.test","username":"bob"}`, `{"email":"alice@example.test","role":"admin"}`, `{"email":"Invalid <alice@example.test>"}`} {
		if out := alice.request("POST", "/api/account/email", body); out.Code != 400 {
			t.Fatal("unowned or invalid profile fields accepted")
		}
	}
	var email string
	db.DB.QueryRow("SELECT email FROM users WHERE username='bob'").Scan(&email)
	if email != "" {
		t.Fatal("other user's email changed")
	}
	if out := alice.request("POST", "/api/account/password", `{"current_password":"wrong","new_password":"Replacement-Test-42!"}`); out.Code != 403 {
		t.Fatal("password changed without old password")
	}
	if out := alice.request("POST", "/api/account/password", `{"current_password":"Initial-Test-42!","new_password":"Replacement-Test-42!","username":"bob"}`); out.Code != 400 {
		t.Fatal("unowned password target accepted")
	}
	if out := alice.request("POST", "/api/account/password", `{"current_password":"Initial-Test-42!","new_password":"Replacement-Test-42!"}`); out.Code != 200 {
		t.Fatalf("own password change status=%d", out.Code)
	}
	if out := alice.request("GET", "/api/account", ""); out.Code != 200 {
		t.Fatal("current session not refreshed")
	}
	if out := otherAlice.request("GET", "/api/account", ""); out.Code != 401 {
		t.Fatal("old session survived password change")
	}
	var hash string
	db.DB.QueryRow("SELECT password_hash FROM users WHERE username='alice'").Scan(&hash)
	if checkPassword(hash, "Initial-Test-42!") || !checkPassword(hash, "Replacement-Test-42!") {
		t.Fatal("password hash not rotated")
	}
}

func TestAccountDisableResetAndCSRFInvalidateExistingSessions(t *testing.T) {
	_, client := accountTestRouter(t)
	owner, bob := client("owner"), client("bob")
	oldToken := owner.token
	owner.token = ""
	if out := owner.request("POST", "/api/accounts/bob/status", `{"disabled":true}`); out.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	owner.token = oldToken
	if out := owner.request("POST", "/api/accounts/bob/password", `{"password":"Replacement-Test-42!"}`); out.Code != 200 {
		t.Fatal("owner reset failed")
	}
	if out := bob.request("GET", "/api/account", ""); out.Code != 401 {
		t.Fatal("reset did not revoke existing session")
	}
	if out := owner.request("POST", "/api/accounts/alice/status", `{"disabled":true}`); out.Code != 200 {
		t.Fatal("disable failed")
	}
	alice := client("alice")
	if out := alice.request("GET", "/api/account", ""); out.Code != 401 {
		t.Fatal("disabled account retained access")
	}
	request := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"username": {"alice"}, "password": {"Initial-Test-42!"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	out := httptest.NewRecorder()
	owner.router.ServeHTTP(out, request)
	if out.Code != 200 || !strings.Contains(out.Body.String(), "Identifiants invalides") {
		t.Fatal("disabled account logged in")
	}
	if out := owner.request("POST", "/api/accounts/alice/status", `{"disabled":false}`); out.Code != 200 {
		t.Fatal("reactivation failed")
	}
}

func TestAccountRoutesCoexistWithAdminRoutesInCompleteMode(t *testing.T) {
	old := config.GeneratorOnly
	config.GeneratorOnly = false
	defer func() { config.GeneratorOnly = old }()
	r := gin.New()
	api := r.Group("/api")
	RegisterAccountRoutes(r, api)
	RegisterAdminRoutes(api)
	found := map[string]bool{}
	for _, route := range r.Routes() {
		found[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{"GET /compte", "GET /api/account", "GET /api/account/setup", "POST /api/account/setup", "GET /api/admin/list-users"} {
		if !found[route] {
			t.Errorf("missing complete-mode route: %s", route)
		}
	}
}

func TestLegacyPasswordChangeRevokesOtherSessionsInCompleteMode(t *testing.T) {
	_, client := accountTestRouter(t, true)
	alice, otherAlice := client("alice"), client("alice")
	response := alice.request("POST", "/api/change-password", `{"old_password":"Initial-Test-42!","new_password":"Replacement-Test-42!"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("legacy password update failed: %d %s", response.Code, response.Body.String())
	}
	if response := alice.request("GET", "/api/account", ""); response.Code != http.StatusOK {
		t.Fatalf("current session revoked: %d", response.Code)
	}
	if response := otherAlice.request("GET", "/api/account", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("older session retained access: %d", response.Code)
	}
}
