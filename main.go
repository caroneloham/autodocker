package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"html/template"
	"log"
	"math/big"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/handlers"
	"autodocker-go/middleware"
	"autodocker-go/services"
	"autodocker-go/workers"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

//go:embed templates/*.html lab-ui/*
var embeddedFiles embed.FS

func waitAndExit() {
	fmt.Println()
	fmt.Println("Appuyez sur ENTREE pour fermer...")
	fmt.Scanln()
}

func loadLocalEnvFile(searchRoots ...string) {
	runtimeKeys := explicitRuntimeEnvKeys()
	candidates := localEnvCandidates(searchRoots...)
	loadedFiles := []string{}
	totalLoaded := 0

	for _, envPath := range candidates {
		loaded, ok := loadEnvFileWithRuntimeKeys(envPath, runtimeKeys)
		if !ok {
			continue
		}
		loadedFiles = append(loadedFiles, fmt.Sprintf("%s (%d)", filepath.Base(envPath), loaded))
		totalLoaded += loaded
	}

	if len(loadedFiles) == 0 {
		return
	}

	normalizeAIEnvAliases()

	fmt.Printf("[ENV] Configuration locale chargee depuis %s (%d variable(s))\n", strings.Join(loadedFiles, ", "), totalLoaded)
	logAIEnvStatus()
}

func localEnvCandidates(searchRoots ...string) []string {
	dirs := []string{}
	exe, err := os.Executable()
	if err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	dirs = append(dirs, searchRoots...)
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, cwd)
	}
	if root := strings.TrimSpace(os.Getenv("AUTODOCKER_ROOT")); root != "" {
		dirs = append(dirs, root)
	}

	seenDirs := map[string]bool{}
	seenFiles := map[string]bool{}
	candidates := []string{}

	addFile := func(path string) {
		abs, err := filepath.Abs(path)
		if err == nil {
			path = abs
		}
		path = filepath.Clean(path)
		key := strings.ToLower(path)
		if !seenFiles[key] {
			seenFiles[key] = true
			candidates = append(candidates, path)
		}
	}

	addDir := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		abs, err := filepath.Abs(dir)
		if err == nil {
			dir = abs
		}
		dir = filepath.Clean(dir)
		key := strings.ToLower(dir)
		if seenDirs[key] {
			return
		}
		seenDirs[key] = true
		addFile(filepath.Join(dir, ".env"))
		addFile(filepath.Join(dir, "autodocker.env"))
	}

	for _, dir := range dirs {
		addDir(dir)
		parent := filepath.Dir(dir)
		if parent != "" && parent != dir {
			if _, err := os.Stat(filepath.Join(parent, "lab-ui")); err == nil {
				addDir(parent)
			}
		}
	}

	return candidates
}

func loadEnvFile(envPath string) (int, bool) {
	return loadEnvFileWithRuntimeKeys(envPath, explicitRuntimeEnvKeys())
}

// Preserve launch-time routing and project-root choices. Capture these once
// before reading any files so later files can still override earlier defaults.
func explicitRuntimeEnvKeys() map[string]bool {
	keys := make(map[string]bool, 3)
	for _, key := range []string{"PORT", "BIND_HOST", "AUTODOCKER_ROOT", "AUTODOCKER_AI_KEYS_FILE", "AUTODOCKER_AI_PROVIDER", "AUTODOCKER_AI_FALLBACKS", "AUTODOCKER_ACCOUNT_OWNER", "AUTH_BYPASS", "DEMO", "AUTODOCKER_GENERATOR_ONLY", "AUTODOCKER_LOCAL_BOOTSTRAP", "AUTODOCKER_LOCAL_HTTP", "AUTODOCKER_BOOTSTRAP_TOKEN", "AUTODOCKER_OPEN_BROWSER"} {
		_, keys[key] = os.LookupEnv(key)
	}
	return keys
}

func loadEnvFileWithRuntimeKeys(envPath string, runtimeKeys map[string]bool) (int, bool) {
	data, err := os.ReadFile(envPath)
	if err != nil {
		return 0, false
	}

	loaded := 0
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimPrefix(rawLine, "\ufeff"))
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if key == "" || val == "" {
			continue
		}
		if runtimeKeys[key] {
			continue
		}
		os.Setenv(key, val)
		loaded++
	}

	return loaded, true
}

