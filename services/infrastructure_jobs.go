package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	ErrInfrastructureQueueFull  = errors.New("la file IA est pleine ; réessayez dans quelques instants")
	ErrInfrastructureUserBusy   = errors.New("une demande est déjà en cours pour votre compte ; attendez sa fin ou annulez-la dans l’onglet qui l’a lancée")
	ErrInfrastructureJobMissing = errors.New("demande introuvable ou expirée")
)

type InfrastructureJobView struct {
	ID       string         `json:"id"`
	State    string         `json:"state"`
	Position int            `json:"position"`
	Active   int            `json:"active"`
	Workers  int            `json:"workers"`
	Error    string         `json:"error,omitempty"`
	Result   *MagicAIResult `json:"-"`
}

type infrastructureJob struct {
	id, owner, prompt, state, message string
	created, finished                 time.Time
	ctx                               context.Context
	cancel                            context.CancelFunc
	result                            *MagicAIResult
	onAbandon                         func()
}

// Jobs and their results stay in bounded memory, never in browser storage or a
// database. Ownership applies even to cancellation and completed results.
type InfrastructureJobQueue struct {
	mu                                  sync.Mutex
	cond                                *sync.Cond
	jobs                                map[string]*infrastructureJob
	waiting                             []*infrastructureJob
	workers, capacity, active           int
	closed                              bool
	done                                chan struct{}
	wg                                  sync.WaitGroup
	run                                 func(context.Context, string) (*MagicAIResult, error)
	runTimeout, queueTimeout, retention time.Duration
}

var defaultInfrastructureJobs struct {
	sync.Once
	queue *InfrastructureJobQueue
}

func InfrastructureJobs() *InfrastructureJobQueue {
	defaultInfrastructureJobs.Do(func() {
		workers, _ := strconv.Atoi(infrastructureEnvDefault("AUTODOCKER_AI_WORKERS", "3"))
		if workers < 1 || workers > 6 {
			workers = 3
		}
		defaultInfrastructureJobs.queue = NewInfrastructureJobQueue(workers, 24, GenerateInfrastructureConfig)
	})
	return defaultInfrastructureJobs.queue
}

func NewInfrastructureJobQueue(workers, capacity int, run func(context.Context, string) (*MagicAIResult, error)) *InfrastructureJobQueue {
	if workers < 1 {
		workers = 1
	}
	if workers > 6 {
		workers = 6
	}
	if capacity < 1 {
		capacity = 1
	}
	if capacity > 64 {
		capacity = 64
	}
	q := &InfrastructureJobQueue{jobs: map[string]*infrastructureJob{}, workers: workers, capacity: capacity, done: make(chan struct{}), run: run, runTimeout: 150 * time.Second, queueTimeout: 180 * time.Second, retention: 10 * time.Minute}
	q.cond = sync.NewCond(&q.mu)
	for i := 0; i < workers; i++ {
		q.wg.Add(1)
		go q.worker()
	}
	q.wg.Add(1)
	go q.janitor()
	return q
}

func (q *InfrastructureJobQueue) Submit(owner, prompt string) (InfrastructureJobView, error) {
	view, _, err := q.SubmitWithStatus(owner, prompt)
	return view, err
}

// SubmitWithStatus reports whether a new job was created. A repeated prompt
// for an active job returns that existing job without spending another quota.
func (q *InfrastructureJobQueue) SubmitWithStatus(owner, prompt string) (InfrastructureJobView, bool, error) {
	return q.SubmitWithStatusContext(context.Background(), owner, prompt)
}

// SubmitWithStatusContext retains only a caller-created detached context (for
// example a frozen provider choice), never an HTTP request context.
func (q *InfrastructureJobQueue) SubmitWithStatusContext(base context.Context, owner, prompt string) (InfrastructureJobView, bool, error) {
	return q.SubmitWithStatusContextOnAbandon(base, owner, prompt, nil)
}

