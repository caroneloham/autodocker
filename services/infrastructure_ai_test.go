package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func infrastructureAnswer(keys ...string) string {
	list := []map[string]string{}
	for _, key := range keys {
		list = append(list, map[string]string{"key": key})
	}
	data, _ := json.Marshal(map[string]interface{}{"services": list})
	return string(data)
}

func infrastructureKeys(result *MagicAIResult) map[string]bool {
	keys := map[string]bool{}
	for _, entry := range normalizeServiceList(result.Payload["services"]) {
		key, _ := entry["key"].(string)
		keys[key] = true
	}
	return keys
}

func TestInfrastructureFrenchAucunExclusionsAndPositiveResumption(t *testing.T) {
	catalogue := infrastructureCatalogue()
	for _, prompt := range []string{"Grafana avec Loki. Aucun Prometheus, aucun Redis, aucun Traefik et aucun Portainer.", "Grafana avec Loki, aucun Redis ni Traefik ou Portainer."} {
		mentions := mentionedInfrastructureServices(prompt, catalogue)
		for _, key := range []string{"redis", "traefik", "portainer"} {
			if present, found := mentions[key]; !found || present {
				t.Fatalf("negative mention lost for %s", key)
			}
		}
		if !mentions["grafana"] || !mentions["loki"] {
			t.Fatal("positive services lost")
		}
	}
	if !mentionedInfrastructureServices("aucun Redis ni Traefik, avec Portainer", catalogue)["portainer"] {
		t.Fatal("negative scope swallowed a new positive request")
	}
}

func TestInfrastructureEnvironmentAliasPreservesStrictValidation(t *testing.T) {
	input := map[string]interface{}{"key": "redis", "environment": map[string]interface{}{"TZ": "Europe/Paris"}}
	normalized := normalizeInfrastructureServiceAliases(input)
	if normalized["env"] == nil || normalized["environment"] != nil || input["environment"] == nil {
		t.Fatal("equivalent alias not normalized without mutation")
	}
	p := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) {
		return `{"services":[{"key":"redis","environment":{"TZ":"Europe/Paris"}}]}`, nil
	}}
	if _, err := generateInfrastructureWithProvider(context.Background(), "Redis avec TZ Europe/Paris", p); err != nil {
		t.Fatal(err)
	}
	p.complete = func(context.Context, string, string) (string, error) {
		return `{"services":[{"key":"redis","environment":["TZ=Europe/Paris"]}]}`, nil
	}
	if _, err := generateInfrastructureWithProvider(context.Background(), "Redis avec TZ Europe/Paris", p); err != nil {
		t.Fatal("Compose-style KEY=VALUE environment rejected:", err)
	}
	if infrastructureDocumentSearchRequested("Open WebUI pour du texte, n’ajoute ni RAG ni Qdrant") {
		t.Fatal("negated RAG became a request")
	}
	for _, field := range []string{`"privileged":true`, `"env":{"TZ":"conflicting"},"environment":{"TZ":"Europe/Paris"}`} {
		p.complete = func(context.Context, string, string) (string, error) {
			return `{"services":[{"key":"redis",` + field + `}]}`, nil
		}
		if _, err := generateInfrastructureWithProvider(context.Background(), "Redis", p); err == nil {
			t.Fatal("unknown or conflicting field silently accepted")
		}
	}
	for _, env := range []string{`["PATH"]`, `["TZ=one","TZ=two"]`, `[true]`} {
		p.complete = func(context.Context, string, string) (string, error) {
			return `{"services":[{"key":"redis","env":` + env + `}]}`, nil
		}
		if _, err := generateInfrastructureWithProvider(context.Background(), "Redis", p); err == nil {
			t.Fatal("ambiguous environment silently accepted")
		}
	}
}

func TestInfrastructureBuiltinsDoNotDependOnInventedEnvFiles(t *testing.T) {
	entry := map[string]interface{}{"key": "gitea", "env_file": ".env"}
	clean, changed := normalizeUnrequestedTemplateEnvFile("Gitea avec PostgreSQL, prépare les fichiers", entry)
	if !changed || clean["env_file"] != nil || entry["env_file"] == nil {
		t.Fatal("unrequested file was retained or input mutated")
	}
	for _, prompt := range []string{"Gitea avec env_file", "Gitea avec mon fichier d’environnement", "Gitea doit charger .env"} {
		if _, changed := normalizeUnrequestedTemplateEnvFile(prompt, entry); changed {
			t.Fatal("explicit unsupported request silently discarded")
		}
	}
	if _, changed := normalizeUnrequestedTemplateEnvFile("Une application personnalisée", map[string]interface{}{"key": "custom-app", "env_file": ".env"}); changed {
		t.Fatal("custom configuration silently discarded")
	}
}