func normalizeAIEnvAliases() {
	setEnvFromAliases("GEMINI_API_KEYS", "GEMINI_API_KEY")
	setEnvFromAliases("OPENROUTER_API_KEYS", "OPENROUTER_API_KEY", "OPEN_ROUTER_API_KEYS", "OPEN_ROUTER_API_KEY")
}

func setEnvFromAliases(target string, aliases ...string) {
	if strings.TrimSpace(os.Getenv(target)) != "" {
		return
	}
	for _, alias := range aliases {
		if val := strings.TrimSpace(os.Getenv(alias)); val != "" {
			os.Setenv(target, val)
			return
		}
	}
}

func logAIEnvStatus() {
	if count := countCSVEnv("GEMINI_API_KEYS"); count > 0 {
		fmt.Printf("[ENV] Gemini configure via fichier local (%d cle(s))\n", count)
	}
	if count := countCSVEnv("OPENROUTER_API_KEYS"); count > 0 {
		fmt.Printf("[ENV] OpenRouter configure via fichier local (%d cle(s))\n", count)
	}
}

func countCSVEnv(key string) int {
	count := 0
	for _, val := range strings.Split(os.Getenv(key), ",") {
		if strings.TrimSpace(val) != "" {
			count++
		}
	}
	return count
}

func openBrowserIfRequested(url string) {
	requested, specified := os.LookupEnv("AUTODOCKER_OPEN_BROWSER")
	if !specified {
		requested = os.Getenv("OPEN_BROWSER")
	}
	if requested != "1" {
		return
	}

	go func() {
		time.Sleep(3 * time.Second)
		_ = openBrowserURL(url)
	}()
}

