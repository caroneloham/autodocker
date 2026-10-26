package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsedSubnetsExcludesCurrentProjectAndIncludesOtherNetworks(t *testing.T) {
	root := t.TempDir()
	for _, project := range []string{"demo", "other"} {
		if err := os.Mkdir(filepath.Join(root, project), 0755); err != nil {
			t.Fatal(err)
		}
		cidr := map[string]string{"demo": "172.20.10.0/24", "other": "172.20.11.0/24"}[project]
		content := "networks:\n  front:\n    ipam:\n      config:\n        - subnet: " + cidr + "\n"
		if err := os.WriteFile(filepath.Join(root, project, "compose.yaml"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	used := getUsedSubnetsWithRunner(root, "demo", func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("timeout requis pour Docker")
		}
		if strings.Join(args, " ") == "network ls -q" {
			return []byte("own other bridge\n"), nil
		}
		if strings.Join(args, " ") != "network inspect own other bridge" {
			t.Fatalf("commande inattendue: %v", args)
		}
		return []byte(`[
			{"Labels":{"com.docker.compose.project":"demo"},"IPAM":{"Config":[{"Subnet":"172.20.10.0/24"}]}},
			{"Labels":{"com.docker.compose.project":"elsewhere"},"IPAM":{"Config":[{"Subnet":"172.20.12.0/24"}]}},
			{"Labels":{},"IPAM":{"Config":[{"Subnet":"172.17.0.0/16"}]}}
		]`), nil
	})
	if used["172.20.10.0/24"] {
		t.Fatal("le propre reseau du projet est traite comme un conflit")
	}
	for _, cidr := range []string{"172.20.11.0/24", "172.20.12.0/24", "172.17.0.0/16"} {
		if !used[cidr] {
			t.Errorf("reseau occupe manquant: %s", cidr)
		}
	}
	if calls != 2 {
		t.Fatalf("%d commandes au lieu de 2", calls)
	}
}

func TestUsedSubnetsOfflineKeepsPreparedProjectReservations(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "other"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other", "compose.yaml"), []byte("networks:\n  back:\n    ipam:\n      config:\n        - subnet: 172.20.20.0/24\n"), 0644); err != nil {
		t.Fatal(err)
	}
	used := getUsedSubnetsWithRunner(root, "demo", func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("Docker absent")
	})
	if !used["172.20.20.0/24"] {
		t.Fatal("un projet prepare hors ligne doit reserver son sous-reseau")
	}
}

