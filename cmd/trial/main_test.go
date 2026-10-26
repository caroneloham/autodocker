package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autodocker-go/services"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

func TestVisitorIPUsesProxyObservedHop(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/essai/api/me", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("X-Forwarded-For", "198.51.100.10, 203.0.113.8")
	if got := visitorIP(request); got != "203.0.113.8" {
		t.Fatalf("client-supplied prefix controlled quota IP: %s", got)
	}
	request.RemoteAddr = "192.0.2.4:12345"
	if got := visitorIP(request); got != "192.0.2.4" {
		t.Fatalf("untrusted peer supplied forwarded IP: %s", got)
	}
}

func TestGuestQuotaSeparatesBrowserAndIPLimits(t *testing.T) {
	quota, err := newAIQuota(filepath.Join(t.TempDir(), "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	ip := "guest:192.0.2.5"
	for browser := 0; browser < 3; browser++ {
		subject := "guest-cookie:browser-" + string(rune('a'+browser))
		for request := 0; request < 2; request++ {
			allowed, err := quota.reserve(subject, 2, ip, 6)
			if err != nil || !allowed {
				t.Fatalf("browser %d request %d should be allowed: %v", browser, request, err)
			}
		}
		if left, _ := quota.remaining(subject, 2, ip, 6); left != 0 {
			t.Fatalf("browser %d has %d requests after using two", browser, left)
		}
		if browser < 2 && quota.exhaustedReason(subject, 2, ip, 6) != "visitor" {
			t.Fatalf("browser %d should receive the account prompt after two requests", browser)
		}
	}
	if quota.exhaustedReason("guest-cookie:browser-d", 2, ip, 6) != "connection" {
		t.Fatal("shared IP limit must not suggest that signing up will restore capacity")
	}
	if allowed, err := quota.reserve("guest-cookie:browser-d", 2, ip, 6); err != nil || allowed {
		t.Fatalf("shared IP must stop after six requests: allowed=%v err=%v", allowed, err)
	}
	if err := quota.refund("guest-cookie:browser-c", ip); err != nil {
		t.Fatal(err)
	}
	if allowed, err := quota.reserve("guest-cookie:browser-d", 2, ip, 6); err != nil || !allowed {
		t.Fatalf("refunded request must restore capacity: allowed=%v err=%v", allowed, err)
	}
}

func trialAITestRouter(t *testing.T) (*gin.Engine, *aiQuota, *services.InfrastructureJobQueue) {
	t.Helper()
	dir := t.TempDir()
	quota, err := newAIQuota(filepath.Join(dir, "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := newAccountStore(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.Use(sessions.Sessions("trial-test", cookie.NewStore([]byte("trial-test-cookie-key-32-bytes!!!!"))))
	queue := services.NewInfrastructureJobQueue(1, 4, func(_ context.Context, _ string) (*services.MagicAIResult, error) {
		return &services.MagicAIResult{Confidence: "validated"}, nil
	})
	registerAIRoutesWithQueue(router.Group("/essai/api"), quota, accounts, queue)
	t.Cleanup(queue.Close)
	return router, quota, queue
}

func postTrialAIPrompt(t *testing.T, router *gin.Engine, prompt string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"prompt": prompt})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/essai/api/ai-jobs", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestTrialAILimitCountsUnicodeCharacters(t *testing.T) {
	router, quota, _ := trialAITestRouter(t)
	for _, test := range []struct {
		name, prompt string
		status       int
	}{
		{"ASCII boundary", "Moodle " + strings.Repeat("a", 1993), http.StatusOK},
		{"accented boundary", "Moodle " + strings.Repeat("é", 1993), http.StatusOK},
		{"emoji boundary", "Moodle " + strings.Repeat("🙂", 1993), http.StatusOK},
		{"accented overflow", "Moodle " + strings.Repeat("é", 1994), http.StatusBadRequest},
		{"emoji overflow", "Moodle " + strings.Repeat("🙂", 1994), http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := postTrialAIPrompt(t, router, test.prompt)
			if response.Code != test.status {
				t.Fatalf("status=%d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
	if quota.state.Total != 0 {
		t.Fatalf("boundary checks or deterministic clarifications consumed %d quota", quota.state.Total)
	}
}

func TestTrialPreflightClarificationDoesNotSpendOrBypassQuota(t *testing.T) {
	router, quota, queue := trialAITestRouter(t)
	checkClarification := func() {
		t.Helper()
		response := postTrialAIPrompt(t, router, "Je veux Moodle")
		if response.Code != http.StatusOK {
			t.Fatalf("clarification status=%d: %s", response.Code, response.Body.String())
		}
		var data struct {
			Confidence string   `json:"confidence"`
			Questions  []string `json:"questions"`
			Job        any      `json:"job"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil || data.Confidence != "needs_clarification" || len(data.Questions) != 1 || data.Job != nil {
			t.Fatalf("expected immediate, private clarification: %s (%v)", response.Body.String(), err)
		}
	}
	checkClarification()
	if quota.state.Total != 0 {
		t.Fatal("clarification spent a quota unit")
	}
	for i := 0; i < 40; i++ {
		if allowed, err := quota.reserve("account:already-used", 40, "", 0); err != nil || !allowed {
			t.Fatalf("quota setup failed at %d: %v", i, err)
		}
	}
	checkClarification()
	if quota.state.Total != 40 {
		t.Fatal("clarification changed an exhausted quota")
	}
	if response := postTrialAIPrompt(t, router, "Un site WordPress"); response.Code != http.StatusTooManyRequests {
		t.Fatalf("ordinary AI request bypassed quota: %d %s", response.Code, response.Body.String())
	}
	other, err := queue.Submit("trial:another-visitor", "WordPress")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/essai/api/ai-jobs/"+other.ID, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unowned job became readable: %d", response.Code)
	}
}

func TestTrialAPIBudgetsKeepManualGenerationAvailable(t *testing.T) {
	router := gin.New()
	router.Use(sessions.Sessions("trial-test", cookie.NewStore([]byte("trial-test-cookie-key-32-bytes!!!!"))))
	api := router.Group("/essai/api")
	api.Use(trialAPIMiddleware())
	for _, path := range []string{"/login", "/logout", "/ai-jobs", "/generate/preview", "/ai-jobs/:id/cancel"} {
		api.POST(path, func(c *gin.Context) { c.Status(http.StatusOK) })
	}
	post := func(path string) int {
		request := httptest.NewRequest(http.MethodPost, "/essai/api"+path, strings.NewReader(`{}`))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response.Code
	}
	for i := 0; i < 12; i++ {
		if status := post("/login"); status != http.StatusOK {
			t.Fatalf("account request %d: %d", i, status)
		}
	}
	if status := post("/login"); status != http.StatusTooManyRequests {
		t.Fatalf("account budget not enforced: %d", status)
	}
	if status := post("/logout"); status != http.StatusOK {
		t.Fatalf("logout was rate limited: %d", status)
	}
	for i := 0; i < 12; i++ {
		if status := post("/ai-jobs"); status != http.StatusOK {
			t.Fatalf("AI request %d inherited account budget: %d", i, status)
		}
	}
	if status := post("/ai-jobs"); status != http.StatusTooManyRequests {
		t.Fatalf("AI submission budget not enforced: %d", status)
	}
	for i := 0; i < 120; i++ {
		if status := post("/generate/preview"); status != http.StatusOK {
			t.Fatalf("manual generation %d blocked early: %d", i, status)
		}
	}
	if status := post("/generate/preview"); status != http.StatusTooManyRequests {
		t.Fatalf("manual generation budget not enforced: %d", status)
	}
	if status := post("/ai-jobs/" + strings.Repeat("a", 48) + "/cancel"); status != http.StatusOK {
		t.Fatalf("cancellation was rate limited: %d", status)
	}
}

func TestTrialAPIBodyLimitsMatchRoutePurpose(t *testing.T) {
	router := gin.New()
	router.Use(sessions.Sessions("trial-test", cookie.NewStore([]byte("trial-test-cookie-key-32-bytes!!!!"))))
	api := router.Group("/essai/api")
	api.Use(trialAPIMiddleware())
	for _, path := range []string{"/login", "/ai-jobs", "/generate"} {
		api.POST(path, func(c *gin.Context) {
			if _, err := io.ReadAll(c.Request.Body); err != nil {
				c.Status(http.StatusRequestEntityTooLarge)
				return
			}
			c.Status(http.StatusOK)
		})
	}
	post := func(path string, size int) int {
		request := httptest.NewRequest(http.MethodPost, "/essai/api"+path, strings.NewReader(strings.Repeat("x", size)))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response.Code
	}
	if status := post("/generate", 700<<10); status != http.StatusOK {
		t.Fatalf("logo-sized generation payload blocked: %d", status)
	}
	if status := post("/generate", (2<<20)+1); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized generation payload accepted: %d", status)
	}
	if status := post("/login", 4097); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized account payload accepted: %d", status)
	}
	if status := post("/ai-jobs", 24<<10); status != http.StatusOK {
		t.Fatalf("JSON-escaped Unicode-sized AI payload blocked: %d", status)
	}
	if status := post("/ai-jobs", (32<<10)+1); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized AI payload accepted: %d", status)
	}
}

func TestTrialQueuedCancellationRefundsOnlyItsOwnerBeforeStart(t *testing.T) {
	dir := t.TempDir()
	quota, err := newAIQuota(filepath.Join(dir, "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := newAccountStore(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	queue := services.NewInfrastructureJobQueue(1, 2, func(ctx context.Context, _ string) (*services.MagicAIResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	t.Cleanup(queue.Close)
	router := gin.New()
	router.Use(sessions.Sessions("trial-test", cookie.NewStore([]byte("trial-test-cookie-key-32-bytes!!!!"))))
	registerAIRoutesWithQueue(router.Group("/essai/api"), quota, accounts, queue)
	requestJob := func(prompt string) (*httptest.ResponseRecorder, string) {
		t.Helper()
		response := postTrialAIPrompt(t, router, prompt)
		if response.Code != http.StatusAccepted {
			t.Fatalf("job status=%d: %s", response.Code, response.Body.String())
		}
		var data struct {
			Job struct {
				ID string `json:"id"`
			} `json:"job"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil || data.Job.ID == "" {
			t.Fatalf("job ID missing: %s (%v)", response.Body.String(), err)
		}
		return response, data.Job.ID
	}
	cancel := func(id string, cookies []*http.Cookie) int {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/essai/api/ai-jobs/"+id+"/cancel", strings.NewReader(`{}`))
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response.Code
	}
	first, firstID := requestJob("WordPress")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first job did not start")
	}
	second, secondID := requestJob("Nextcloud")
	if quota.state.Total != 2 {
		t.Fatalf("expected two reserved jobs, got %d", quota.state.Total)
	}
	if status := cancel(secondID, first.Result().Cookies()); status != http.StatusNotFound {
		t.Fatalf("another visitor cancelled a job: %d", status)
	}
	if quota.state.Total != 2 {
		t.Fatal("cross-owner cancellation changed quota")
	}
	if status := cancel(secondID, second.Result().Cookies()); status != http.StatusOK {
		t.Fatalf("owner could not cancel queued job: %d", status)
	}
	if quota.state.Total != 1 {
		t.Fatalf("queued cancellation did not refund its reservation: %d", quota.state.Total)
	}
	if status := cancel(firstID, first.Result().Cookies()); status != http.StatusOK {
		t.Fatalf("owner could not cancel running job: %d", status)
	}
	if quota.state.Total != 1 {
		t.Fatal("running job was refunded")
	}
}

func TestTrialGuestJobRemainsPrivateAcrossLogin(t *testing.T) {
	dir := t.TempDir()
	quota, err := newAIQuota(filepath.Join(dir, "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := newAccountStore(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.create("alice", "Trial-Test-Password-42!"); err != nil {
		t.Fatal(err)
	}
	queue := services.NewInfrastructureJobQueue(1, 1, func(ctx context.Context, _ string) (*services.MagicAIResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	t.Cleanup(queue.Close)
	router := gin.New()
	router.Use(sessions.Sessions("trial-test", cookie.NewStore([]byte("trial-test-cookie-key-32-bytes!!!!"))))
	api := router.Group("/essai/api")
	registerAIRoutesWithQueue(api, quota, accounts, queue)
	api.POST("/test-login", func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("account", "alice")
		if session.Save() != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusOK)
	})
	api.POST("/test-logout", func(c *gin.Context) {
		session := sessions.Default(c)
		session.Delete("account")
		if session.Save() != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusOK)
	})
	request := func(method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/essai/api"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, r)
		return response
	}
	jobID := func(response *httptest.ResponseRecorder) string {
		t.Helper()
		if response.Code != http.StatusAccepted {
			t.Fatalf("job status=%d: %s", response.Code, response.Body.String())
		}
		var data struct {
			Job struct {
				ID string `json:"id"`
			} `json:"job"`
		}
		if json.Unmarshal(response.Body.Bytes(), &data) != nil || data.Job.ID == "" {
			t.Fatalf("job ID missing: %s", response.Body.String())
		}
		return data.Job.ID
	}
	guest := request(http.MethodPost, "/ai-jobs", `{"prompt":"WordPress"}`, nil)
	guestID := jobID(guest)
	guestCookies := guest.Result().Cookies()
	if status := request(http.MethodGet, "/ai-jobs/"+guestID, "", nil).Code; status != http.StatusNotFound {
		t.Fatalf("other visitor read guest job: %d", status)
	}
	login := request(http.MethodPost, "/test-login", `{}`, guestCookies)
	if login.Code != http.StatusOK {
		t.Fatalf("login failed: %d", login.Code)
	}
	loginCookies := login.Result().Cookies()
	if status := request(http.MethodGet, "/ai-jobs/"+guestID, "", loginCookies).Code; status != http.StatusOK {
		t.Fatalf("guest job lost after login: %d", status)
	}
	otherLogin := request(http.MethodPost, "/test-login", `{}`, nil)
	if status := request(http.MethodGet, "/ai-jobs/"+guestID, "", otherLogin.Result().Cookies()).Code; status != http.StatusNotFound {
		t.Fatalf("same account in another session read guest job: %d", status)
	}
	if status := request(http.MethodPost, "/ai-jobs/"+guestID+"/cancel", `{}`, loginCookies).Code; status != http.StatusOK {
		t.Fatalf("guest job could not be cancelled after login: %d", status)
	}
	account := request(http.MethodPost, "/ai-jobs", `{"prompt":"Nextcloud"}`, loginCookies)
	accountID := jobID(account)
	logout := request(http.MethodPost, "/test-logout", `{}`, loginCookies)
	if logout.Code != http.StatusOK {
		t.Fatalf("logout failed: %d", logout.Code)
	}
	logoutCookies := logout.Result().Cookies()
	if status := request(http.MethodGet, "/ai-jobs/"+accountID, "", logoutCookies).Code; status != http.StatusNotFound {
		t.Fatalf("account job remained visible after logout: %d", status)
	}
	if status := request(http.MethodGet, "/ai-jobs/"+guestID, "", logoutCookies).Code; status != http.StatusOK {
		t.Fatalf("original guest job lost after logout: %d", status)
	}
}
