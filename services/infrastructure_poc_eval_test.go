package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPOCLiveQwenEvaluation(t *testing.T) {
	if os.Getenv("AUTODOCKER_QWEN_EVAL") != "1" {
		t.Skip("explicit live evaluation only")
	}
	type scenario struct {
		ID, Prompt         string
		Required, Excluded []string
		Question           bool
	}
	cases := []scenario{
		{"wordpress-metrics", "Un site WordPress avec Grafana pour les métriques. Sans Portainer ni Traefik.", []string{"wordpress", "mariadb", "grafana", "prometheus"}, []string{"portainer", "traefik"}, false},
		{"rag-functional", "Une IA locale accessible dans un navigateur, avec comptes privés pour plusieurs utilisateurs. Je veux poser des questions sur mes documents PDF avec des sources. Utilise Open WebUI et Ollama.", []string{"open-webui", "ollama", "tei", "qdrant"}, nil, false},
		{"private-vision", "Une plateforme IA privée avec Open WebUI, vLLM, Qwen/Qwen2.5-VL-3B-Instruct, vision, TEI et Qdrant. Caddy en HTTPS privé local sur le port 8443. Contexte 8192, deux requêtes simultanées. Sans Ollama.", []string{"open-webui", "vllm", "tei", "qdrant", "caddy"}, []string{"ollama"}, false},
		{"logs-exclusion", "Grafana avec Loki pour les logs, sans Prometheus ni Portainer.", []string{"grafana", "loki"}, []string{"prometheus", "portainer"}, false},
		{"dev-versions", "Gitea avec PostgreSQL version 16 et Redis version 7. Sans Portainer.", []string{"gitea", "postgres", "redis"}, []string{"portainer"}, false},
		{"gaming-clarification", "Une infra pour du jeu vidéo que je voudrais partager.", nil, nil, true},
	}
	records := []M{}
	for _, tc := range cases {
		t.Run(tc.ID, func(t *testing.T) {
			start := time.Now()
			calls := 0
			p := infrastructureProvider{name: "lmstudio-qwen", complete: func(ctx context.Context, system, message string) (string, error) {
				calls++
				return requestInfrastructureModel(ctx, "http://127.0.0.1:1234/v1/chat/completions", "qwen3.5-9b", "", "lmstudio", system, message)
			}}
			result, err := generateInfrastructureWithProvider(context.Background(), tc.Prompt, p)
			problems := []string{}
			if err != nil {
				problems = append(problems, err.Error())
			} else if tc.Question {
				if result.Payload != nil || len(result.Questions) == 0 {
					problems = append(problems, "clarification expected")
				}
			} else {
				keys := infrastructureKeys(result)
				for _, key := range tc.Required {
					if !keys[key] {
						problems = append(problems, "missing "+key)
					}
				}
				for _, key := range tc.Excluded {
					if keys[key] {
						problems = append(problems, "excluded "+key)
					}
				}
				if result.Payload != nil {
					data, _ := json.Marshal(result.Payload)
					var payload BuildComposePayload
					_ = json.Unmarshal(data, &payload)
					if e := ValidateInfrastructureOptions(payload); e != nil {
						problems = append(problems, e.Error())
					}
				}
			}
			record := M{"id": tc.ID, "passed": len(problems) == 0, "calls": calls, "duration_ms": time.Since(start).Milliseconds(), "problems": problems}
			records = append(records, record)
			t.Logf("EVAL %s: calls=%d duration=%s passed=%t", tc.ID, calls, time.Since(start).Round(time.Millisecond), len(problems) == 0)
			for _, problem := range problems {
				t.Error(problem)
			}
		})
	}
	if output := os.Getenv("AUTODOCKER_EVAL_REPORT"); output != "" {
		if filepath.IsAbs(output) {
			data, _ := json.MarshalIndent(M{"model": "qwen3.5-9b", "scenarios": records}, "", "  ")
			if err := os.WriteFile(output, data, 0600); err != nil {
				t.Error(err)
			}
		}
	}
}
