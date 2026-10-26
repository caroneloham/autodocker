// Public AutoDocker trial. This isolated process exposes generation, a bounded
// AI queue, and trial accounts; it has no Docker or host management routes.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"autodocker-go/handlers"
	"autodocker-go/services"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

type visitorBudget struct {
	until time.Time
	count int
}

type limiter struct {
	mu                      sync.Mutex
	visitors                map[string]visitorBudget
	global                  visitorBudget
	perIPLimit, globalLimit int
}

func newLimiter(perIPLimit, globalLimit int) *limiter {
	return &limiter{visitors: map[string]visitorBudget{}, perIPLimit: perIPLimit, globalLimit: globalLimit}
}

func (l *limiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.After(l.global.until) {
		l.global = visitorBudget{until: now.Add(10 * time.Minute)}
	}
	if l.global.count >= l.globalLimit {
		return false
	}
	entry := l.visitors[ip]
	if now.After(entry.until) {
		entry = visitorBudget{until: now.Add(10 * time.Minute)}
	}
	if entry.count >= l.perIPLimit {
		return false
	}
	entry.count++
	l.visitors[ip] = entry
	l.global.count++
	if len(l.visitors) > 10000 {
		for key, value := range l.visitors {
			if now.After(value.until) {
				delete(l.visitors, key)
			}
		}
	}
	return true
}

func visitorIP(request *http.Request) string {
	remote, _, _ := net.SplitHostPort(request.RemoteAddr)
	if remote == "127.0.0.1" || remote == "::1" {
		// The local reverse proxy appends its observed peer as the rightmost hop.
		// Earlier values may have been supplied by the visitor and cannot key quotas.
		hops := strings.Split(request.Header.Get("X-Forwarded-For"), ",")
		forwarded := strings.TrimSpace(hops[len(hops)-1])
		if ip := net.ParseIP(forwarded); ip != nil {
			return ip.String()
		}
	}
	return remote
}

func trialAPIMiddleware() gin.HandlerFunc {
	accountLimits := newLimiter(12, 300)
	aiLimits := newLimiter(12, 300)
	generationLimits := newLimiter(120, 3000)
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin != "" && origin != "https://autodocker.eu" && origin != "https://www.autodocker.eu" && origin != "http://127.0.0.1:18082" {
			c.AbortWithStatusJSON(403, gin.H{"status": "error", "error": "Origine refusée"})
			return
		}
		path := c.Request.URL.Path
		bodyLimit := int64(4096)
		var budget *limiter
		if c.Request.Method == http.MethodPost {
			switch path {
			case "/essai/api/generate", "/essai/api/generate/preview":
				budget, bodyLimit = generationLimits, 2<<20
			case "/essai/api/ai-jobs":
				// 2,000 Unicode code points can occupy 24 KiB when emoji are JSON-escaped.
				budget, bodyLimit = aiLimits, 32<<10
			case "/essai/api/signup", "/essai/api/login":
				budget = accountLimits
			case "/essai/api/logout":
				// Users must be able to end their session after exhausting login attempts.
			default:
				if !strings.HasPrefix(path, "/essai/api/ai-jobs/") || !strings.HasSuffix(path, "/cancel") {
					budget = accountLimits
				}
			}
			if budget != nil && !budget.allow(visitorIP(c.Request), time.Now()) {
				c.Header("Retry-After", "600")
				c.AbortWithStatusJSON(429, gin.H{"status": "error", "error": "Limite de l’essai atteinte. Revenez dans dix minutes."})
				return
			}
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, bodyLimit)
		session := sessions.Default(c)
		session.Set("user", "trial-visitor")
		session.Set("role", "user")
		c.Next()
	}
}

