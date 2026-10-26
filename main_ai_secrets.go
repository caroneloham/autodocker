package main

import (
	"fmt"
	"os"
	"strings"

	"autodocker-go/services"
)

// The private file can configure AI, never the listener, authentication or root.
func loadAISecretsFile(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 512<<10 {
		return fmt.Errorf("fichier de clés invalide")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("fichier de clés illisible")
	}
	allowed := map[string]bool{}
	for _, key := range services.CloudEnvironmentNames() {
		allowed[key] = true
	}
	for _, key := range []string{
		"GEMINI_API_KEY", "GEMINI_API_KEYS", "GEMINI_API_KEY_PROD", "GEMINI_MODEL",
		"GROQ_API_KEY", "GROQ_API_KEYS", "GROQ_BASE_URL", "GROQ_MODEL",
		"NVIDIA_API_KEY", "NVIDIA_API_KEYS", "NVIDIA_BASE_URL", "NVIDIA_MODEL",
		"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_API_TOKENS", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_BASE_URL", "CLOUDFLARE_MODEL",
	} {
		allowed[key] = true
	}
	loaded := 0
	parsed := map[string]string{}
	for _, raw := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if ok && allowed[name] && value != "" {
			parsed[name] = value
		}
	}
	if len(parsed) == 0 {
		return fmt.Errorf("aucune configuration IA reconnue")
	}
	// A supplied private key replaces stale aliases from the installation.
	groups := [][]string{
		{"GEMINI_API_KEY", "GEMINI_API_KEYS", "GEMINI_API_KEY_PROD"},
		{"GROQ_API_KEY", "GROQ_API_KEYS"}, {"NVIDIA_API_KEY", "NVIDIA_API_KEYS"},
		{"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_API_TOKENS"},
	}
	for _, key := range services.CloudEnvironmentNames() {
		if strings.HasSuffix(key, "_API_KEY") && key != "GEMINI_API_KEY" {
			groups = append(groups, []string{strings.TrimSuffix(key, "_KEY") + "_KEYS", key})
		}
	}
	for _, group := range groups {
		replace := false
		for _, key := range group {
			if parsed[key] != "" {
				replace = true
			}
		}
		if replace {
			for _, key := range group {
				if parsed[key] == "" {
					os.Unsetenv(key)
				}
			}
		}
	}
	for name, value := range parsed {
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("configuration IA invalide")
		}
		loaded++
	}
	fmt.Printf("[ENV] Configuration API privee chargee (%d variables ; valeurs masquees)\n", loaded)
	return nil
}