func TestGatewayFromCIDRUsesNetworkBase(t *testing.T) {
	for _, tc := range []struct{ cidr, gateway string }{
		{"172.20.10.0/24", "172.20.10.1"}, {"172.20.10.128/25", "172.20.10.129"},
	} {
		if got := GatewayFromCIDR(tc.cidr); got != tc.gateway {
			t.Errorf("%s: got %s want %s", tc.cidr, got, tc.gateway)
		}
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func TestBuildComposeLocalAIStack(t *testing.T) {
	payload := BuildComposePayload{
		Addressing: AddressingConfig{Networks: []NetworkDef{
			{Name: "front", CIDR: "172.20.10.0/24", Gateway: "172.20.10.1"},
			{Name: "back", CIDR: "172.20.20.0/24", Gateway: "172.20.20.1"},
		}},
		UI: UIFlags{},
		Services: []ServiceEntry{
			{Key: "open-webui", Network: "front"},
		},
	}

	compose := BuildCompose(payload, "test-ai")
	services := compose["services"].(M)

	if _, ok := services["ollama"]; !ok {
		t.Fatal("expected ollama to be auto-added for open-webui")
	}
	if _, ok := services["open-webui"]; !ok {
		t.Fatal("expected open-webui service")
	}
	if _, ok := services["ollama-model-init"]; !ok {
		t.Fatal("expected ollama model init service")
	}

	openWebUI := services["open-webui"].(M)
	if !hasPortMapping(openWebUI, ":8080") {
		t.Fatalf("expected open-webui to expose container port 8080, got %#v", openWebUI["ports"])
	}
	nets := openWebUI["networks"].(M)
	if _, ok := nets["front"]; !ok {
		t.Fatalf("expected open-webui on front network, got %#v", nets)
	}
	if _, ok := nets["back"]; !ok {
		t.Fatalf("expected open-webui to share ollama back network, got %#v", nets)
	}

	ollama := services["ollama"].(M)
	if !hasPortMapping(ollama, ":11434") {
		t.Fatalf("expected ollama to expose API port 11434, got %#v", ollama["ports"])
	}

	initSvc := services["ollama-model-init"].(M)
	env := initSvc["environment"].(M)
	if env["OLLAMA_MODEL"] != DefaultOllamaModel {
		t.Fatalf("expected default model %q, got %#v", DefaultOllamaModel, env["OLLAMA_MODEL"])
	}
	command, _ := initSvc["command"].([]string)
	if len(command) != 2 || command[0] != "pull" || command[1] != DefaultOllamaModel {
		t.Fatalf("expected direct model pull command, got %#v", initSvc["command"])
	}

	openEnv := openWebUI["environment"].(M)
	if openEnv["WEBUI_AUTH"] != "False" {
		t.Fatalf("expected Open WebUI auth disabled for local demo, got %#v", openEnv["WEBUI_AUTH"])
	}
	if openEnv["DEFAULT_MODELS"] != DefaultOllamaModel {
		t.Fatalf("expected default Open WebUI model %q, got %#v", DefaultOllamaModel, openEnv["DEFAULT_MODELS"])
	}
}

func TestBuildComposeUptimeKumaStack(t *testing.T) {
	payload := BuildComposePayload{
		Addressing: AddressingConfig{Networks: []NetworkDef{
			{Name: "observ", CIDR: "172.20.30.0/24", Gateway: "172.20.30.1"},
		}},
		UI: UIFlags{Observ: boolPtr(true)},
		Services: []ServiceEntry{
			{Key: "uptime-kuma", Network: "observ"},
		},
	}

	compose := BuildCompose(payload, "status-test")
	services := compose["services"].(M)
	svc, ok := services["uptime-kuma"].(M)
	if !ok {
		t.Fatalf("expected uptime-kuma service, got %#v", services)
	}
	if svc["image"] != ImagesBase["uptime-kuma"] {
		t.Fatalf("expected uptime-kuma image %q, got %#v", ImagesBase["uptime-kuma"], svc["image"])
	}
	if !hasPortMapping(svc, ":3001") {
		t.Fatalf("expected uptime-kuma to expose container port 3001, got %#v", svc["ports"])
	}
	volumes := compose["volumes"].(M)
	if _, ok := volumes["uptime_kuma_data"]; !ok {
		t.Fatalf("expected uptime_kuma_data volume, got %#v", volumes)
	}
}

func TestBuildComposeDokuWikiStack(t *testing.T) {
	payload := BuildComposePayload{
		Addressing: AddressingConfig{Networks: []NetworkDef{
			{Name: "front", CIDR: "172.20.40.0/24", Gateway: "172.20.40.1"},
		}},
		UI: UIFlags{Collab: boolPtr(true)},
		Services: []ServiceEntry{
			{Key: "dokuwiki", Network: "front"},
		},
	}

	compose := BuildCompose(payload, "wiki-test")
	services := compose["services"].(M)
	svc, ok := services["dokuwiki"].(M)
	if !ok {
		t.Fatalf("expected dokuwiki service, got %#v", services)
	}
	if svc["image"] != ImagesBase["dokuwiki"] {
		t.Fatalf("expected dokuwiki image %q, got %#v", ImagesBase["dokuwiki"], svc["image"])
	}
	if !hasPortMapping(svc, ":8080") {
		t.Fatalf("expected dokuwiki to expose container port 8080, got %#v", svc["ports"])
	}
	volumes := compose["volumes"].(M)
	if _, ok := volumes["dokuwiki_data"]; !ok {
		t.Fatalf("expected dokuwiki_data volume, got %#v", volumes)
	}
}

func TestGeneratePresetConfigLocalAI(t *testing.T) {
	payload, ok := GeneratePresetConfig("monte une ia locale mistral avec une interface web")
	if !ok {
		t.Fatal("expected local AI preset to match")
	}
	services, ok := payload["services"].([]map[string]interface{})
	if !ok {
		t.Fatalf("unexpected services payload type: %T", payload["services"])
	}

	keys := map[string]bool{}
	for _, svc := range services {
		if key, _ := svc["key"].(string); key != "" {
			keys[key] = true
		}
	}
	for _, key := range []string{"portainer", "traefik", "ollama", "open-webui"} {
		if !keys[key] {
			t.Fatalf("expected preset service %q in %#v", key, keys)
		}
	}
}

func TestGeneratePresetConfigWiki(t *testing.T) {
	payload, ok := GeneratePresetConfig("je veux un wiki perso pour ma documentation")
	if !ok {
		t.Fatal("expected wiki preset to match")
	}
	services, ok := payload["services"].([]map[string]interface{})
	if !ok {
		t.Fatalf("unexpected services payload type: %T", payload["services"])
	}

	keys := map[string]bool{}
	for _, svc := range services {
		if key, _ := svc["key"].(string); key != "" {
			keys[key] = true
		}
	}
	for _, key := range []string{"portainer", "traefik", "dokuwiki"} {
		if !keys[key] {
			t.Fatalf("expected preset service %q in %#v", key, keys)
		}
	}
}

func TestGeneratePresetConfigLocalAIFastModel(t *testing.T) {
	payload, ok := GeneratePresetConfig("prebuild rapide ia locale open webui pour test")
	if !ok {
		t.Fatal("expected local AI preset to match")
	}
	services, ok := payload["services"].([]map[string]interface{})
	if !ok {
		t.Fatalf("unexpected services payload type: %T", payload["services"])
	}
	for _, svc := range services {
		if svc["key"] != "ollama" {
			continue
		}
		extra, _ := svc["extra"].(map[string]string)
		if extra["model"] != FastOllamaModel {
			t.Fatalf("expected fast model %q, got %#v", FastOllamaModel, extra["model"])
		}
		return
	}
	t.Fatal("expected ollama service")
}

func TestGenerateMagicConfigUsesLocalProviderWhenEnabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Error("local provider did not call Ollama")
		}
		fmt.Fprintf(w, `{"message":{"content":%q}}`, `{"services":[{"key":"wordpress"}]}`)
	}))
	defer server.Close()
	t.Setenv("AUTODOCKER_LOCAL_AI", "1")
	t.Setenv("LM_STUDIO_MODEL", "")
	t.Setenv("AUTODOCKER_AI_PROVIDER", "local")
	t.Setenv("LOCAL_AI_URL", server.URL)
	result, err := GenerateMagicConfig("une boutique avec catalogue produit, 8 Go RAM et 4 CPU, accès réseau privé")
	if err != nil {
		t.Fatalf("GenerateMagicConfig failed: %v", err)
	}
	if result.Source != "local" {
		t.Fatalf("expected local source, got %q", result.Source)
	}
	if result.Payload == nil {
		t.Fatal("expected payload")
	}
}