func main() {
	// Catch panics so the window stays open on double-click
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("\n[ERREUR FATALE] %v\n", r)
			waitAndExit()
		}
	}()

	fmt.Println(">>> CHARGEMENT AUTODOCKER (Go)...")

	startDir, _ := os.Getwd()

	// Set working directory to executable location for double-click support
	if exe, err := os.Executable(); err == nil {
		os.Chdir(filepath.Dir(exe))
	}

	loadLocalEnvFile(startDir)
	if err := loadAISecretsFile(os.Getenv("AUTODOCKER_AI_KEYS_FILE")); err != nil {
		log.Fatal("[ENV] Le fichier prive des API IA est illisible ou invalide")
	}
	normalizeAIEnvAliases()

	// Init config
	config.Init()
	if config.LocalBootstrapMode() {
		token := os.Getenv("AUTODOCKER_BOOTSTRAP_TOKEN")
		decoded, err := hex.DecodeString(token)
		if err != nil || len(decoded) != 32 || len(token) != 64 || strings.ToLower(token) != token {
			log.Fatal("[AUTH] Jeton d'amorcage local absent ou invalide")
		}
	}

	// Init database
	db.Init()
	if err := db.EnsureAccountColumns(db.DB); err != nil {
		panic("Migration des comptes impossible")
	}

	// Init Docker
	services.InitDocker()

	// Init AI service
	services.InitAI()

	// Init multi-AI providers from environment
	// Load multi-AI keys (comma-separated for multiple keys)
	if k := os.Getenv("GROQ_API_KEY"); k != "" {
		services.SetGroqKeys(strings.Split(k, ","))
	}
	if k := os.Getenv("OPENROUTER_API_KEYS"); k != "" {
		services.SetOpenRouterKeys(strings.Split(k, ","))
	} else if k := os.Getenv("OPENROUTER_API_KEY"); k != "" {
		services.SetOpenRouterKey(k)
	}
	if k := os.Getenv("MISTRAL_API_KEY"); k != "" {
		services.SetMistralKeys(strings.Split(k, ","))
	}
	if services.GroqAvailable() {
		fmt.Println("[AI] Groq configure (rotation de cles)")
	}
	if services.MistralAvailable() {
		fmt.Println("[AI] Mistral configure")
	}
	if services.OpenRouterAvailable() {
		fmt.Printf("[AI] OpenRouter configure: %d cle(s)\n", services.OpenRouterKeyCount())
	}

	// Gin mode
	if !config.Debug {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(gin.Recovery())
	if config.Debug {
		// The one-time bootstrap URL carries a secret: never log /setup requests.
		r.Use(gin.LoggerWithConfig(gin.LoggerConfig{SkipPaths: []string{"/setup"}}))
	}

	loadHTMLTemplates(r)

	// Session store — use raw 32-byte keys for gorilla/securecookie
	authKey, encKey := loadOrCreateSessionKeys()
	store := cookie.NewStore(authKey, encKey)
	store.Options(sessions.Options{
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400 * 7, // 7 days
		SameSite: http.SameSiteLaxMode,
		Secure:   !config.NoSSL, // match actual protocol: Secure=true for HTTPS, false for HTTP
	})
	r.Use(sessions.Sessions(config.SessionCookieName(), store))
	r.Use(middleware.LocalHostOriginProtection())

	// Session recovery: detect corrupted/invalid cookies early and clear them
	r.Use(middleware.SessionRecovery())
	r.Use(middleware.LiveAccountSession())

	if config.AuthBypass {
		r.Use(middleware.AuthBypassSession())
		fmt.Printf("[AUTH] Bypass local actif - login desactive, utilisateur=%s, role=%s\n", config.AuthBypassUser, config.AuthBypassRole)
	}

	// Make gin.Engine available in context (for API docs)
	r.Use(func(c *gin.Context) { c.Set("gin_engine", r); c.Next() })

	// Demo mode: inject fake session for all requests
	if config.DemoMode {
		r.Use(middleware.DemoSession())
		fmt.Println("[DEMO] Mode demonstration active - authentification desactivee")
	}

	// Middleware
	r.Use(middleware.SecurityHeaders())
	if !config.DemoMode {
		r.Use(middleware.CSRFProtection())
		// MFA obligatoire : un admin sans TOTP peut consulter mais pas modifier
		r.Use(middleware.TOTPSetupRequired())
	}

	// === ROUTES ===

	// Static files (lab-ui) with no-cache to ensure latest version.
	serveUIAsset(r, "/styles.css", "styles.css")
	serveUIAsset(r, "/workspace.css", "workspace.css")
	serveUIAsset(r, "/favicon.svg", "favicon.svg")
	serveUIAsset(r, "/app.js", "app.js")
	serveUIAsset(r, "/i18n.js", "i18n.js")
	serveUIAsset(r, "/accounts.js", "accounts.js")
	serveUIAsset(r, "/accounts.css", "accounts.css")
	serveUIAsset(r, "/favicon.ico", "favicon.ico")

	// Ensure admin user exists on first run
	handlers.EnsureAdminUser()

	// Main app (requires auth)
	authorized := r.Group("/")
	authorized.Use(middleware.LoginRequired())
	{
		// Serve main UI
		authorized.GET("/", handleIndex)
	}

	// API routes (auth required)
	api := r.Group("/api")
	api.Use(middleware.LoginRequired())

	// Register auth routes (login/logout are public, API routes need auth)
	handlers.RegisterAuthRoutes(r, nil, api)

	// Register admin routes
	handlers.RegisterAccountRoutes(r, api)
	if !config.GeneratorOnly {
		handlers.RegisterAdminRoutes(api)
	}

	if config.GeneratorOnly {
		handlers.RegisterGeneratorRoutes(api)
	} else {
		// Register builder routes (deploy, preview, catalogue)
		handlers.RegisterBuilderRoutes(api)

		// Register dashboard routes (refresh, delete, bulk)
		handlers.RegisterDashboardRoutes(api)

		// Register container routes (logs, files, stats, actions)
		handlers.RegisterContainerRoutes(api)

		// Register tools routes (vuln-scan, audit, hardening, network-map, images)
		handlers.RegisterToolsRoutes(api)

		// Register firewall routes (iptables management)
		handlers.RegisterFirewallRoutes(api)

		// Register flow map routes (D3 network topology)
		handlers.RegisterFlowMapRoutes(api)

		// Register supervision routes (monitoring, alerting, agents)
		handlers.RegisterSupervisionRoutes(api)

		// Register health routes (health dashboard)
		handlers.RegisterHealthRoutes(api)

		// Register metrics routes (metrics history, export, retention)
		handlers.RegisterMetricsRoutes(api)

		// Register logs routes (centralized log search, timeline)
		handlers.RegisterLogsRoutes(api)

		// Register backup routes (backups, schedules, config backups)
		handlers.RegisterBackupRoutes(api)

		// Register scaling routes (auto-scaling rules)
		handlers.RegisterScalingRoutes(api)

		// Register multi-host Docker management routes
		handlers.RegisterMultihostRoutes(api)

		// Register template management routes
		handlers.RegisterTemplateRoutes(api)

		// Register alert channel routes
		handlers.RegisterAlertRoutes(api)

		// Register container update/rollback routes
		handlers.RegisterUpdateRoutes(api)

		// Register WebSocket terminal routes
		handlers.RegisterTerminalRoutes(r)

		// Register API documentation routes
		handlers.RegisterAPIDocsRoutes(r)

		// Register new v2.1 features
		handlers.RegisterSecretsRoutes(api)
		handlers.RegisterScheduledTasksRoutes(api)
		handlers.RegisterRegistryRoutes(api)
		handlers.RegisterAnomalyRoutes(api)
		handlers.RegisterTracingRoutes(api)
		handlers.RegisterCopilotRoutes(api)

		// Register reverse proxy routes
		handlers.RegisterProxyRoutes(r)
	}

	// CA download endpoint (no auth - needed for other machines to trust us)
	r.GET("/ca.pem", func(c *gin.Context) {
		caFile := filepath.Join(config.SecretsDir, "ca.pem")
		if _, err := os.Stat(caFile); err != nil {
			c.String(404, "CA not found")
			return
		}
		c.Header("Content-Disposition", "attachment; filename=AutoDocker-CA.pem")
		c.File(caFile)
	})

	// Start background workers
	if !config.GeneratorOnly {
		workers.StartBackgroundWorkers(db.DB, services.DockerClient)
	}

	// Start server
	addr := fmt.Sprintf("%s:%s", config.BindHost, config.Port)
	localIP := "127.0.0.1"
	if !config.LocalBootstrapMode() {
		localIP = getLocalIP()
	}
	dockerStatus := boolStr(services.DockerClient != nil, "Connecte", "Non connecte")
	iaStatus := boolStr(services.AIAvailable(), "Active", "Desactivee")

	proto := "https"
	if config.NoSSL {
		proto = "http"
	}
	mainURL := fmt.Sprintf("%s://%s:%s", proto, localIP, config.Port)
	localHost := "localhost"
	if config.LocalBootstrapMode() {
		localHost = config.BindHost
	}
	localURL := fmt.Sprintf("%s://%s:%s", proto, localHost, config.Port)

	w := 56
	line := func(content string) string {
		pad := w - len([]rune(content))
		if pad < 0 {
			pad = 0
		}
		return fmt.Sprintf("║ %s%s ║", content, strings.Repeat(" ", pad))
	}
	topBot := strings.Repeat("═", w+2)
	fmt.Println()
	fmt.Printf("╔%s╗\n", topBot)
	serverTitle := "       AUTODOCKER SERVER (Go)"
	if config.AuthBypass {
		serverTitle = " AUTODOCKER SERVER (Go) - AUTH BYPASS"
	} else if config.DemoMode {
		serverTitle = "    AUTODOCKER SERVER (Go) - DEMO"
	}
	fmt.Println(line(serverTitle))
	fmt.Printf("╠%s╣\n", topBot)
	fmt.Println(line(""))
	fmt.Println(line(fmt.Sprintf("%-7s: %s", strings.ToUpper(proto), mainURL)))
	fmt.Println(line(fmt.Sprintf("Local  : %s", localURL)))
	fmt.Println(line(""))
	fmt.Println(line(fmt.Sprintf("Docker : %s  |  IA : %s", dockerStatus, iaStatus)))
	if config.AuthBypass {
		fmt.Println(line(fmt.Sprintf("Auth   : Bypass local (%s/%s)", config.AuthBypassUser, config.AuthBypassRole)))
	} else if config.DemoMode {
		fmt.Println(line("Auth   : Desactivee (mode demo)"))
	}
	fmt.Println(line(""))
	fmt.Printf("╚%s╝\n", topBot)
	fmt.Println()

	browserURL := localURL
	if config.LocalBootstrapMode() {
		var userCount int
		if err := db.DB.QueryRow("SELECT COUNT(*) FROM users").Scan(&userCount); err == nil && userCount == 0 {
			browserURL += "/setup?token=" + os.Getenv("AUTODOCKER_BOOTSTRAP_TOKEN")
		}
	}
	openBrowserIfRequested(browserURL)

	if config.NoSSL {
		// HTTP mode
		if err := r.Run(addr); err != nil {
			fmt.Printf("\n[ERREUR] Impossible de demarrer le serveur HTTP: %v\n", err)
			waitAndExit()
		}
	} else {
		// HTTPS mode
		certFile, keyFile := ensureTLSCert()
		srv := &http.Server{
			Addr:    addr,
			Handler: r,
			TLSConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
			ErrorLog: log.New(&tlsErrorFilter{}, "", 0),
		}
		if err := srv.ListenAndServeTLS(certFile, keyFile); err != nil {
			fmt.Printf("\n[ERREUR] Impossible de demarrer le serveur HTTPS: %v\n", err)
			fmt.Println("Verifiez qu'aucun autre programme n'utilise le port " + config.Port)
			waitAndExit()
		}
	}
}