func main() {
	gin.SetMode(gin.ReleaseMode)
	assets := os.Getenv("AUTODOCKER_TRIAL_ASSETS")
	if assets == "" {
		assets = filepath.Join("cmd", "trial", "ui")
	}
	if _, err := os.Stat(filepath.Join(assets, "index.html")); err != nil {
		panic("trial assets missing")
	}
	key := make([]byte, 32)
	if keyFile := os.Getenv("AUTODOCKER_TRIAL_COOKIE_KEY_FILE"); keyFile != "" {
		contents, err := os.ReadFile(keyFile)
		if err != nil || len(contents) != 32 {
			panic("trial cookie key missing or invalid")
		}
		copy(key, contents)
	} else if os.Getenv("AUTODOCKER_TRIAL_LOCAL") == "1" {
		if _, err := rand.Read(key); err != nil {
			panic(err)
		}
	} else {
		panic("AUTODOCKER_TRIAL_COOKIE_KEY_FILE required")
	}
	store := cookie.NewStore(key)
	store.Options(sessions.Options{Path: "/essai", HttpOnly: true, MaxAge: 7 * 86400, SameSite: http.SameSiteStrictMode, Secure: os.Getenv("AUTODOCKER_TRIAL_LOCAL") != "1"})
	router := gin.New()
	router.Use(gin.Recovery(), sessions.Sessions("autodocker-trial", store))
	router.Use(func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Cache-Control", "no-store")
		c.Next()
	})
	router.GET("/essai", func(c *gin.Context) { c.Redirect(http.StatusTemporaryRedirect, "/essai/") })
	router.GET("/essai/", func(c *gin.Context) { c.File(filepath.Join(assets, "index.html")) })
	for _, name := range []string{"app.js", "workspace.css", "favicon.svg"} {
		name := name
		router.GET("/essai/"+name, func(c *gin.Context) { c.File(filepath.Join(assets, name)) })
	}
	router.GET("/essai/tracer", func(c *gin.Context) { c.Redirect(http.StatusTemporaryRedirect, "/essai/tracer/") })
	router.GET("/essai/tracer/", func(c *gin.Context) { c.File(filepath.Join(assets, "tracer", "index.html")) })
	router.GET("/essai/tracer/assets/*path", func(c *gin.Context) {
		name := strings.TrimPrefix(c.Param("path"), "/")
		if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.HasPrefix(name, ".") {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.File(filepath.Join(assets, "tracer", "assets", name))
	})
	for _, name := range []string{
		"Docker-Tracer-0.12.0-Windows-x64.zip",
		"Docker-Tracer-0.12.1-Windows-x64.zip",
		"Docker-Tracer-0.12.1-Linux-x64.tar.gz",
		"AutoDocker-Windows-x64-preview.zip",
		"AutoDocker-Linux-x64-preview.tar.gz",
	} {
		name := name
		download := func(c *gin.Context) {
			path := filepath.Join(assets, "downloads", name)
			if _, err := os.Stat(path); err != nil {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			if strings.HasSuffix(name, ".tar.gz") {
				c.Header("Content-Type", "application/gzip")
			} else {
				c.Header("Content-Type", "application/zip")
			}
			c.FileAttachment(path, name)
		}
		router.GET("/essai/downloads/"+name, download)
		router.HEAD("/essai/downloads/"+name, download)
	}
	router.GET("/essai/compte", func(c *gin.Context) { c.Redirect(http.StatusTemporaryRedirect, "/essai/compte/") })
	router.GET("/essai/compte/", func(c *gin.Context) { c.File(filepath.Join(assets, "compte", "index.html")) })
	router.GET("/essai/compte/account.js", func(c *gin.Context) { c.File(filepath.Join(assets, "compte", "account.js")) })
	api := router.Group("/essai/api")
	api.Use(trialAPIMiddleware())
	handlers.RegisterPublicTrialRoutes(api)
	quota, err := newAIQuota(os.Getenv("AUTODOCKER_TRIAL_QUOTA_FILE"))
	if err != nil {
		panic(err)
	}
	accounts, err := newAccountStore(os.Getenv("AUTODOCKER_TRIAL_ACCOUNTS_FILE"))
	if err != nil {
		panic(err)
	}
	registerAccountRoutes(api, accounts, quota)
	if os.Getenv("GEMINI_API_KEYS") != "" || os.Getenv("GEMINI_API_KEY") != "" {
		services.InitAI()
	}
	if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
		services.SetOpenRouterKey(key)
	}
	if keys := os.Getenv("OPENROUTER_API_KEYS"); keys != "" {
		services.SetOpenRouterKeys(strings.Split(keys, ","))
	}
	if os.Getenv("GEMINI_API_KEYS") == "" && os.Getenv("GEMINI_API_KEY") == "" && !services.OpenRouterAvailable() {
		panic("public trial AI provider missing")
	}
	queue := registerAIRoutes(api, quota, accounts)
	address := os.Getenv("AUTODOCKER_TRIAL_ADDR")
	if address == "" {
		address = "127.0.0.1:18082"
	}
	server := &http.Server{Addr: address, Handler: router, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second}
	fmt.Println("AutoDocker public trial listening on", address)
	stopping := make(chan os.Signal, 1)
	signal.Notify(stopping, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stopping)
	served := make(chan error, 1)
	go func() { served <- server.ListenAndServe() }()
	var serveErr error
	select {
	case <-stopping:
		// Shutdown stops admitting requests and waits for active handlers before
		// Close refunds queued AI reservations. Force-close only after the bound.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
		cancel()
		serveErr = <-served
	case serveErr = <-served:
		_ = server.Close()
	}
	queue.Close()
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		panic(serveErr)
	}
}

