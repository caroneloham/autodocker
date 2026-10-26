package services

import (
	"context"
	"strings"
	"testing"
)

func TestSimpleAIRequestDoesNotRequireAnIntakeForm(t *testing.T) {
	t.Setenv("AUTODOCKER_AI_PROVIDER", "missing-provider")
	result, err := GenerateInfrastructureConfig(context.Background(), "Un chat IA pour mon équipe avec Open WebUI")
	if err == nil || result != nil {
		t.Fatal("simple prompt was blocked by a mandatory intake questionnaire")
	}
}

func TestExportSQLAccountsAreIsolatedAndProvisioned(t *testing.T) {
	p := BuildComposePayload{Services: []ServiceEntry{{Key: "wordpress"}, {Key: "nextcloud"}, {Key: "mariadb"}, {Key: "gitea"}, {Key: "postgres"}, {Key: "redis"}}}
	c := BuildComposeForExport(p, "isolated-sql")
	if err := ValidateGeneratedConnections(c); err != nil {
		t.Fatal(err)
	}
	sv := c["services"].(M)
	configs := c["configs"].(M)
	wp := sv["wordpress"].(M)["environment"].(M)
	nc := sv["nextcloud"].(M)["environment"].(M)
	if wp["WORDPRESS_DB_USER"] == nc["MYSQL_USER"] || wp["WORDPRESS_DB_PASSWORD"] == nc["MYSQL_PASSWORD"] || wp["WORDPRESS_DB_NAME"] == nc["MYSQL_DATABASE"] {
		t.Fatal("SQL applications share a credential/database")
	}
	sql := configs["autodocker_mariadb_apps"].(M)["content"].(string)
	for _, entry := range []struct {
		env         M
		field, name string
	}{{wp, "WORDPRESS_DB_PASSWORD", "wordpress"}, {nc, "MYSQL_PASSWORD", "nextcloud"}} {
		if !strings.Contains(sql, entry.env[entry.field].(string)) || !strings.Contains(sql, "ON `"+entry.name+"`.*") {
			t.Fatal("SQL initializer and application diverge")
		}
	}
	if strings.Contains(sql, "ON *.*") {
		t.Fatal("global SQL grant")
	}
	gt := sv["gitea"].(M)["environment"].(M)
	pg := configs["autodocker_postgres_apps"].(M)["content"].(string)
	if gt["GITEA__database__USER"] == "postgres" || !strings.Contains(pg, gt["GITEA__database__PASSWD"].(string)) || !strings.Contains(pg, "NOSUPERUSER NOCREATEDB NOCREATEROLE") {
		t.Fatal("Gitea is not provisioned with a limited account")
	}
	if nc["REDIS_HOST_PASSWORD"] != applicationCommandOption(sv["redis"].(M), "--requirepass") {
		t.Fatal("Redis password mismatch")
	}
	other := BuildComposeForExport(p, "isolated-sql")["services"].(M)["wordpress"].(M)["environment"].(M)
	if other["WORDPRESS_DB_PASSWORD"] == wp["WORDPRESS_DB_PASSWORD"] {
		t.Fatal("credentials reused across exports")
	}
}