func TestLocalCopilotReplyWorksWithoutOllama(t *testing.T) {
	t.Setenv("AUTODOCKER_LOCAL_AI", "1")
	t.Setenv("LOCAL_AI_URL", "http://127.0.0.1:1")
	reply, err := CallLocalAIChat("system", "resume mon infrastructure")
	if err != nil {
		t.Fatalf("CallLocalAIChat failed: %v", err)
	}
	if !strings.Contains(reply, "Mode local actif") {
		t.Fatalf("expected local reply, got %q", reply)
	}
}

func TestGeminiModelDefaultsToFlash(t *testing.T) {
	t.Setenv("GEMINI_MODEL", "")
	if GeminiModel() != DefaultGeminiModel {
		t.Fatalf("expected Gemini default model %q, got %q", DefaultGeminiModel, GeminiModel())
	}
}

func TestGeneratePresetConfigCoursePlatform(t *testing.T) {
	payload, ok := GeneratePresetConfig("une plateforme de cours pour mes etudiants avec espace enseignant")
	if !ok {
		t.Fatal("expected course platform preset to match")
	}
	services, ok := payload["services"].([]map[string]interface{})
	if !ok {
		t.Fatalf("unexpected services payload type: %T", payload["services"])
	}
	keys := map[string]bool{}
	for _, svc := range services {
		if key, _ := svc["key"].(string); key != "" {
			keys[key] = true
		}
	}
	for _, key := range []string{"portainer", "traefik", "nginx"} {
		if !keys[key] {
			t.Fatalf("expected preset service %q in %#v", key, keys)
		}
	}
	if keys["postgres"] {
		t.Fatalf("course platform quick preset should not require postgres: %#v", keys)
	}
}