func boolStr(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

// tlsErrorFilter suppresses noisy TLS handshake errors from self-signed certs
type tlsErrorFilter struct{}

func (t *tlsErrorFilter) Write(p []byte) (n int, err error) {
	s := string(p)
	if strings.Contains(s, "TLS handshake error") {
		return len(p), nil
	} // suppress
	fmt.Print(s)
	return len(p), nil
}

func getLocalIP() string {
	// Best method: detect real outbound IP via UDP dial (no actual traffic)
	conn, err := net.DialTimeout("udp4", "8.8.8.8:53", 2*time.Second)
	if err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			return addr.IP.String()
		}
	}
	// Fallback: scan interfaces, skip APIPA and VirtualBox adapters
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			ip := ipnet.IP.String()
			if strings.HasPrefix(ip, "169.254.") || strings.HasPrefix(ip, "192.168.56.") {
				continue
			}
			return ip
		}
	}
	return "127.0.0.1"
}

// loadOrCreateSessionKeys returns 32-byte auth key and 32-byte encryption key
// for gorilla/securecookie. Stored as raw binary in secrets/session.key
func loadOrCreateSessionKeys() (authKey, encKey []byte) {
	keyPath := filepath.Join(config.SecretsDir, "session.key")
	data, err := os.ReadFile(keyPath)
	if err == nil && len(data) == 64 {
		fmt.Printf("[SESSION] Cles chargees depuis %s\n", keyPath)
		return data[:32], data[32:]
	}
	// Generate new keys — this invalidates ALL existing session cookies
	keys := make([]byte, 64)
	rand.Read(keys)
	if err := os.WriteFile(keyPath, keys, 0600); err != nil {
		fmt.Printf("[SESSION] ATTENTION: impossible d'ecrire %s: %v\n", keyPath, err)
		fmt.Println("[SESSION] Les sessions ne survivront pas a un redemarrage!")
	}
	fmt.Println("[SESSION] Nouvelles cles de session generees (les sessions precedentes sont invalidees)")
	return keys[:32], keys[32:]
}