type quotaState struct {
	Day       string         `json:"day"`
	Salt      string         `json:"salt"`
	Total     int            `json:"total"`
	Visitors  map[string]int `json:"visitors"`
	Signups   int            `json:"signups"`
	SignupIPs map[string]int `json:"signup_ips"`
}

type aiQuota struct {
	mu    sync.Mutex
	path  string
	state quotaState
}

func newAIQuota(path string) (*aiQuota, error) {
	if path == "" {
		return nil, errors.New("AUTODOCKER_TRIAL_QUOTA_FILE required")
	}
	q := &aiQuota{path: path, state: quotaState{Visitors: map[string]int{}}}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &q.state); err != nil {
			return nil, fmt.Errorf("invalid trial quota file: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if q.state.Visitors == nil {
		q.state.Visitors = map[string]int{}
	}
	if q.state.SignupIPs == nil {
		q.state.SignupIPs = map[string]int{}
	}
	if q.state.Salt == "" {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		q.state.Salt = hex.EncodeToString(secret)
	}
	return q, nil
}

func (q *aiQuota) resetDayLocked() {
	day := time.Now().UTC().Format("2006-01-02")
	if q.state.Day != day {
		q.state = quotaState{Day: day, Salt: q.state.Salt, Visitors: map[string]int{}, SignupIPs: map[string]int{}}
	}
}

func (q *aiQuota) hashedKey(value string) string {
	secret, _ := hex.DecodeString(q.state.Salt)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (q *aiQuota) remaining(subject string, limit int, secondary string, secondaryLimit int) (int, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.resetDayLocked()
	used := q.state.Visitors[q.hashedKey(subject)]
	left := max(0, limit-used)
	if secondary != "" {
		left = min(left, max(0, secondaryLimit-q.state.Visitors[q.hashedKey(secondary)]))
	}
	return left, max(0, 40-q.state.Total)
}

func (q *aiQuota) reserve(subject string, limit int, secondary string, secondaryLimit int) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.resetDayLocked()
	visitorKey := q.hashedKey(subject)
	if q.state.Total >= 40 || q.state.Visitors[visitorKey] >= limit {
		return false, nil
	}
	secondaryKey := ""
	if secondary != "" {
		secondaryKey = q.hashedKey(secondary)
		if q.state.Visitors[secondaryKey] >= secondaryLimit {
			return false, nil
		}
	}
	q.state.Total++
	q.state.Visitors[visitorKey]++
	if secondaryKey != "" {
		q.state.Visitors[secondaryKey]++
	}
	if err := q.saveLocked(); err != nil {
		q.state.Total--
		q.state.Visitors[visitorKey]--
		if secondaryKey != "" {
			q.state.Visitors[secondaryKey]--
		}
		return false, err
	}
	return true, nil
}