func TestExplicitHTTPProxyDoesNotBecomePrivateHTTPS(t *testing.T) {
	payload := BuildComposePayload{Services: []ServiceEntry{{Key: "caddy"}}, Ingress: &IngressConfig{Proxy: "caddy", Domain: "localhost", Port: 8443, TLS: "internal"}}
	normalizeExplicitIngressIntent("Caddy comme unique entrée HTTP sur observ.test:8090", &payload)
	if payload.Ingress.TLS != "" || payload.Ingress.Domain != "observ.test" || payload.Ingress.Port != 8090 {
		t.Fatal("explicit HTTP protocol/address changed")
	}
	payload.Ingress = nil
	normalizeExplicitIngressIntent("Nextcloud avec Caddy en HTTPS privé sur localhost:8444", &payload)
	if payload.Ingress == nil || payload.Ingress.TLS != "internal" || payload.Ingress.Port != 8444 {
		t.Fatal("non-AI private TLS request lost")
	}
}

func TestExplicitCaddyProxyGetsRealRoutesWithoutModelIngress(t *testing.T) {
	p := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) {
		return infrastructureAnswer("gitea", "postgres", "caddy"), nil
	}}
	result, err := generateInfrastructureWithProvider(context.Background(), "Gitea avec PostgreSQL et Caddy comme unique proxy des interfaces web", p)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result.Payload)
	var payload BuildComposePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Ingress == nil || payload.Ingress.Proxy != "caddy" {
		t.Fatal("Caddy was added without a routing configuration")
	}
	compose := BuildCompose(payload, "routing-regression")
	services := compose["services"].(M)
	config, _ := services["caddy"].(M)["environment"].(M)["AUTODOCKER_PROXY_CONFIG"].(string)
	if !strings.Contains(config, "gitea:3000") || len(toStringList(services["gitea"].(M)["ports"])) != 0 {
		t.Fatal("Gitea does not actually go through Caddy")
	}
	if !strings.Contains(strings.Join(result.Warnings, " "), "HTTP") {
		t.Fatal("default local HTTP routing must be explained")
	}
}

func TestCaddyRoutingDefaultPreservesTLSAndStandaloneIntent(t *testing.T) {
	payload := BuildComposePayload{Services: []ServiceEntry{{Key: "caddy"}}, Ingress: &IngressConfig{Proxy: "caddy", Domain: "localhost", Port: 8443, TLS: "internal"}}
	normalizeExplicitIngressIntent("Caddy comme unique proxy", &payload)
	if payload.Ingress.TLS != "internal" || payload.Ingress.Port != 8443 {
		t.Fatal("existing private TLS was downgraded")
	}
	for _, prompt := range []string{"Caddy seul pour plus tard", "Caddy sans routage", "Caddy sans reverse proxy"} {
		payload.Ingress = nil
		normalizeExplicitIngressIntent(prompt, &payload)
		if payload.Ingress != nil {
			t.Fatalf("unrequested routing added for %q", prompt)
		}
	}
}

func TestCommaExclusionsAndSSHPortDoNotReconfigureCaddy(t *testing.T) {
	prompt := "Gitea avec PostgreSQL 16 et Caddy. Accès web derrière Caddy en HTTP sur le domaine forge.test et le port 8088 ; conserve un accès Git SSH sur le port 2222 si possible. Pas de Traefik, Nginx, Portainer, MariaDB ni outil CI."
	mentions := mentionedInfrastructureServices(prompt, infrastructureCatalogue())
	for _, key := range []string{"traefik", "nginx", "portainer", "mariadb"} {
		if wanted, found := mentions[key]; !found || wanted {
			t.Fatalf("comma exclusion lost: %s", key)
		}
	}
	if !mentionedInfrastructureServices("Sans Redis. Nginx seul.", infrastructureCatalogue())["nginx"] {
		t.Fatal("negative scope crossed a sentence")
	}
	settings := infrastructureExplicitSettings(prompt, infrastructureCatalogue())
	if settings["caddy"].HostPort != 8088 {
		t.Fatal("SSH port overwrote Caddy port")
	}
}

func TestPrivateDataIgnoresModelPublicationAndImplicitAdminer(t *testing.T) {
	p := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) {
		return `{"services":[{"key":"wordpress"},{"key":"mariadb","ports":["3306:3306"]},{"key":"redis","ports":["6379:6379"]}],"ui":{"db":true}}`, nil
	}}
	r, err := generateInfrastructureWithProvider(context.Background(), "WordPress avec MariaDB et Redis. Les bases et caches restent internes, sans port publié.", p)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r.Payload)
	var payload BuildComposePayload
	json.Unmarshal(data, &payload)
	sv := BuildCompose(payload, "private-data-test")["services"].(M)
	if sv["adminer"] != nil || len(toStringList(sv["redis"].(M)["ports"])) != 0 || len(toStringList(sv["mariadb"].(M)["ports"])) != 0 {
		t.Fatal("model UI flags or ports exposed private databases")
	}
}