func ensureTLSCert() (string, string) {
	certPath := filepath.Join(config.SecretsDir, "cert.pem")
	keyPath := filepath.Join(config.SecretsDir, "key.pem")
	caPath := filepath.Join(config.SecretsDir, "ca.pem")
	caKeyPath := filepath.Join(config.SecretsDir, "ca-key.pem")

	if _, err := os.Stat(certPath); err == nil {
		return certPath, keyPath
	}

	localIP := getLocalIP()
	fmt.Printf("[TLS] Generation du CA + certificat pour %s...\n", localIP)

	// --- Step 1: Create CA (Certificate Authority) ---
	caKey, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	caTemplate := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"AutoDocker CA"}, CommonName: "AutoDocker Root CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour), // 10 years
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	caCertDER, _ := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caCertDER)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})
	caKeyDER, _ := x509.MarshalECPrivateKey(caKey)
	caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER})
	os.WriteFile(caPath, caPEM, 0600)
	os.WriteFile(caKeyPath, caKeyPEM, 0600)

	// --- Step 2: Create Server Certificate signed by CA ---
	srvKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ipAddr := net.ParseIP(localIP)
	// Include common private network ranges as SANs for portability
	serverIPs := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	if ipAddr != nil {
		serverIPs = append(serverIPs, ipAddr)
	}
	srvTemplate := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{Organization: []string{"AutoDocker"}, CommonName: localIP},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(2 * 365 * 24 * time.Hour), // 2 years
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", localIP, "*.local"},
		IPAddresses:  serverIPs,
	}
	srvCertDER, _ := x509.CreateCertificate(rand.Reader, &srvTemplate, caCert, &srvKey.PublicKey, caKey)
	srvCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvCertDER})
	// Chain: server cert + CA cert
	fullChain := append(srvCertPEM, caPEM...)
	srvKeyDER, _ := x509.MarshalECPrivateKey(srvKey)
	srvKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: srvKeyDER})
	os.WriteFile(certPath, fullChain, 0600)
	os.WriteFile(keyPath, srvKeyPEM, 0600)

	// --- Step 3: Auto-install CA in system trust store ---
	installCA(caPath)

	fmt.Println("[TLS] Certificats generes avec succes")
	fmt.Printf("[TLS] CA root : %s (a installer sur les autres machines)\n", caPath)
	return certPath, keyPath
}

