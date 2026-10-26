package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autodocker-go/config"
	"autodocker-go/services"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

func TestInfrastructureJobHTTPPrivacyAndSixUserQueue(t *testing.T) {
	previous := infrastructureJobQueue
	oldBypass, oldDemo := config.AuthBypass, config.DemoMode
	config.AuthBypass = false
	config.DemoMode = false
	started := make(chan struct{}, 6)
	release := make(chan struct{})
	q := services.NewInfrastructureJobQueue(3, 24, func(ctx context.Context, prompt string) (*services.MagicAIResult, error) {
		started <- struct{}{}
		select {
		case <-release:
			return &services.MagicAIResult{Summary: prompt, Confidence: "validated", Source: "synthetic-api"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	infrastructureJobQueue = func() *services.InfrastructureJobQueue { return q }
	defer func() {
		q.Close()
		infrastructureJobQueue = previous
		config.AuthBypass = oldBypass
		config.DemoMode = oldDemo
	}()
	r := gin.New()
	r.Use(sessions.Sessions("test_session", cookie.NewStore([]byte("test-job-cookie-signing-key-32bytes"))))
	// Test identities are injected only by this local test router.
	r.Use(func(c *gin.Context) {
		s := sessions.Default(c)
		if user := c.GetHeader("X-Test-User"); user != "" {
			s.Set("user", user)
		}
		c.Next()
	})
	registerInfrastructureJobRoutes(r.Group("/api"))
	request := func(method, path, user, body string) (int, map[string]interface{}) {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-User", user)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var data map[string]interface{}
		if json.Unmarshal(w.Body.Bytes(), &data) != nil {
			t.Fatal("non-JSON API response")
		}
		if strings.Contains(w.Body.String(), "X-Test-User") {
			t.Fatal("identity leaked")
		}
		return w.Code, data
	}
	ids := make([]string, 6)
	for i := range ids {
		status, data := request("POST", "/api/ai-jobs", fmt.Sprintf("user-%d", i), fmt.Sprintf(`{"prompt":"private-%d"}`, i))
		if status != 202 {
			t.Fatalf("six users rejected: %d", status)
		}
		ids[i] = data["job"].(map[string]interface{})["id"].(string)
	}
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers did not run")
		}
	}
	for _, suffix := range []string{"", "/cancel"} {
		method := http.MethodGet
		if suffix != "" {
			method = http.MethodPost
		}
		status, data := request(method, "/api/ai-jobs/"+ids[0]+suffix, "another-user", "{}")
		if status != 404 || data["result"] != nil {
			t.Fatal("cross-user access accepted")
		}
	}
	if status, _ := request("POST", "/api/ai-jobs", "", `{"prompt":"anonymous"}`); status != 401 {
		t.Fatal("anonymous job accepted")
	}
	close(release)
	for i, id := range ids {
		deadline := time.Now().Add(2 * time.Second)
		success := false
		for time.Now().Before(deadline) {
			status, data := request("GET", "/api/ai-jobs/"+id, fmt.Sprintf("user-%d", i), "")
			if status != 200 {
				t.Fatal("own job unavailable")
			}
			job := data["job"].(map[string]interface{})
			if job["state"] == "succeeded" {
				result := data["result"].(map[string]interface{})
				if result["summary"] != fmt.Sprintf("private-%d", i) || result["source"] != "synthetic-api" {
					t.Fatal("results mixed between accounts")
				}
				success = true
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !success {
			t.Fatal("six jobs did not complete")
		}
	}
}
