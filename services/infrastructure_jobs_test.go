package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func awaitInfrastructureJob(t *testing.T, q *InfrastructureJobQueue, owner, id string) InfrastructureJobView {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view, err := q.Get(owner, id)
		if err != nil {
			t.Fatal(err)
		}
		if view.State == "succeeded" || view.State == "failed" || view.State == "cancelled" {
			return view
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job did not finish")
	return InfrastructureJobView{}
}

func TestInfrastructureSixUsersShareThreeWorkersAndKeepPrivateResults(t *testing.T) {
	started := make(chan struct{}, 6)
	release := make(chan struct{})
	var active, peak atomic.Int32
	q := NewInfrastructureJobQueue(3, 24, func(ctx context.Context, prompt string) (*MagicAIResult, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &MagicAIResult{Summary: prompt, Confidence: "validated", Source: "synthetic-api"}, nil
	})
	defer q.Close()
	jobs := make([]InfrastructureJobView, 6)
	var submissions sync.WaitGroup
	submitErrors := make(chan error, len(jobs))
	for i := range jobs {
		submissions.Add(1)
		go func(index int) {
			defer submissions.Done()
			var err error
			jobs[index], err = q.Submit(fmt.Sprintf("user-%d", index), fmt.Sprintf("private request %d", index))
			submitErrors <- err
		}(i)
	}
	submissions.Wait()
	for range jobs {
		if err := <-submitErrors; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("three jobs did not start")
		}
	}
	queued := 0
	for i, job := range jobs {
		v, _ := q.Get(fmt.Sprintf("user-%d", i), job.ID)
		if v.State == "queued" {
			queued++
			if v.Position < 1 {
				t.Fatal("queue position missing")
			}
		}
		if _, err := q.Get("another-user", job.ID); !errors.Is(err, ErrInfrastructureJobMissing) {
			t.Fatal("other user's outcome disclosed")
		}
	}
	if queued != 3 || peak.Load() != 3 {
		t.Fatalf("unbounded concurrency or missing queue: %d queued, peak%d", queued, peak.Load())
	}
	close(release)
	for i, job := range jobs {
		v := awaitInfrastructureJob(t, q, fmt.Sprintf("user-%d", i), job.ID)
		if v.State != "succeeded" || v.Result.Summary != fmt.Sprintf("private request %d", i) {
			t.Fatal("result mixed between users")
		}
	}
}