func TestGeneratePresetConfigGenericBusinessApps(t *testing.T) {
	cases := []struct {
		name     string
		prompt   string
		intent   string
		services []string
	}{
		{"crm", "un CRM pour suivre mes clients", "crm", []string{"nginx"}},
		{"reservation", "une application de reservation restaurant", "reservation", []string{"nginx"}},
		{"automation", "une plateforme automation avec n8n", "automation", []string{"nginx", "n8n"}},
		{"storage", "un portail stockage objet s3 minio", "storage", []string{"nginx", "minio"}},
		{"dev", "une forge git pour developpeurs", "dev_stack", []string{"nginx", "postgres", "gitea"}},
		{"minecraft browser", "un jeu minecraft jouable depuis une page web html js", "browser_game", []string{"nginx"}},
		{"minecraft server", "un serveur minecraft docker pour jouer avec mes amis", "game_server", []string{"nginx", "minecraft"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intent := DetectMagicIntent(tc.prompt)
			if intent.Intent != tc.intent {
				t.Fatalf("expected intent %q, got %q", tc.intent, intent.Intent)
			}
			payload, ok := GeneratePresetConfig(tc.prompt)
			if !ok {
				t.Fatal("expected preset to match")
			}
			keys := servicesInPayload(t, payload)
			for _, key := range tc.services {
				if !keys[key] {
					t.Fatalf("expected service %q in %#v", key, keys)
				}
			}
		})
	}
}

func TestGameServerPresetAvoidsInventedRCONImage(t *testing.T) {
	if !presetPreferredIntent("game_server") {
		t.Fatal("game_server must use the deterministic preset before external AI")
	}
	payload, ok := GeneratePresetConfig("j'aimerais que tu me cree un serveur minecraft docker jouable avec mes amis")
	if !ok {
		t.Fatal("expected minecraft preset")
	}
	keys := servicesInPayload(t, payload)
	if !keys["minecraft"] || !keys["nginx"] {
		t.Fatalf("expected minecraft + nginx, got %#v", keys)
	}
	for _, svc := range payload["services"].([]map[string]interface{}) {
		if svc["key"] == "rcon-web-admin" {
			t.Fatalf("unexpected invented rcon service: %#v", svc)
		}
		if svc["key"] == "minecraft" && svc["image"] != "itzg/minecraft-server:latest" {
			t.Fatalf("unexpected minecraft image: %#v", svc["image"])
		}
	}
}

func TestBrowserVoxelGamePresetIsHiddenStaticNginx(t *testing.T) {
	prompt := `Cree un jeu 3D voxel inspire de Minecraft, jouable directement dans une page web statique servie par nginx.
Pas de serveur Minecraft, pas de backend, uniquement HTML CSS JS.
Le jeu doit avoir une barre d'inventaire simple et sauvegarde locale avec localStorage.`
	intent := DetectMagicIntent(prompt)
	if intent.Intent != "browser_game" {
		t.Fatalf("expected browser_game, got %q", intent.Intent)
	}
	if !presetPreferredIntent(intent.Intent) {
		t.Fatal("browser_game must stay deterministic and hidden from external AI routing")
	}
	payload, ok := GeneratePresetConfig(prompt)
	if !ok {
		t.Fatal("expected browser game preset")
	}
	keys := servicesInPayload(t, payload)
	for _, key := range []string{"portainer", "traefik", "nginx"} {
		if !keys[key] {
			t.Fatalf("expected service %q in %#v", key, keys)
		}
	}
	if keys["ollama"] || keys["open-webui"] || keys["minecraft"] {
		t.Fatalf("browser web game must not deploy AI or Minecraft server services: %#v", keys)
	}
	provision, ok := GeneratePresetProvision(prompt, "minecra", []string{"minecra-nginx", "minecra-traefik", "minecra-portainer"})
	if !ok {
		t.Fatal("expected browser game provision")
	}
	files := provision["files"].([]interface{})
	content := files[0].(map[string]interface{})["content"].(string)
	for _, snippet := range []string{"three.min.js", "requestPointerLock", "localStorage", "MiniCraft Web"} {
		if !strings.Contains(content, snippet) {
			t.Fatalf("expected game HTML to contain %q", snippet)
		}
	}
}