// SubmitWithStatusContextOnAbandon calls onAbandon once if a newly queued job
// is cancelled, expires, or the queue closes before a worker starts it. The
// callback runs outside the queue lock. Callers must release their own
// reservation when this method returns an error or an existing job.
func (q *InfrastructureJobQueue) SubmitWithStatusContextOnAbandon(base context.Context, owner, prompt string, onAbandon func()) (InfrastructureJobView, bool, error) {
	prompt = strings.TrimSpace(prompt)
	if owner == "" || prompt == "" || utf8.RuneCountInString(prompt) > 8000 {
		return InfrastructureJobView{}, false, fmt.Errorf("décrivez votre infrastructure en 8 000 caractères maximum")
	}
	idBytes := make([]byte, 24)
	if _, err := rand.Read(idBytes); err != nil {
		return InfrastructureJobView{}, false, fmt.Errorf("création de la demande impossible")
	}
	q.mu.Lock()
	var abandoned []func()
	defer func() {
		q.mu.Unlock()
		runAbandonCallbacks(abandoned)
	}()
	abandoned = q.pruneLocked(time.Now())
	if q.closed {
		return InfrastructureJobView{}, false, ErrInfrastructureQueueFull
	}
	for _, job := range q.jobs {
		if job.owner == owner && (job.state == "queued" || job.state == "running") {
			if job.prompt == prompt {
				return q.viewLocked(job), false, nil
			}
			return InfrastructureJobView{}, false, ErrInfrastructureUserBusy
		}
	}
	if len(q.waiting) >= q.capacity {
		return InfrastructureJobView{}, false, ErrInfrastructureQueueFull
	}
	// Keep at most 64 retained outcomes in addition to active/waiting work.
	for len(q.jobs) >= q.capacity+q.workers+64 {
		var oldest *infrastructureJob
		for _, job := range q.jobs {
			if !job.finished.IsZero() && (oldest == nil || job.finished.Before(oldest.finished)) {
				oldest = job
			}
		}
		if oldest == nil {
			return InfrastructureJobView{}, false, ErrInfrastructureQueueFull
		}
		delete(q.jobs, oldest.id)
	}
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	job := &infrastructureJob{id: hex.EncodeToString(idBytes), owner: owner, prompt: prompt, state: "queued", created: time.Now(), ctx: ctx, cancel: cancel, onAbandon: onAbandon}
	q.jobs[job.id] = job
	q.waiting = append(q.waiting, job)
	q.cond.Signal()
	return q.viewLocked(job), true, nil
}

func (q *InfrastructureJobQueue) Get(owner, id string) (InfrastructureJobView, error) {
	q.mu.Lock()
	abandoned := q.pruneLocked(time.Now())
	defer func() {
		q.mu.Unlock()
		runAbandonCallbacks(abandoned)
	}()
	job, ok := q.jobs[id]
	if !ok || job.owner != owner {
		return InfrastructureJobView{}, ErrInfrastructureJobMissing
	}
	return q.viewLocked(job), nil
}

func (q *InfrastructureJobQueue) Cancel(owner, id string) (InfrastructureJobView, error) {
	q.mu.Lock()
	abandoned := q.pruneLocked(time.Now())
	defer func() {
		q.mu.Unlock()
		runAbandonCallbacks(abandoned)
	}()
	job, ok := q.jobs[id]
	if !ok || job.owner != owner {
		return InfrastructureJobView{}, ErrInfrastructureJobMissing
	}
	if job.state == "queued" || job.state == "running" {
		if job.state == "queued" {
			abandoned = append(abandoned, takeAbandonLocked(job)...)
		}
		job.state = "cancelled"
		job.prompt = ""
		job.finished = time.Now()
		job.cancel()
		abandoned = append(abandoned, q.pruneLocked(time.Now())...)
		q.cond.Broadcast()
	}
	return q.viewLocked(job), nil
}

func (q *InfrastructureJobQueue) viewLocked(job *infrastructureJob) InfrastructureJobView {
	position := 0
	if job.state == "queued" {
		for _, waiting := range q.waiting {
			if waiting.state == "queued" {
				position++
			}
			if waiting == job {
				break
			}
		}
	}
	return InfrastructureJobView{ID: job.id, State: job.state, Position: position, Active: q.active, Workers: q.workers, Error: job.message, Result: job.result}
}