// installCA installs the CA cert into the system trust store (cross-platform)
func installCA(caPath string) {
	abs, _ := filepath.Abs(caPath)
	if runtime.GOOS == "windows" {
		fmt.Println("[TLS] Installation du CA dans le magasin Windows...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "certutil", "-addstore", "-user", "Root", abs)
		out, err := cmd.CombinedOutput()
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Println("[TLS] Installation auto du CA trop longue, demarrage du serveur sans attendre.")
			fmt.Println("[TLS] Si le navigateur affiche un avertissement, installez manuellement le CA :")
			fmt.Printf("       certutil -addstore -user Root \"%s\"\n", abs)
		} else if err != nil {
			fmt.Printf("[TLS] Installation auto impossible (droits admin requis ?): %v\n", err)
			fmt.Println("[TLS] Pour installer manuellement, executez en admin :")
			fmt.Printf("       certutil -addstore -user Root \"%s\"\n", abs)
		} else {
			outStr := string(out)
			if strings.Contains(outStr, "already in store") || strings.Contains(outStr, "existe deja") {
				fmt.Println("[TLS] CA deja installe dans le magasin Windows")
			} else {
				fmt.Println("[TLS] CA installe avec succes ! Les navigateurs feront confiance au certificat.")
			}
		}
	} else {
		// Linux: copy CA to system trust store
		fmt.Println("[TLS] Installation du CA dans le magasin Linux...")
		destDir := "/usr/local/share/ca-certificates"
		dest := filepath.Join(destDir, "autodocker-ca.crt")
		// Try to copy and update — may require root
		cpCmd := exec.Command("cp", abs, dest)
		if err := cpCmd.Run(); err == nil {
			updateCmd := exec.Command("update-ca-certificates")
			if out, err := updateCmd.CombinedOutput(); err != nil {
				fmt.Printf("[TLS] update-ca-certificates echoue: %v\n", err)
			} else {
				fmt.Printf("[TLS] CA installe: %s\n", strings.TrimSpace(string(out)))
			}
		} else {
			fmt.Println("[TLS] Installation auto impossible (droits root requis ?)")
			fmt.Println("[TLS] Pour installer manuellement :")
			fmt.Printf("       sudo cp %s %s && sudo update-ca-certificates\n", abs, dest)
		}
	}
}

