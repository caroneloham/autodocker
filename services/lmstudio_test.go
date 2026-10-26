package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLMStudioRealQwenComposition(t *testing.T) {
	if os.Getenv("LM_STUDIO_INTEGRATION") != "1" {
		t.Skip("requires local Qwen in LM Studio")
	}
	t.Setenv("LM_STUDIO_MODEL", "autodocker-qwen-test")
	t.Setenv("LM_STUDIO_URL", "http://127.0.0.1:1234")
	result, err := GenerateMagicConfig("Une infrastructure de developpement avec Gitea, PostgreSQL et Redis. Ajoute uniquement ces services et leurs dependances.")
	if err != nil {
		t.Fatal(err)
	}
	services := result.Payload["services"].([]interface{})
	seen := map[string]bool{}
	for _, entry := range services {
		if item, ok := entry.(map[string]interface{}); ok {
			if key, ok := item["key"].(string); ok {
				seen[key] = true
			}
		}
	}
	for _, key := range []string{"gitea", "postgres", "redis"} {
		if !seen[key] {
			t.Errorf("Qwen omitted %s", key)
		}
	}
	if result.Source != "lmstudio" {
		t.Fatal("request did not use LM Studio")
	}
	result, err = GenerateMagicConfig("une infra wordpress avec de la supervision graphana")
	if err != nil {
		t.Fatal(err)
	}
	seen = map[string]bool{}
	for _, entry := range result.Payload["services"].([]interface{}) {
		if item, ok := entry.(map[string]interface{}); ok {
			if key, ok := item["key"].(string); ok {
				seen[key] = true
			}
		}
	}
	for _, key := range []string{"wordpress", "mariadb", "grafana", "prometheus"} {
		if !seen[key] {
			t.Errorf("Qwen omitted %s from WordPress monitoring", key)
		}
	}
}

func TestLMStudioRealQwenQualityMatrix(t *testing.T) {
	if os.Getenv("LM_STUDIO_INTEGRATION") != "1" {
		t.Skip("requires local Qwen")
	}
	t.Setenv("LM_STUDIO_MODEL", "autodocker-qwen-test")
	t.Setenv("LM_STUDIO_URL", "http://127.0.0.1:1234")
	t.Setenv("AUTODOCKER_AI_PROVIDER", "lmstudio")
	for _, tc := range []struct {
		name, prompt     string
		wanted, unwanted []string
		question         bool
	}{
		{"multiple-needs-exclusions", "WordPress avec supervision graphana et Redis, sans Traefik ou Portainer.", []string{"wordpress", "mariadb", "grafana", "prometheus", "redis"}, []string{"traefik", "portainer"}, false},
		{"logs-not-metrics", "Grafana avec Loki pour les logs, sans Prometheus.", []string{"grafana", "loki"}, []string{"prometheus"}, false},
		{"ambiguity", "Une infra pour du jeu vidéo que j'aimerais partager.", nil, nil, true},
		{"clarified-game", "Une infra pour du jeu vidéo que j'aimerais partager. Précision : un serveur Minecraft Java pour 5 amis. Seulement le serveur de jeu.", []string{"minecraft"}, []string{"jellyfin", "minio", "portainer", "traefik", "debian"}, false},
		{"custom-settings", "WordPress version 6.8 accessible sur le port 43879, avec sa base MariaDB. Seulement ces services.", []string{"wordpress", "mariadb"}, []string{"traefik", "portainer"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now()
			provider, _ := selectedInfrastructureProvider()
			complete := provider.complete
			provider.complete = func(ctx context.Context, system, message string) (string, error) {
				text, err := complete(ctx, system, message)
				if err == nil {
					if raw, parseErr := parseAIConfigText(text); parseErr == nil {
						if _, _, issues := validateInfrastructureProposal(tc.prompt, raw, infrastructureCatalogue()); len(issues) > 0 {
							t.Logf("invalid proposal diagnostics: %s", boundedAIText(text, 3500))
						}
					}
				}
				return text, err
			}
			result, err := generateInfrastructureWithProvider(context.Background(), tc.prompt, provider)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("source=%s attempts=%d elapsed=%s", result.Source, result.Attempts, time.Since(started).Round(time.Millisecond))
			if tc.question {
				if len(result.Questions) == 0 || result.Payload != nil {
					t.Fatal("ambiguous request did not ask a question")
				}
				return
			}
			keys := infrastructureKeys(result)
			for _, key := range tc.wanted {
				if !keys[key] {
					t.Fatal("missing " + key)
				}
			}
			for _, key := range tc.unwanted {
				if keys[key] {
					t.Fatal("excluded service included: " + key)
				}
			}
			if tc.name == "custom-settings" {
				for _, s := range normalizeServiceList(result.Payload["services"]) {
					if s["key"] == "wordpress" {
						image, _ := s["image"].(string)
						if !strings.HasPrefix(image, "wordpress:6.8") {
							t.Fatal("version ignored")
						}
						ports := toStringList(s["ports"])
						if !containsInfrastructureString(ports, "43879:80") {
							t.Fatalf("port ignored: %v", ports)
						}
					}
				}
			}
		})
	}
}

func TestLMStudioPreservesRequestedComposition(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/models" {
			w.Write([]byte(`{"models":[]}`))
			return
		}
		if r.URL.Path != "/v1/chat/completions" || r.Method != "POST" {
			t.Error("wrong LM Studio request")
		}
		var request map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request["model"] != "qwen-test" {
			t.Error("wrong model")
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"message": map[string]interface{}{"content": `{"services":[{"key":"gitea"},{"key":"postgres"},{"key":"redis"}]}`}}}})
	}))
	defer server.Close()
	t.Setenv("LM_STUDIO_MODEL", "qwen-test")
	t.Setenv("LM_STUDIO_URL", server.URL)
	result, err := GenerateMagicConfig("Gitea, PostgreSQL et Redis, 8 Go RAM et 4 CPU, accès réseau privé")
	if err != nil {
		t.Fatal(err)
	}
	if result.Source != "lmstudio" || len(result.Payload["services"].([]interface{})) != 3 {
		t.Fatalf("composition changed: %+v", result)
	}
}

func TestLMStudioFailureDoesNotBecomeAnUnrelatedPreset(t *testing.T) {
	for _, answer := range []string{`{"error":"model unavailable"}`, `{"choices":[{"message":{"content":"not json"}}]}`, `{"choices":[{"message":{"content":"{\"services\":[]}"}}]}`} {
		t.Run(answer, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(answer)) }))
			defer server.Close()
			t.Setenv("LM_STUDIO_MODEL", "qwen-test")
			t.Setenv("LM_STUDIO_URL", server.URL)
			if _, err := GenerateMagicConfig("Gitea et Redis, 8 Go RAM et 4 CPU, accès réseau privé"); err == nil {
				t.Fatal("invalid provider response became success")
			}
		})
	}
}

func TestLMStudioRejectsRemoteDestinations(t *testing.T) {
	t.Setenv("LM_STUDIO_MODEL", "qwen-test")
	t.Setenv("LM_STUDIO_URL", "https://example.com")
	if _, err := GenerateLMStudioConfig("Un site"); err == nil {
		t.Fatal("accepted a remote destination")
	}
}