func (q *aiQuota) exhaustedReason(subject string, limit int, secondary string, secondaryLimit int) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.resetDayLocked()
	if q.state.Total >= 40 {
		return "global"
	}
	if secondary != "" && q.state.Visitors[q.hashedKey(secondary)] >= secondaryLimit {
		return "connection"
	}
	if q.state.Visitors[q.hashedKey(subject)] >= limit {
		return "visitor"
	}
	return "unavailable"
}

func (q *aiQuota) refund(subject string, secondary string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.resetDayLocked()
	visitorKey := q.hashedKey(subject)
	if q.state.Total == 0 || q.state.Visitors[visitorKey] == 0 {
		return nil
	}
	q.state.Total--
	q.state.Visitors[visitorKey]--
	if secondary != "" {
		secondaryKey := q.hashedKey(secondary)
		if q.state.Visitors[secondaryKey] > 0 {
			q.state.Visitors[secondaryKey]--
		}
	}
	return q.saveLocked()
}

func (q *aiQuota) reserveSignup(ip string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.resetDayLocked()
	key := q.hashedKey("signup:" + ip)
	if q.state.Signups >= 20 || q.state.SignupIPs[key] >= 2 {
		return false, nil
	}
	q.state.Signups++
	q.state.SignupIPs[key]++
	if err := q.saveLocked(); err != nil {
		q.state.Signups--
		q.state.SignupIPs[key]--
		return false, err
	}
	return true, nil
}

func (q *aiQuota) saveLocked() error {
	data, err := json.Marshal(q.state)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(q.path), "quota-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), q.path); err != nil {
		return err
	}
	return nil
}

func jobResult(result *services.MagicAIResult) gin.H {
	return gin.H{"status": "ok", "payload": result.Payload, "summary": result.Summary, "reasons": result.Reasons, "questions": result.Questions, "attempts": result.Attempts, "preset": result.Source == "preset", "intent": result.Intent, "confidence": result.Confidence, "source": result.Source, "model": result.Model, "warnings": result.Warnings, "design_checks": result.DesignChecks, "manual_steps": result.ManualSteps}
}

func jobResponse(view services.InfrastructureJobView) gin.H {
	response := gin.H{"status": "ok", "job": view}
	if view.Result != nil {
		response["result"] = jobResult(view.Result)
	}
	return response
}

func currentAccount(c *gin.Context, accounts *accountStore) string {
	name, _ := sessions.Default(c).Get("account").(string)
	if name != "" && accounts.exists(name) {
		return name
	}
	return ""
}