func TestInfrastructureMariaDBImageCorrectsMisnamedMySQLService(t *testing.T) {
	answer := `{"services":[{"key":"wordpress","depends_on":["mysql"]},{"key":"mysql","image":"mariadb:11"}]}`
	provider := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) { return answer, nil }}
	result, err := generateInfrastructureWithProvider(context.Background(), "WordPress avec MariaDB", provider)
	if err != nil {
		t.Fatal(err)
	}
	keys := infrastructureKeys(result)
	if !keys["mariadb"] || keys["mysql"] {
		t.Fatal("MariaDB image was not normalized to its actual product")
	}
	for _, service := range normalizeServiceList(result.Payload["services"]) {
		dependencies, _ := service["depends_on"].([]interface{})
		for _, dependency := range dependencies {
			if dependency == "mysql" {
				t.Fatal("stale database reference")
			}
		}
	}

	answer = `{"services":[{"key":"mysql","image":"mysql:8.4"}]}`
	result, err = generateInfrastructureWithProvider(context.Background(), "Un serveur MySQL", provider)
	if err != nil {
		t.Fatal(err)
	}
	if !infrastructureKeys(result)["mysql"] || infrastructureKeys(result)["mariadb"] {
		t.Fatal("a real MySQL service must remain MySQL")
	}
	answer = `{"services":[{"key":"mysql","image":"mariadb:11"}]}`
	_, err = generateInfrastructureWithProvider(context.Background(), "Un serveur MySQL", provider)
	if err == nil || !strings.Contains(err.Error(), "pas MariaDB") {
		t.Fatal("the requested database product was silently replaced")
	}
}

func TestInfrastructureMinecraftHasKnownDefaultsWithoutAcceptingTerms(t *testing.T) {
	provider := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) {
		return `{"services":[{"key":"minecraft","env":{"EULA":"TRUE"}}]}`, nil
	}}
	result, err := generateInfrastructureWithProvider(context.Background(), "Un serveur Minecraft Java pour 5 amis", provider)
	if err != nil {
		t.Fatal(err)
	}
	entries := normalizeServiceList(result.Payload["services"])
	if len(entries) != 1 || entries[0]["key"] != "minecraft" || entries[0]["image"] != "itzg/minecraft-server:latest" {
		t.Fatalf("wrong game specification: %+v", entries)
	}
	env, _ := entries[0]["env"].(map[string]interface{})
	if env["EULA"] != "FALSE" {
		t.Fatal("the assistant accepted terms for the user")
	}
	if len(result.Warnings) == 0 {
		t.Fatal("manual setup was not explained")
	}
}

func TestInfrastructurePipelineRecoversExplicitCatalogueOmissionsForAnyProvider(t *testing.T) {
	for _, name := range []string{"lmstudio", "local", "gemini", "openrouter", "mistral", "groq", "compatible"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			p := infrastructureProvider{name: name, complete: func(ctx context.Context, system, message string) (string, error) {
				calls++
				if !strings.Contains(system, "CATALOGUE") || strings.Contains(system, "TOUJOURS inclus") {
					t.Fatal("provider did not get the shared grounded contract")
				}
				return infrastructureAnswer("gitea", "postgres"), nil
			}}
			result, err := generateInfrastructureWithProvider(context.Background(), "Gitea, PostgreSQL et Redis", p)
			if err != nil {
				t.Fatal(err)
			}
			if result.Attempts != 1 || calls != 1 || result.Source != name {
				t.Fatalf("wrong provenance: %+v", result)
			}
			keys := infrastructureKeys(result)
			for _, key := range []string{"gitea", "postgres", "redis"} {
				if !keys[key] {
					t.Fatal("missing " + key)
				}
			}
			if keys["portainer"] || keys["traefik"] || keys["nginx"] {
				t.Fatal("unrequested foundation was forced")
			}
		})
	}
}

func TestInfrastructureDependenciesAndExclusions(t *testing.T) {
	tests := []struct {
		prompt, answer   string
		wanted, unwanted []string
	}{
		{"WordPress et graphana", infrastructureAnswer("wordpress", "grafana"), []string{"wordpress", "mariadb", "grafana", "prometheus"}, []string{"portainer"}},
		{"Grafana avec Loki sans Prometheus", `{"services":[{"key":"grafana"},{"key":"loki","image":"grafana/loki:3.0.0"}]}`, []string{"grafana", "loki"}, []string{"prometheus"}},
		{"WordPress sans Redis", infrastructureAnswer("wordpress"), []string{"wordpress", "mariadb"}, []string{"redis"}},
		{"WordPress sans Traefik ou Portainer", infrastructureAnswer("wordpress"), []string{"wordpress", "mariadb"}, []string{"traefik", "portainer"}},
	}
	for _, tc := range tests {
		t.Run(tc.prompt, func(t *testing.T) {
			p := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) { return tc.answer, nil }}
			r, err := generateInfrastructureWithProvider(context.Background(), tc.prompt, p)
			if err != nil {
				t.Fatal(err)
			}
			keys := infrastructureKeys(r)
			for _, key := range tc.wanted {
				if !keys[key] {
					t.Fatal("missing " + key)
				}
			}
			for _, key := range tc.unwanted {
				if keys[key] {
					t.Fatal("unexpected " + key)
				}
			}
		})
	}
}