func (q *InfrastructureJobQueue) pruneLocked(now time.Time) []func() {
	var abandoned []func()
	waiting := q.waiting[:0]
	for _, job := range q.waiting {
		if job.state == "queued" && now.Sub(job.created) >= q.queueTimeout {
			abandoned = append(abandoned, takeAbandonLocked(job)...)
			job.state = "failed"
			job.message = "attente trop longue ; réessayez plus tard"
			job.prompt = ""
			job.finished = now
			job.cancel()
		}
		if job.state == "queued" {
			waiting = append(waiting, job)
		}
	}
	for i := len(waiting); i < len(q.waiting); i++ {
		q.waiting[i] = nil
	}
	q.waiting = waiting
	for id, job := range q.jobs {
		if !job.finished.IsZero() && now.Sub(job.finished) >= q.retention {
			delete(q.jobs, id)
		}
	}
	return abandoned
}

// The queue lock serialises the decision to start or abandon a job, so a
// callback can only be claimed by one terminal path.
func takeAbandonLocked(job *infrastructureJob) []func() {
	if job.onAbandon == nil {
		return nil
	}
	callback := job.onAbandon
	job.onAbandon = nil
	return []func(){callback}
}

func runAbandonCallbacks(callbacks []func()) {
	for _, callback := range callbacks {
		func() {
			defer func() { _ = recover() }()
			callback()
		}()
	}
}

func (q *InfrastructureJobQueue) worker() {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		abandoned := q.pruneLocked(time.Now())
		for len(q.waiting) == 0 && !q.closed {
			if len(abandoned) > 0 {
				q.mu.Unlock()
				runAbandonCallbacks(abandoned)
				q.mu.Lock()
				abandoned = q.pruneLocked(time.Now())
				continue
			}
			q.cond.Wait()
			abandoned = append(abandoned, q.pruneLocked(time.Now())...)
		}
		if q.closed {
			q.mu.Unlock()
			runAbandonCallbacks(abandoned)
			return
		}
		job := q.waiting[0]
		q.waiting[0] = nil
		q.waiting = q.waiting[1:]
		job.state = "running"
		job.onAbandon = nil
		q.active++
		prompt := job.prompt
		q.mu.Unlock()
		runAbandonCallbacks(abandoned)
		ctx, cancel := context.WithTimeout(job.ctx, q.runTimeout)
		result, err := runInfrastructureJob(q.run, ctx, prompt)
		cancel()
		q.mu.Lock()
		q.active--
		job.prompt = ""
		if job.state != "cancelled" {
			job.finished = time.Now()
			if err != nil || result == nil {
				job.state = "failed"
				job.message = "la génération a échoué ; réessayez ou utilisez le catalogue"
				if err != nil {
					job.message = boundedAIText(err.Error(), 600)
				}
				if errors.Is(err, context.DeadlineExceeded) {
					job.message = "le délai de génération est dépassé ; réessayez ou utilisez le catalogue"
				}
			} else {
				job.state = "succeeded"
				job.result = result
			}
		}
		job.cancel()
		q.mu.Unlock()
	}
}

func runInfrastructureJob(run func(context.Context, string) (*MagicAIResult, error), ctx context.Context, prompt string) (result *MagicAIResult, err error) {
	defer func() {
		if recover() != nil {
			result = nil
			err = fmt.Errorf("erreur interne de génération ; réessayez ou utilisez le catalogue")
		}
	}()
	return run(ctx, prompt)
}

func (q *InfrastructureJobQueue) janitor() {
	defer q.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-q.done:
			return
		case <-ticker.C:
			q.mu.Lock()
			abandoned := q.pruneLocked(time.Now())
			q.mu.Unlock()
			runAbandonCallbacks(abandoned)
		}
	}
}

func (q *InfrastructureJobQueue) Close() {
	q.mu.Lock()
	var abandoned []func()
	if !q.closed {
		q.closed = true
		close(q.done)
		for _, job := range q.jobs {
			if job.state == "queued" {
				abandoned = append(abandoned, takeAbandonLocked(job)...)
				job.state = "cancelled"
				job.prompt = ""
				job.finished = time.Now()
			}
			job.cancel()
		}
		q.waiting = nil
		q.cond.Broadcast()
	}
	q.mu.Unlock()
	runAbandonCallbacks(abandoned)
	q.wg.Wait()
}
