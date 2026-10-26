package services

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in only. Loads credentials in memory, makes sequential cloud
// requests, and writes only provider/status/timing metadata to the report.
func TestPOCLiveAPIEvaluation(t *testing.T) {
	path := os.Getenv("AUTODOCKER_API_EVAL_ENV")
	if path == "" {
		t.Skip("explicit live API evaluation only")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("private API evaluation file unreadable")
	}
	allowed := map[string]bool{}
	for _, name := range []string{"GROQ_API_KEY", "GROQ_API_KEYS", "GROQ_MODEL", "GROQ_BASE_URL", "NVIDIA_API_KEY", "NVIDIA_API_KEYS", "NVIDIA_MODEL", "NVIDIA_BASE_URL", "CLOUDFLARE_API_TOKEN", "CLOUDFLARE_API_TOKENS", "CLOUDFLARE_MODEL", "CLOUDFLARE_BASE_URL", "CLOUDFLARE_ACCOUNT_ID", "GEMINI_API_KEY", "GEMINI_API_KEYS", "GEMINI_API_KEY_PROD", "GEMINI_MODEL"} {
		allowed[name] = true
		t.Setenv(name, "")
	}
	for _, line := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "export ")), "=")
		if ok && allowed[name] {
			t.Setenv(name, strings.Trim(strings.TrimSpace(value), `"'`))
		}
	}
	var records []M
	passed := 0
	for _, name := range []string{"groq", "nvidia", "cloudflare", "gemini"} {
		if only := os.Getenv("AUTODOCKER_API_EVAL_PROVIDER"); only != "" && only != name {
			continue
		}
		if len(infrastructureCredentials(name)) == 0 {
			continue
		}
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		result, err := generateInfrastructureWithProvider(ctx, "Un site WordPress avec Grafana pour les métriques, sans Portainer ni Traefik.", infrastructureProviderForName(name))
		cancel()
		ok := err == nil && result != nil && result.Confidence == "validated"
		problem := ""
		if err != nil {
			problem = err.Error()
		}
		if ok {
			keys := infrastructureKeys(result)
			ok = keys["wordpress"] && keys["mariadb"] && keys["grafana"] && keys["prometheus"] && !keys["portainer"] && !keys["traefik"]
			if !ok {
				problem = "requirements missing or exclusion ignored"
			}
		}
		if ok {
			passed++
		}
		records = append(records, M{"provider": name, "passed": ok, "duration_ms": time.Since(start).Milliseconds(), "problem": problem})
		t.Logf("API %s passed=%t duration=%s", name, ok, time.Since(start).Round(time.Millisecond))
	}
	if report := os.Getenv("AUTODOCKER_API_EVAL_REPORT"); report != "" {
		out, _ := json.MarshalIndent(M{"providers": records, "passed": passed}, "", "  ")
		if err := os.WriteFile(report, out, 0600); err != nil {
			t.Error("could not write sanitized evaluation report")
		}
	}
	if passed == 0 {
		t.Fatal("no configured cloud provider passed validation")
	}
}
