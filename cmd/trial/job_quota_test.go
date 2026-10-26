package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"autodocker-go/services"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

func TestJobQuotaReservationRefundIsIdempotent(t *testing.T) {
	quota, err := newAIQuota(filepath.Join(t.TempDir(), "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	allowed, reservation, err := quota.reserveJob("visitor", 2, "connection", 6)
	if err != nil || !allowed || reservation == nil {
		t.Fatalf("reserve: allowed=%t reservation=%v err=%v", allowed, reservation, err)
	}
	var refunds sync.WaitGroup
	for i := 0; i < 32; i++ {
		refunds.Add(1)
		go func() {
			defer refunds.Done()
			if err := reservation.Refund(); err != nil {
				t.Errorf("refund: %v", err)
			}
		}()
	}
	refunds.Wait()
	quota.mu.Lock()
	defer quota.mu.Unlock()
	if quota.state.Total != 0 || quota.state.Visitors[reservation.subjectKey] != 0 || quota.state.Visitors[reservation.secondaryKey] != 0 {
		t.Fatalf("refund changed quota more than once: %+v", quota.state)
	}
}

func TestTrialQueuedCancellationRestoresRealAPIQuota(t *testing.T) {
	dir := t.TempDir()
	quota, err := newAIQuota(filepath.Join(dir, "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := newAccountStore(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	queue := services.NewInfrastructureJobQueue(1, 4, func(ctx context.Context, prompt string) (*services.MagicAIResult, error) {
		if prompt == "blocker" {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &services.MagicAIResult{Confidence: "validated"}, nil
	})
	defer queue.Close()
	if _, err := queue.Submit("blocker-owner", "blocker"); err != nil {
		t.Fatal(err)
	}
	<-started
	router := gin.New()
	router.Use(sessions.Sessions("trial-test", cookie.NewStore([]byte("trial-test-cookie-key-32-bytes!!!!"))))
	registerAIRoutesWithQueue(router.Group("/essai/api"), quota, accounts, queue)
	request := httptest.NewRequest(http.MethodPost, "/essai/api/ai-jobs", strings.NewReader(`{"prompt":"Un site WordPress"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("submit status=%d body=%s", response.Code, response.Body.String())
	}
	var accepted struct {
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil || accepted.Job.ID == "" {
		t.Fatalf("missing job id: %s (%v)", response.Body.String(), err)
	}
	quota.mu.Lock()
	reserved := quota.state.Total
	quota.mu.Unlock()
	if reserved != 1 {
		t.Fatalf("queued job consumed %d quota units, want 1", reserved)
	}
	for attempt := 0; attempt < 2; attempt++ {
		cancel := httptest.NewRequest(http.MethodPost, "/essai/api/ai-jobs/"+accepted.Job.ID+"/cancel", nil)
		for _, saved := range response.Result().Cookies() {
			cancel.AddCookie(saved)
		}
		result := httptest.NewRecorder()
		router.ServeHTTP(result, cancel)
		if result.Code != http.StatusOK {
			t.Fatalf("cancel status=%d body=%s", result.Code, result.Body.String())
		}
	}
	quota.mu.Lock()
	remaining := quota.state.Total
	quota.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("cancelled queued job kept %d quota units", remaining)
	}
}

func TestJobQuotaOldDayRefundDoesNotTouchNewDay(t *testing.T) {
	quota, err := newAIQuota(filepath.Join(t.TempDir(), "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	allowed, oldReservation, err := quota.reserveJob("visitor", 2, "connection", 6)
	if err != nil || !allowed {
		t.Fatalf("old reservation: allowed=%t err=%v", allowed, err)
	}
	// Model an unstarted job reserved yesterday and expiring after UTC midnight.
	oldReservation.day = time.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02")
	allowed, todayReservation, err := quota.reserveJob("visitor", 2, "connection", 6)
	if err != nil || !allowed {
		t.Fatalf("new reservation: allowed=%t err=%v", allowed, err)
	}
	if err := oldReservation.Refund(); err != nil {
		t.Fatal(err)
	}
	quota.mu.Lock()
	if quota.state.Total != 2 || quota.state.Visitors[todayReservation.subjectKey] != 2 || quota.state.Visitors[todayReservation.secondaryKey] != 2 {
		t.Fatalf("old job decremented today's counters: %+v", quota.state)
	}
	quota.mu.Unlock()
	if err := todayReservation.Refund(); err != nil {
		t.Fatal(err)
	}
	quota.mu.Lock()
	defer quota.mu.Unlock()
	if quota.state.Total != 1 {
		t.Fatalf("today's job was not refunded: %+v", quota.state)
	}
}
