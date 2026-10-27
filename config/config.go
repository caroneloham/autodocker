package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

var (
	RootDir        string
	ProjectsDir    string
	AuditsDir      string
	BackupsDir     string
	SecretsDir     string
	AgentsDir      string
	TemplatesDir   string
	UIDir          string
	DBPath         string
	Port           string
	BindHost       string
	Debug          bool
	NoSSL          bool
	DemoMode       bool
	AuthBypass     bool
	AuthBypassUser string
	AuthBypassRole string
	GeneratorOnly  bool
)

// Browsers share cookies between localhost ports. Separate local installations
// must not overwrite each other's owner-setup grants or authenticated sessions.
func SessionCookieName() string {
	if !LocalBootstrapMode() {
		return "session"
	}
	port, err := strconv.Atoi(Port)
	if err == nil && port > 0 && port <= 65535 {
		return "autodocker_local_" + strconv.Itoa(port)
	}
	return "autodocker_local"
}

func Init() {
	exe, _ := os.Executable()
	RootDir = filepath.Dir(exe)

	// Allow override via env
	if v := os.Getenv("AUTODOCKER_ROOT"); v != "" {
		RootDir = v
	}
	// Keep compatibility with the original layout where autodocker-go is a
	// subdirectory of the main AUTODOCKER folder, while still allowing the
	// autodocker-go folder to run by itself after being copied elsewhere.
	parent := filepath.Dir(RootDir)
	if isExistingAutoDockerRoot(parent) {
		RootDir = parent
	} else if _, err := os.Stat(filepath.Join(RootDir, "lab-ui")); err != nil {
		if _, err2 := os.Stat(filepath.Join(parent, "lab-ui")); err2 == nil {
			RootDir = parent
		}
	}

	ProjectsDir = filepath.Join(RootDir, "projects")
	AuditsDir = filepath.Join(RootDir, "audits")
	BackupsDir = filepath.Join(RootDir, "backups")
	SecretsDir = filepath.Join(RootDir, "secrets")
	AgentsDir = filepath.Join(RootDir, "agents")
	TemplatesDir = filepath.Join(RootDir, "templates_store")
	UIDir = filepath.Join(RootDir, "lab-ui")
	DBPath = filepath.Join(RootDir, "auth.db")

	Port = getEnv("PORT", "8000")
	BindHost = getEnv("BIND_HOST", "127.0.0.1")
	Debug = os.Getenv("FLASK_DEBUG") == "true" || os.Getenv("GIN_MODE") == "debug"
	// DEMO=1 : mode demo publique (HTTPS, pas d'auth)
	DemoMode = os.Getenv("DEMO") == "1"
	// AUTH_BYPASS=1 : mode atelier local, sans login mais avec droits reels.
	AuthBypass = getEnv("AUTH_BYPASS", "0") == "1"
	AuthBypassUser = getEnv("AUTH_BYPASS_USER", "elohgam")
	AuthBypassRole = getEnv("AUTH_BYPASS_ROLE", "super_admin")
	GeneratorOnly = getEnv("AUTODOCKER_GENERATOR_ONLY", "1") != "0"
	if AuthBypass || DemoMode {
		// An unauthenticated workshop/demo must never expose host Docker control
		// through a wildcard or LAN listener.
		BindHost = "127.0.0.1"
	}
	// HTTP is available only for the explicitly bundled, loopback-only local
	// installation. Hosted installations always keep TLS and Secure cookies.
	NoSSL = LocalBootstrapMode() && os.Getenv("AUTODOCKER_LOCAL_HTTP") == "1"
	if os.Getenv("NO_SSL") == "1" && !NoSSL {
		fmt.Println("[SECURITE] NO_SSL=1 ignore : HTTPS est obligatoire.")
	}
	switch AuthBypassRole {
	case "user", "admin", "super_admin":
	default:
		AuthBypassRole = "super_admin"
	}

	// Ensure directories exist
	for _, d := range []string{ProjectsDir, AuditsDir, BackupsDir, SecretsDir, AgentsDir, TemplatesDir} {
		os.MkdirAll(d, 0755)
	}
}

func LocalBootstrapMode() bool {
	ip := net.ParseIP(BindHost)
	return os.Getenv("AUTODOCKER_LOCAL_BOOTSTRAP") == "1" && !AuthBypass && !DemoMode && ip != nil && ip.IsLoopback()
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func isExistingAutoDockerRoot(path string) bool {
	if _, err := os.Stat(filepath.Join(path, "lab-ui")); err != nil {
		return false
	}
	for _, marker := range []string{"auth.db", "projects", "templates_store", "secrets"} {
		if _, err := os.Stat(filepath.Join(path, marker)); err == nil {
			return true
		}
	}
	return false
}