func TestInfrastructureQueueFIFOOwnershipCancellationAndBounds(t *testing.T) {
	started := make(chan string, 3)
	release := make(chan struct{})
	q := NewInfrastructureJobQueue(1, 2, func(ctx context.Context, prompt string) (*MagicAIResult, error) {
		started <- prompt
		select {
		case <-release:
			return &MagicAIResult{Summary: prompt}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	defer q.Close()
	first, _ := q.Submit("one", "first")
	<-started
	second, _ := q.Submit("two", "second")
	third, _ := q.Submit("three", "third")
	if _, err := q.Submit("four", "overflow"); !errors.Is(err, ErrInfrastructureQueueFull) {
		t.Fatal("queue has no capacity bound")
	}
	if _, err := q.Submit("one", "different"); !errors.Is(err, ErrInfrastructureUserBusy) {
		t.Fatal("one user can monopolise the workers")
	}
	if duplicate, err := q.Submit("one", "first"); err != nil || duplicate.ID != first.ID {
		t.Fatal("identical submission is billed twice")
	}
	if duplicate, created, err := q.SubmitWithStatus("one", "first"); err != nil || created || duplicate.ID != first.ID {
		t.Fatal("duplicate job must be reported as reused")
	}
	if _, err := q.Cancel("one", second.ID); !errors.Is(err, ErrInfrastructureJobMissing) {
		t.Fatal("cross-user cancellation accepted")
	}
	if view, err := q.Cancel("two", second.ID); err != nil || view.State != "cancelled" {
		t.Fatal("own queued cancellation failed")
	}
	if view, _ := q.Get("three", third.ID); view.Position != 1 {
		t.Fatal("cancelled job retains a queue position")
	}
	close(release)
	awaitInfrastructureJob(t, q, "one", first.ID)
	awaitInfrastructureJob(t, q, "three", third.ID)
	if next := <-started; next != "third" {
		t.Fatal("FIFO order lost or cancelled job was executed")
	}
}

func TestInfrastructureRunningCancellationReleasesCapacityAndClearsPrompt(t *testing.T) {
	started := make(chan struct{}, 1)
	q := NewInfrastructureJobQueue(1, 2, func(ctx context.Context, prompt string) (*MagicAIResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	defer q.Close()
	job, _ := q.Submit("owner", "private prompt")
	<-started
	q.Cancel("owner", job.ID)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		active := q.active
		prompt := q.jobs[job.ID].prompt
		q.mu.Unlock()
		if active == 0 && prompt == "" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("cancellation kept a running slot or private prompt")
}

func TestInfrastructureJobRetentionAndQueueExpiry(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	q := NewInfrastructureJobQueue(1, 2, func(ctx context.Context, prompt string) (*MagicAIResult, error) {
		started <- struct{}{}
		select {
		case <-release:
			return &MagicAIResult{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	defer q.Close()
	first, _ := q.Submit("one", "first")
	<-started
	second, _ := q.Submit("two", "second")
	q.mu.Lock()
	q.jobs[second.ID].created = time.Now().Add(-181 * time.Second)
	q.mu.Unlock()
	if v, _ := q.Get("two", second.ID); v.State != "failed" || v.Error == "" {
		t.Fatal("queue timeout ignored")
	}
	q.mu.Lock()
	q.jobs[second.ID].finished = time.Now().Add(-11 * time.Minute)
	q.mu.Unlock()
	if _, err := q.Get("two", second.ID); !errors.Is(err, ErrInfrastructureJobMissing) {
		t.Fatal("expired private result retained")
	}
	close(release)
	awaitInfrastructureJob(t, q, "one", first.ID)
}

func TestInfrastructureWorkerSurvivesGeneratorPanic(t *testing.T) {
	q := NewInfrastructureJobQueue(1, 2, func(context.Context, string) (*MagicAIResult, error) { panic("private implementation details") })
	defer q.Close()
	job, _ := q.Submit("one", "prompt")
	v := awaitInfrastructureJob(t, q, "one", job.ID)
	if v.State != "failed" || v.Error == "private implementation details" {
		t.Fatal("worker panic escaped or leaked")
	}
	second, err := q.Submit("two", "second")
	if err != nil {
		t.Fatal(err)
	}
	awaitInfrastructureJob(t, q, "two", second.ID)
}

func TestInfrastructureQueuedAbandonCallbackCancelExpiryAndClose(t *testing.T) {
	for _, reason := range []string{"cancel", "expiry", "close"} {
		t.Run(reason, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			q := NewInfrastructureJobQueue(1, 2, func(ctx context.Context, prompt string) (*MagicAIResult, error) {
				if prompt == "first" {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
				return &MagicAIResult{}, nil
			})
			defer q.Close()
			first, err := q.Submit("one", "first")
			if err != nil {
				t.Fatal(err)
			}
			<-started
			var refunds atomic.Int32
			second, created, err := q.SubmitWithStatusContextOnAbandon(context.Background(), "two", "second", func() {
				refunds.Add(1)
				// Re-entering the queue proves the callback is outside its lock.
				_, _ = q.Get("one", first.ID)
			})
			if err != nil || !created {
				t.Fatalf("queued submit: created=%t err=%v", created, err)
			}
			switch reason {
			case "cancel":
				if _, err := q.Cancel("two", second.ID); err != nil {
					t.Fatal(err)
				}
				_, _ = q.Cancel("two", second.ID)
			case "expiry":
				q.mu.Lock()
				q.jobs[second.ID].created = time.Now().Add(-q.queueTimeout - time.Second)
				q.mu.Unlock()
				if view, err := q.Get("two", second.ID); err != nil || view.State != "failed" {
					t.Fatalf("expired job: %+v %v", view, err)
				}
			case "close":
				q.Close()
			}
			if got := refunds.Load(); got != 1 {
				t.Fatalf("abandon refund count=%d, want 1", got)
			}
			close(release)
		})
	}
}

func TestInfrastructureAbandonAndWorkerStartRaceRefundsOnlyUnstartedJobs(t *testing.T) {
	for attempt := 0; attempt < 64; attempt++ {
		started := make(chan struct{})
		release := make(chan struct{})
		var runs, refunds atomic.Int32
		q := NewInfrastructureJobQueue(1, 2, func(ctx context.Context, prompt string) (*MagicAIResult, error) {
			if prompt == "first" {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
				}
			} else {
				runs.Add(1)
			}
			return &MagicAIResult{}, nil
		})
		if _, err := q.Submit("one", "first"); err != nil {
			t.Fatal(err)
		}
		<-started
		second, created, err := q.SubmitWithStatusContextOnAbandon(context.Background(), "two", "second", func() { refunds.Add(1) })
		if err != nil || !created {
			t.Fatalf("queued submit: created=%t err=%v", created, err)
		}
		var race sync.WaitGroup
		race.Add(2)
		go func() { defer race.Done(); close(release) }()
		go func() { defer race.Done(); _, _ = q.Cancel("two", second.ID) }()
		race.Wait()
		q.Close()
		if got := refunds.Load() + runs.Load(); got != 1 {
			t.Fatalf("attempt %d: refund=%d run=%d, want exactly one", attempt, refunds.Load(), runs.Load())
		}
	}
}

func TestInfrastructureRouterAllowsDistinctAPIsButSerialisesEachProvider(t *testing.T) {
	router := newInfrastructureRouter()
	providers := []infrastructureProvider{{name: "groq"}, {name: "cloudflare"}, {name: "nvidia"}}
	started := make(chan string, 6)
	release := make(chan struct{})
	results := make(chan error, 6)
	var mu sync.Mutex
	current := map[string]int{}
	peak := map[string]int{}
	run := func(ctx context.Context, prompt string, p infrastructureProvider) (*MagicAIResult, error) {
		mu.Lock()
		current[p.name]++
		if current[p.name] > peak[p.name] {
			peak[p.name] = current[p.name]
		}
		mu.Unlock()
		defer func() { mu.Lock(); current[p.name]--; mu.Unlock() }()
		started <- p.name
		select {
		case <-release:
			return &MagicAIResult{Confidence: "validated"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	for i := 0; i < 6; i++ {
		go func() { _, err := router.generate(context.Background(), "prompt", providers, run); results <- err }()
	}
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("different API providers cannot run together")
		}
	}
	mu.Lock()
	distinct := len(peak)
	mu.Unlock()
	close(release)
	for i := 0; i < 6; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if distinct != 3 {
		t.Fatal("concurrency did not distribute across providers")
	}
	for _, n := range peak {
		if n > 1 {
			t.Fatal("same provider received parallel calls")
		}
	}
}