func loadHTMLTemplates(r *gin.Engine) {
	templateDir := filepath.Join(filepath.Dir(os.Args[0]), "templates")
	if _, err := os.Stat(templateDir); err != nil {
		templateDir = "templates"
	}

	if matches, err := filepath.Glob(filepath.Join(templateDir, "*.html")); err == nil && len(matches) > 0 {
		r.LoadHTMLGlob(filepath.Join(templateDir, "*.html"))
		return
	}

	tmpl, err := template.ParseFS(embeddedFiles, "templates/*.html")
	if err != nil {
		panic(fmt.Errorf("charger les templates embarques: %w", err))
	}
	r.SetHTMLTemplate(tmpl)
}

func serveUIAsset(r *gin.Engine, route, name string) {
	r.GET(route, func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Header("Pragma", "no-cache")

		data, err := os.ReadFile(filepath.Join(config.UIDir, name))
		if err != nil {
			data, err = embeddedFiles.ReadFile("lab-ui/" + name)
		}
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}

		c.Data(http.StatusOK, contentTypeFor(name), data)
	})
}

func contentTypeFor(name string) string {
	switch filepath.Ext(name) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	}
	if contentType := mime.TypeByExtension(filepath.Ext(name)); contentType != "" {
		return contentType
	}
	return "application/octet-stream"
}

// handleIndex serves the main UI page
func handleIndex(c *gin.Context) {
	indexPath := filepath.Join(config.UIDir, "index.html")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		data, err = embeddedFiles.ReadFile("lab-ui/index.html")
		if err != nil {
			c.String(http.StatusOK, "UI manquante")
			return
		}
	}
	html := string(data)

	// Inject CSRF token
	csrfToken := middleware.GenerateCSRFToken(c)
	csrfMeta := fmt.Sprintf(`<meta name="csrf-token" content="%s">`, csrfToken)
	html = strings.Replace(html, "<head>", "<head>\n    "+csrfMeta, 1)

	// Inject session info (replace Jinja2 template variables)
	session := sessions.Default(c)
	user, _ := session.Get("user").(string)
	role, _ := session.Get("role").(string)
	if user == "" {
		user = "?"
	}
	if role == "" {
		role = "user"
	}
	roleDisplay := role
	if config.DemoMode {
		roleDisplay = "demo"
	}
	html = strings.ReplaceAll(html, "{{ session.user }}", user)
	html = strings.ReplaceAll(html, "{{ session.role }}", roleDisplay)

	// Handle the avatar initial
	initial := "?"
	if len(user) > 0 && user != "?" {
		r, _ := utf8.DecodeRuneInString(user)
		initial = strings.ToUpper(string(r))
	}
	html = strings.ReplaceAll(html, "{{ session.user[0]|upper if session.user else '?' }}", initial)

	showAdmin := middleware.IsAdminOrAbove(role) && !config.DemoMode
	showSuperAdmin := role == "super_admin" && !config.DemoMode
	html = renderTemplateIfBlocks(html, "{% if session.role in ['admin', 'super_admin'] %}", showAdmin)
	html = renderTemplateIfBlocks(html, "{% if session.role == 'super_admin' %}", showSuperAdmin)

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, html)
}

func renderTemplateIfBlocks(html, marker string, show bool) string {
	const ifToken = "{% if "
	const endToken = "{% endif %}"

	for {
		start := strings.Index(html, marker)
		if start == -1 {
			return html
		}

		searchFrom := start + len(marker)
		pos := searchFrom
		depth := 1
		endStart := -1
		endEnd := -1

		for depth > 0 {
			nextIfRel := strings.Index(html[pos:], ifToken)
			nextEndRel := strings.Index(html[pos:], endToken)
			if nextEndRel == -1 {
				return strings.Replace(html, marker, "", 1)
			}

			nextEnd := pos + nextEndRel
			if nextIfRel >= 0 {
				nextIf := pos + nextIfRel
				if nextIf < nextEnd {
					depth++
					pos = nextIf + len(ifToken)
					continue
				}
			}

			depth--
			if depth == 0 {
				endStart = nextEnd
				endEnd = nextEnd + len(endToken)
				break
			}
			pos = nextEnd + len(endToken)
		}

		if endStart == -1 {
			return html
		}
		if show {
			html = html[:start] + html[searchFrom:endStart] + html[endEnd:]
		} else {
			html = html[:start] + html[endEnd:]
		}
	}
}