func registerAccountRoutes(api *gin.RouterGroup, accounts *accountStore, quota *aiQuota) {
	api.GET("/me", func(c *gin.Context) {
		account := currentAccount(c, accounts)
		owner := trialOwner(c, true, accounts)
		if owner == "" {
			c.JSON(503, gin.H{"status": "error", "error": "Session indisponible"})
			return
		}
		subject, limit := "guest-cookie:"+owner, 2
		secondary, secondaryLimit := "guest:"+visitorIP(c.Request), 6
		if account != "" {
			subject, limit = "account:"+account, 5
			secondary, secondaryLimit = "", 0
		}
		remaining, global := quota.remaining(subject, limit, secondary, secondaryLimit)
		c.JSON(200, gin.H{"status": "ok", "account": account, "authenticated": account != "", "remaining": remaining, "daily_limit": limit, "global_available": global > 0})
	})
	api.POST("/signup", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
		var request struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if c.ShouldBindJSON(&request) != nil {
			c.JSON(400, gin.H{"status": "error", "error": "Formulaire invalide"})
			return
		}
		name, err := normalizeAccountName(request.Username)
		if err != nil {
			c.JSON(400, gin.H{"status": "error", "error": err.Error()})
			return
		}
		if len(request.Password) < 10 || len(request.Password) > 72 {
			c.JSON(400, gin.H{"status": "error", "error": "Mot de passe : 10 à 72 caractères."})
			return
		}
		if accounts.exists(name) {
			c.JSON(409, gin.H{"status": "error", "error": "Ce nom est déjà utilisé."})
			return
		}
		allowed, err := quota.reserveSignup(visitorIP(c.Request))
		if err != nil {
			c.JSON(503, gin.H{"status": "error", "error": "Inscription momentanément indisponible."})
			return
		}
		if !allowed {
			c.JSON(429, gin.H{"status": "error", "error": "Trop d’inscriptions aujourd’hui depuis cette connexion. Réessayez demain."})
			return
		}
		account, err := accounts.create(name, request.Password)
		if err != nil {
			status := 503
			if errors.Is(err, errAccountExists) {
				status = 409
			}
			if errors.Is(err, errInvalidUsername) || errors.Is(err, errWeakPassword) {
				status = 400
			}
			c.JSON(status, gin.H{"status": "error", "error": err.Error()})
			return
		}
		session := sessions.Default(c)
		session.Set("account", account)
		if session.Save() != nil {
			c.JSON(503, gin.H{"status": "error", "error": "Compte créé. Connectez-vous pour continuer."})
			return
		}
		c.JSON(201, gin.H{"status": "ok", "account": account, "remaining": 5})
	})
	api.POST("/login", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
		var request struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if c.ShouldBindJSON(&request) != nil {
			c.JSON(400, gin.H{"status": "error", "error": "Formulaire invalide"})
			return
		}
		account, ok := accounts.authenticate(request.Username, request.Password)
		if !ok {
			c.JSON(401, gin.H{"status": "error", "error": "Identifiant ou mot de passe incorrect."})
			return
		}
		session := sessions.Default(c)
		session.Set("account", account)
		if session.Save() != nil {
			c.JSON(503, gin.H{"status": "error", "error": "Connexion indisponible."})
			return
		}
		remaining, _ := quota.remaining("account:"+account, 5, "", 0)
		c.JSON(200, gin.H{"status": "ok", "account": account, "remaining": remaining})
	})
	api.POST("/logout", func(c *gin.Context) {
		session := sessions.Default(c)
		session.Delete("account")
		if session.Save() != nil {
			c.JSON(503, gin.H{"status": "error", "error": "Déconnexion indisponible."})
			return
		}
		c.JSON(200, gin.H{"status": "ok"})
	})
}

func trialOwner(c *gin.Context, create bool, accounts *accountStore) string {
	session := sessions.Default(c)
	if account := currentAccount(c, accounts); account != "" {
		return "trial-account:" + account
	}
	id, _ := session.Get("trial_id").(string)
	if id == "" && create {
		bytes := make([]byte, 24)
		if _, err := rand.Read(bytes); err != nil {
			return ""
		}
		id = hex.EncodeToString(bytes)
		session.Set("trial_id", id)
		if session.Save() != nil {
			return ""
		}
	}
	if id == "" {
		return ""
	}
	return "trial:" + id
}

// A signed browser session may still own a guest job after the user signs in.
// Account jobs never become visible to a different account or after logout.
func trialJobOwners(c *gin.Context, accounts *accountStore) []string {
	var owners []string
	if account := currentAccount(c, accounts); account != "" {
		owners = append(owners, "trial-account:"+account)
	}
	if id, _ := sessions.Default(c).Get("trial_id").(string); id != "" {
		owners = append(owners, "trial:"+id)
	}
	return owners
}

func registerAIRoutes(api *gin.RouterGroup, quota *aiQuota, accounts *accountStore) *services.InfrastructureJobQueue {
	queue := services.NewInfrastructureJobQueue(1, 1, services.GenerateInfrastructureConfig)
	registerAIRoutesWithQueue(api, quota, accounts, queue)
	return queue
}