func TestInfrastructureInvalidProposalsNeverBecomePresets(t *testing.T) {
	for _, answer := range []string{
		`not json`, `{"services":[]}`,
		`{"services":[{"key":"unknown"}]}`,
		`{"services":[{"key":"nginx","privileged":true}]}`,
		`{"services":[{"key":"tool","image":"alpine:3.21","volumes":["/etc:/host"]}]}`,
		`{"services":[{"key":"tool","image":"alpine:3.21","volumes":["sock:/var/run/docker.sock"]}]}`,
		`{"services":[{"key":"nginx","depends_on":["redis"]},{"key":"redis","depends_on":["nginx"]}]}`,
		`{"services":[{"key":"nginx"}],"addressing":{"networks":[{"name":"app","cidr":"invalid","gateway":"1.2.3.4"}]}}`,
		`{"services":[{"key":"nginx","env":{"APP_PASSWORD":"hardcoded"}}]}`,
		`{"services":[{"key":"nginx","network":"app","ip":"172.25.0.1"}],"addressing":{"networks":[{"name":"app","cidr":"172.25.0.0/24","gateway":"172.25.0.1"}]}}`,
	} {
		t.Run(answer, func(t *testing.T) {
			calls := 0
			p := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) { calls++; return answer, nil }}
			r, err := generateInfrastructureWithProvider(context.Background(), "Une infrastructure avec configuration des variables", p)
			if err == nil || r != nil || calls != 2 {
				t.Fatalf("invalid response accepted: calls=%d result=%+v err=%v", calls, r, err)
			}
		})
	}
}

func TestInfrastructureClarificationDoesNotInventServices(t *testing.T) {
	p := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) {
		return `{"services":[],"questions":["Un jeu dans le navigateur ou un serveur de jeu existant ?"]}`, nil
	}}
	r, err := generateInfrastructureWithProvider(context.Background(), "Une infra pour du jeu vidéo que je partagerais", p)
	if err != nil || r.Payload != nil || len(r.Questions) != 1 || r.Confidence != "needs_clarification" {
		t.Fatalf("clarification became an architecture: %+v %v", r, err)
	}
}

func TestInfrastructureProviderProtocolsShareContract(t *testing.T) {
	for _, protocol := range []string{"openai", "ollama", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]interface{}
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid request")
					return
				}
				blob, _ := json.Marshal(body)
				if !strings.Contains(string(blob), "CATALOGUE") {
					t.Error("missing shared catalogue")
				}
				answer := infrastructureAnswer("wordpress")
				switch protocol {
				case "gemini":
					fmt.Fprintf(w, `{"candidates":[{"content":{"parts":[{"text":%q}]}}]}`, answer)
				case "ollama":
					fmt.Fprintf(w, `{"message":{"content":%q}}`, answer)
				default:
					fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, answer)
				}
			}))
			defer server.Close()
			p := infrastructureProvider{name: protocol, complete: func(ctx context.Context, system, message string) (string, error) {
				return requestInfrastructureModel(ctx, server.URL, "test", "", protocol, system, message)
			}}
			r, err := generateInfrastructureWithProvider(context.Background(), "WordPress", p)
			if err != nil {
				t.Fatal(err)
			}
			if !infrastructureKeys(r)["mariadb"] {
				t.Fatal("protocol bypassed dependency validation")
			}
		})
	}
}

func TestInfrastructureCancellationAndProviderErrorsStayErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	p := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) {
		calls++
		return "", fmt.Errorf("provider offline")
	}}
	if _, err := generateInfrastructureWithProvider(ctx, "Un site", p); err != context.Canceled || calls != 0 {
		t.Fatal("cancelled request reached provider")
	}
	if _, err := generateInfrastructureWithProvider(context.Background(), "Un site", p); err == nil || calls != 1 {
		t.Fatal("provider error was retried as a fake preset")
	}
	if _, err := generateInfrastructureWithProvider(context.Background(), strings.Repeat("é", 8001), p); err == nil || calls != 1 {
		t.Fatal("input limit not enforced before inference")
	}
}
