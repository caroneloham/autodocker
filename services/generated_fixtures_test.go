package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestGeneratedFixtureMatrix(t *testing.T) {
	cases := map[string]BuildComposePayload{
		"wordpress-nextcloud-sql-redis": {Services: []ServiceEntry{{Key: "wordpress"}, {Key: "nextcloud"}, {Key: "mariadb"}, {Key: "redis"}}},
		"gitea-postgres":                {Services: []ServiceEntry{{Key: "gitea"}, {Key: "postgres"}}},
		"ollama-chat":                   {Services: []ServiceEntry{{Key: "ollama", Extra: map[string]string{"model": "qwen3:4b"}}, {Key: "open-webui"}}},
		"private-vllm-rag":              privatePlatformPayload(),
		"mongo-admin":                   {Services: []ServiceEntry{{Key: "mongo-express"}, {Key: "mongo"}}},
		"grafana-metrics":               {Services: []ServiceEntry{{Key: "grafana"}, {Key: "prometheus"}, {Key: "loki", Image: "grafana/loki:3.7.0"}}},
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			compose := BuildComposeForExport(payload, "hardening-"+name)
			if err := ValidateGeneratedConnections(compose); err != nil {
				t.Fatal(err)
			}
			if path := os.Getenv("AUTODOCKER_FIXTURE_DIR"); path != "" {
				data, err := yaml.Marshal(compose)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, name+".yaml"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestIngressExplanationsCannotPromisePublicTLS(t *testing.T) {
	payload := map[string]interface{}{"ingress": map[string]interface{}{"proxy": "caddy", "tls": "internal"}, "services": []interface{}{map[string]interface{}{"key": "caddy"}}}
	reasons := map[string]string{"caddy": "Let's Encrypt automatique"}
	summary, warnings := infrastructureIngressExplanation(payload, "HTTPS automatique avec Let's Encrypt", reasons, []string{"Un certificat public sera émis automatiquement"})
	if strings.Contains(summary, "Let's Encrypt") || len(warnings) != 1 || !strings.Contains(reasons["caddy"], "certificat privé") {
		t.Fatal("provider claims contradicted generated TLS")
	}
}