func TestNormalizeMagicPayloadRepairsUnknownServices(t *testing.T) {
	payload := map[string]interface{}{
		"services": []interface{}{
			map[string]interface{}{"key": "not-a-real-service", "network": "front"},
		},
	}
	repaired, warnings, valid := NormalizeMagicPayload("un outil interne", payload)
	if !valid {
		t.Fatal("expected repaired payload to be valid")
	}
	if len(warnings) == 0 {
		t.Fatal("expected repair warnings")
	}
	keys := servicesInPayload(t, repaired)
	for _, key := range []string{"portainer", "traefik", "nginx"} {
		if !keys[key] {
			t.Fatalf("expected repaired service %q in %#v", key, keys)
		}
	}
}

func TestOpenRouterKeysRotation(t *testing.T) {
	SetOpenRouterKeys([]string{"key-a", "key-b", "key-c"})
	if OpenRouterKeyCount() != 3 {
		t.Fatalf("expected 3 OpenRouter keys, got %d", OpenRouterKeyCount())
	}
	if getNextOpenRouterKey() != "key-a" || getNextOpenRouterKey() != "key-b" || getNextOpenRouterKey() != "key-c" || getNextOpenRouterKey() != "key-a" {
		t.Fatal("expected OpenRouter key rotation")
	}
	SetOpenRouterKeys(nil)
}

func TestDetectMagicIntentReservationBeatsCRM(t *testing.T) {
	intent := DetectMagicIntent("application de reservation restaurant avec calendrier clients et tables")
	if intent.Intent != "reservation" {
		t.Fatalf("expected reservation intent, got %q", intent.Intent)
	}
}

func TestDetectMagicIntentAvoidsShortTokenFalsePositives(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{"reservation paiement", "reservation hotel avec chambres clients paiements et confirmation", "reservation"},
		{"portfolio developpeur", "portfolio developpeur avec projets Docker et competences", "portfolio"},
		{"associative not soc", "bibliotheque associative livres emprunts lecteurs retards acquisitions statistiques", "unknown"},
		{"course typo", "j'aimearis une plateformle de cours en ligne", "course_platform"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intent := DetectMagicIntent(tc.prompt)
			if intent.Intent != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, intent.Intent)
			}
		})
	}
}

func TestGeneratePresetProvisionCoursePlatform(t *testing.T) {
	payload, ok := GeneratePresetProvision("plateforme de formation pour etudiants", "magic-test", []string{"magic-test-nginx", "magic-test-postgres"})
	if !ok {
		t.Fatal("expected course platform provision preset to match")
	}
	files, ok := payload["files"].([]interface{})
	if !ok || len(files) != 1 {
		t.Fatalf("unexpected files payload: %#v", payload["files"])
	}
	file, ok := files[0].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected file payload type: %T", files[0])
	}
	content, _ := file["content"].(string)
	if !strings.Contains(content, "Plateforme etudiants") || !strings.Contains(content, "Tableau de bord") {
		t.Fatalf("expected rich course platform html, got %q", content[:minLen(len(content), 120)])
	}
}

func hasPortMapping(service M, suffix string) bool {
	ports, _ := service["ports"].([]string)
	for _, p := range ports {
		if len(p) >= len(suffix) && p[len(p)-len(suffix):] == suffix {
			return true
		}
	}
	return false
}

func servicesInPayload(t *testing.T, payload map[string]interface{}) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	switch services := payload["services"].(type) {
	case []map[string]interface{}:
		for _, svc := range services {
			key, _ := svc["key"].(string)
			keys[key] = true
		}
	case []interface{}:
		for _, raw := range services {
			svc, _ := raw.(map[string]interface{})
			key, _ := svc["key"].(string)
			keys[key] = true
		}
	default:
		t.Fatalf("unexpected services payload type: %T", payload["services"])
	}
	return keys
}

func minLen(a, b int) int {
	if a < b {
		return a
	}
	return b
}