func TestExportSecurityAndInferenceConnections(t *testing.T) {
	p := BuildComposePayload{Services: []ServiceEntry{{Key: "ollama", Extra: map[string]string{"model": "qwen3:4b"}}, {Key: "open-webui", Env: map[string]string{"WEBUI_AUTH": "False"}}, {Key: "grafana"}, {Key: "prometheus"}, {Key: "qdrant", Image: "qdrant/qdrant:v1.15", Ports: []string{"6333:6333"}}}}
	c := BuildComposeForExport(p, "secure-inference")
	sv := c["services"].(M)
	if err := ValidateGeneratedConnections(c); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ollama", "prometheus", "qdrant"} {
		if len(toStringList(sv[key].(M)["ports"])) != 0 {
			t.Fatalf("internal port published: %s", key)
		}
	}
	web := sv["open-webui"].(M)
	env := web["environment"].(M)
	if env["WEBUI_AUTH"] != "True" || env["ENABLE_SIGNUP"] != "False" || len(env["WEBUI_ADMIN_PASSWORD"].(string)) < 24 || env["DEFAULT_MODELS"] != "qwen3:4b" {
		t.Fatal("unsafe auth or wrong model")
	}
	for _, port := range toStringList(web["ports"]) {
		if !strings.HasPrefix(port, "127.0.0.1:") {
			t.Fatal("UI publicly bound")
		}
	}
	if web["depends_on"].(M)["ollama-model-init"].(M)["condition"] != "service_completed_successfully" {
		t.Fatal("WebUI can start before model download")
	}
	if strings.Join(toStringList(sv["ollama-model-init"].(M)["command"]), " ") != "pull qwen3:4b" {
		t.Fatal("initializer and interface models diverge")
	}
	if sv["grafana"].(M)["environment"].(M)["GF_SECURITY_ADMIN_PASSWORD"] == "admin" {
		t.Fatal("default Grafana password")
	}
}

func TestLocalDeploymentKeepsInferencePrivateAndRequiresWebUILogin(t *testing.T) {
	p := BuildComposePayload{Services: []ServiceEntry{
		{Key: "ollama"},
		{Key: "open-webui", Env: map[string]string{"WEBUI_AUTH": "False"}},
		{Key: "nginx"},
	}}
	c := BuildComposeForLocalDeployment(p, "local-inference")
	sv := c["services"].(M)
	if ports := toStringList(sv["ollama"].(M)["ports"]); len(ports) != 0 {
		t.Fatalf("Ollama API exposed on host: %v", ports)
	}
	web := sv["open-webui"].(M)
	for _, port := range toStringList(web["ports"]) {
		if !strings.HasPrefix(port, "127.0.0.1:") {
			t.Fatalf("Open WebUI exposed outside localhost: %s", port)
		}
	}
	env := web["environment"].(M)
	if env["WEBUI_AUTH"] != "True" || env["ENABLE_LOGIN_FORM"] != "True" || env["ENABLE_SIGNUP"] != "False" {
		t.Fatalf("Open WebUI login is not required: %#v", env)
	}
	if password, ok := env["WEBUI_ADMIN_PASSWORD"].(string); !ok || len(password) < 24 {
		t.Fatal("Open WebUI lacks a strong initial admin password")
	}
	for _, port := range toStringList(sv["nginx"].(M)["ports"]) {
		if !strings.HasPrefix(port, "127.0.0.1:") {
			t.Fatalf("other local service exposed outside localhost: %s", port)
		}
	}
}

func TestExportKeepsExternalSQLAndRejectsBrokenTopology(t *testing.T) {
	p := BuildComposePayload{Services: []ServiceEntry{{Key: "wordpress", Env: map[string]string{"WORDPRESS_DB_HOST": "db.external.example:3306", "WORDPRESS_DB_PASSWORD": "external-reference"}}}}
	c := BuildComposeForExport(p, "external-sql")
	if c["services"].(M)["wordpress"].(M)["environment"].(M)["WORDPRESS_DB_PASSWORD"] != "external-reference" {
		t.Fatal("external database overwritten")
	}
	broken := M{"services": M{"web": M{"depends_on": []string{"missing"}}}}
	if ValidateGeneratedConnections(broken) == nil {
		t.Fatal("missing dependency accepted")
	}
	broken = M{"services": M{"web": M{"networks": []string{"a"}, "environment": M{"OLLAMA_BASE_URL": "http://ollama:11434"}, "depends_on": []string{"ollama"}}, "ollama": M{"networks": []string{"b"}}}}
	if ValidateGeneratedConnections(broken) == nil {
		t.Fatal("disconnected internal URL accepted")
	}
}