func registerAIRoutesWithQueue(api *gin.RouterGroup, quota *aiQuota, accounts *accountStore, queue *services.InfrastructureJobQueue) {
	api.POST("/ai-jobs", func(c *gin.Context) {
		var request struct {
			Prompt string `json:"prompt"`
		}
		if c.ShouldBindJSON(&request) != nil {
			c.JSON(400, gin.H{"status": "error", "error": "Décrivez votre projet en 2 000 caractères maximum."})
			return
		}
		prompt := strings.TrimSpace(request.Prompt)
		if prompt == "" || utf8.RuneCountInString(prompt) > 2000 {
			c.JSON(400, gin.H{"status": "error", "error": "Décrivez votre projet en 2 000 caractères maximum."})
			return
		}
		owner := trialOwner(c, true, accounts)
		if owner == "" {
			c.JSON(503, gin.H{"status": "error", "error": "Session indisponible"})
			return
		}
		if question := services.InfrastructurePreflightQuestion(prompt); question != "" {
			c.JSON(200, jobResult(&services.MagicAIResult{
				Source: "autodocker", Intent: "clarification", Confidence: "needs_clarification", Questions: []string{question},
			}))
			return
		}
		subject, limit := "guest-cookie:"+owner, 2
		secondary, secondaryLimit := "guest:"+visitorIP(c.Request), 6
		if account := currentAccount(c, accounts); account != "" {
			subject, limit = "account:"+account, 5
			secondary, secondaryLimit = "", 0
		}
		allowed, reservation, err := quota.reserveJob(subject, limit, secondary, secondaryLimit)
		if err != nil {
			c.JSON(503, gin.H{"status": "error", "error": "Quota IA indisponible"})
			return
		}
		if !allowed {
			c.Header("Retry-After", "86400")
			switch quota.exhaustedReason(subject, limit, secondary, secondaryLimit) {
			case "visitor":
				if strings.HasPrefix(subject, "guest-cookie:") {
					c.JSON(429, gin.H{"status": "error", "code": "guest_quota_exhausted", "error": "Vos deux demandes IA gratuites du jour sont utilisées."})
					return
				}
				c.JSON(429, gin.H{"status": "error", "code": "account_quota_exhausted", "error": "Vos cinq demandes IA du jour sont utilisées. Réessayez demain."})
			case "connection":
				c.JSON(429, gin.H{"status": "error", "code": "connection_quota_exhausted", "error": "La limite IA de cette connexion est atteinte pour aujourd’hui. Réessayez demain."})
			default:
				c.JSON(429, gin.H{"status": "error", "code": "trial_quota_exhausted", "error": "L’essai IA est complet pour aujourd’hui. Réessayez demain."})
			}
			return
		}
		view, created, err := queue.SubmitWithStatusContextOnAbandon(context.Background(), owner, prompt, func() {
			if err := reservation.Refund(); err != nil {
				fmt.Println("[TRIAL] Le quota d'une demande abandonnee n'a pas pu etre rembourse")
			}
		})
		if err != nil {
			if refundErr := reservation.Refund(); refundErr != nil {
				c.JSON(503, gin.H{"status": "error", "error": "Demande indisponible. Le quota pourra être corrigé à la prochaine réinitialisation."})
				return
			}
			c.JSON(503, gin.H{"status": "error", "error": err.Error()})
			return
		}
		if !created {
			if err := reservation.Refund(); err != nil {
				c.JSON(503, gin.H{"status": "error", "error": "Demande momentanément indisponible."})
				return
			}
		}
		c.JSON(202, jobResponse(view))
	})
	api.GET("/ai-jobs/:id", func(c *gin.Context) {
		for _, owner := range trialJobOwners(c, accounts) {
			if view, err := queue.Get(owner, c.Param("id")); err == nil {
				c.JSON(200, jobResponse(view))
				return
			}
		}
		c.JSON(404, gin.H{"status": "error", "error": "Demande introuvable"})
	})
	api.POST("/ai-jobs/:id/cancel", func(c *gin.Context) {
		for _, owner := range trialJobOwners(c, accounts) {
			if view, err := queue.Cancel(owner, c.Param("id")); err == nil {
				c.JSON(200, jobResponse(view))
				return
			}
		}
		c.JSON(404, gin.H{"status": "error", "error": "Demande introuvable"})
	})
}
