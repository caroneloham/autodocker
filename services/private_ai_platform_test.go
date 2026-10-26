package services

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"github.com/goccy/go-yaml"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrivateAIProposalRetainsVisionCapacityTLSAndAccountPolicy(t *testing.T) {
	answer := `{"services":[{"key":"vllm","image":"vllm/vllm-openai:latest"},{"key":"open-webui","image":"ghcr.io/open-webui/open-webui:main"},{"key":"tei","image":"ghcr.io/huggingface/text-embeddings-inference:cpu-1.9"},{"key":"qdrant","image":"qdrant/qdrant:v1.15"},{"key":"caddy","image":"caddy:2-alpine"}],"ai_stack":{"engine":"vllm","model":"Qwen/Qwen2.5-VL-3B-Instruct","vision":true,"private_platform":true,"max_context":8192,"concurrency":2},"ingress":{"proxy":"caddy","domain":"localhost","port":8443,"tls":"internal"}}`
	provider := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) { return answer, nil }}
	result, err := generateInfrastructureWithProvider(context.Background(), "Une plateforme IA privée pour plusieurs utilisateurs avec vLLM, vision, Open WebUI, TEI, Qdrant et Caddy en HTTPS privé", provider)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result.Payload)
	var payload BuildComposePayload
	_ = json.Unmarshal(raw, &payload)
	if payload.AIStack == nil || !payload.AIStack.Vision || !payload.AIStack.PrivatePlatform || payload.Ingress == nil || payload.Ingress.TLS != "internal" {
		t.Fatal("AI proposal lost explicit functional options")
	}
	if err := ValidateInfrastructureOptions(payload); err != nil {
		t.Fatal(err)
	}
	c := BuildCompose(payload, "proposed-private")
	if len(PrivateAIProjectFiles(c)) != 2 {
		t.Fatal("AI proposal has no API kit")
	}
}

func TestPrivateAIExplicitRequirementsSurviveProvidersOmittingOptionalFields(t *testing.T) {
	answer := `{"services":[{"key":"vllm","image":"vllm/vllm-openai:latest"},{"key":"open-webui","image":"ghcr.io/open-webui/open-webui:main"},{"key":"tei","image":"ghcr.io/huggingface/text-embeddings-inference:cpu-1.9"},{"key":"qdrant","image":"qdrant/qdrant:v1.15"},{"key":"caddy","image":"caddy:2-alpine"}]}`
	for _, name := range []string{"lmstudio", "ollama", "compatible"} {
		t.Run(name, func(t *testing.T) {
			provider := infrastructureProvider{name: name, complete: func(context.Context, string, string) (string, error) { return answer, nil }}
			result, err := generateInfrastructureWithProvider(context.Background(), "Une plateforme IA privée avec vLLM avec Qwen/Qwen2.5-VL-3B-Instruct, vision, plusieurs utilisateurs, Open WebUI, TEI, Qdrant et Caddy en HTTPS privé sur le port 8443. Contexte 8192, 4 requêtes simultanées.", provider)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(result.Payload)
			var payload BuildComposePayload
			_ = json.Unmarshal(raw, &payload)
			if payload.AIStack == nil || payload.AIStack.Model != "Qwen/Qwen2.5-VL-3B-Instruct" || !payload.AIStack.Vision || !payload.AIStack.PrivatePlatform || payload.AIStack.Concurrency != 4 || payload.AIStack.MaxContext != 8192 || payload.Ingress == nil || payload.Ingress.Port != 8443 || payload.Ingress.TLS != "internal" {
				t.Fatal("explicit requirements were lost")
			}
		})
	}
}

func TestPrivateAIVisionRequirementsDistinguishContainerImagesAndRespectExclusions(t *testing.T) {
	for text, want := range map[string]bool{"provisionner vllm avec une image docker": false, "vllm avec une image vllm vllm openai": false, "vllm pour comprendre les images": true, "vllm multimodal": true, "vllm sans vision": false} {
		if infrastructureVisionRequested(text) != want {
			t.Fatalf("wrong vision requirement for %q", text)
		}
	}
	payload := privatePlatformPayload()
	normalizeProfessionalAIIntent("vllm sans vision", &payload)
	if payload.AIStack.Vision {
		t.Fatal("explicit exclusion ignored")
	}
}

func TestPrivateAILiveQwenDesign(t *testing.T) {
	if os.Getenv("AUTODOCKER_QWEN_LIVE") != "1" {
		t.Skip("LM Studio live test is opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 140*time.Second)
	defer cancel()
	provider := infrastructureProvider{name: "lmstudio-qwen", complete: func(ctx context.Context, system, message string) (string, error) {
		text, err := requestInfrastructureModel(ctx, "http://127.0.0.1:1234/v1/chat/completions", "qwen3.5-9b", "", "lmstudio", system, message)
		if err == nil {
			raw, parseErr := parseAIConfigText(text)
			if parseErr == nil {
				keys := []string{}
				for _, service := range normalizeServiceList(raw["services"]) {
					key, _ := service["key"].(string)
					keys = append(keys, key)
				}
				t.Logf("Qwen proposed service keys: %v", keys)
				names := []string{}
				for key := range raw {
					names = append(names, key)
				}
				t.Logf("Qwen root fields: %v; AI options: %v", names, raw["ai_stack"])
			}
		}
		return text, err
	}}
	result, err := generateInfrastructureWithProvider(ctx, "Crée une plateforme IA privée pour plusieurs utilisateurs. Je veux vLLM avec Qwen/Qwen2.5-VL-3B-Instruct pour comprendre les images, Open WebUI avec comptes et clés API personnelles, TEI et Qdrant pour les documents, et Caddy en HTTPS privé local sur le port 8443. Contexte 8192 tokens, deux requêtes simultanées. Utilise les profils intégrés, ai_stack et ingress pour les réglages.", provider)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result.Payload)
	var payload BuildComposePayload
	_ = json.Unmarshal(raw, &payload)
	if payload.AIStack == nil || !payload.AIStack.Vision || !payload.AIStack.PrivatePlatform || payload.Ingress == nil || payload.Ingress.TLS != "internal" {
		t.Fatal("Qwen did not retain private vision platform settings")
	}
	c := BuildCompose(payload, "qwen-private")
	if len(PrivateAIProjectFiles(c)) != 2 {
		t.Fatal("API kit missing")
	}
	t.Logf("Qwen private platform validated: %d services, vision and private HTTPS retained", len(c["services"].(M)))
}

func privatePlatformPayload() BuildComposePayload {
	return BuildComposePayload{AIStack: &AIStackConfig{Engine: "vllm", Model: "Qwen/Qwen2.5-VL-3B-Instruct", Vision: true, PrivatePlatform: true, MaxContext: 8192, Concurrency: 2, GPUMemory: 0.85}, Ingress: &IngressConfig{Proxy: "caddy", Domain: "localhost", Port: 18443, TLS: "internal"}, Services: []ServiceEntry{{Key: "vllm", Network: "app"}, {Key: "open-webui", Network: "app"}, {Key: "tei", Network: "app"}, {Key: "qdrant", Network: "app"}, {Key: "caddy", Network: "app", Image: "caddy:2-alpine"}}, Addressing: AddressingConfig{Networks: []NetworkDef{{Name: "app", CIDR: "172.29.56.0/24", Gateway: "172.29.56.1"}}}}
}

func TestPrivateAIPlatformVisionUsersHTTPSAndAPIUseActualConfiguration(t *testing.T) {
	p := privatePlatformPayload()
	if err := ValidateInfrastructureOptions(p); err != nil {
		t.Fatal(err)
	}
	c := BuildCompose(p, "private-ia")
	svcs := c["services"].(M)
	meta := c["x-autodocker"].(M)
	cmd := strings.Join(toStringList(svcs["vllm"].(M)["command"]), " ")
	for _, flag := range []string{"Qwen/Qwen2.5-VL-3B-Instruct", "--max-model-len 8192", "--max-num-seqs 2", "--limit-mm-per-prompt", "--mm-processor-kwargs"} {
		if !strings.Contains(cmd, flag) {
			t.Fatal("missing vision/capacity setting")
		}
	}
	env := svcs["open-webui"].(M)["environment"].(M)
	for key, want := range map[string]string{"ENABLE_SIGNUP": "False", "WEBUI_AUTH": "True", "ENABLE_API_KEYS": "True", "USER_PERMISSIONS_FEATURES_API_KEYS": "True", "ENABLE_API_KEYS_ENDPOINT_RESTRICTIONS": "True", "VECTOR_DB": "qdrant", "WEBUI_SESSION_COOKIE_SECURE": "True"} {
		if env[key] != want {
			t.Fatalf("incorrect %s", key)
		}
	}
	if env["WEBUI_ADMIN_EMAIL"] != "admin@autodocker.local" || len(env["WEBUI_ADMIN_PASSWORD"].(string)) < 24 {
		t.Fatal("admin bootstrap not configured")
	}
	ingress := meta["ingress"].(M)
	if svcs["open-webui"].(M)["healthcheck"] == nil || svcs["caddy"].(M)["depends_on"].(M)["open-webui"].(M)["condition"] != "service_healthy" {
		t.Fatal("gateway does not wait for the authenticated UI to be ready")
	}
	if ingress["protocol"] != "https" || ingress["tls"] != "internal" {
		t.Fatal("TLS metadata missing")
	}
	var proxy M
	if err := json.Unmarshal([]byte(svcs["caddy"].(M)["environment"].(M)["AUTODOCKER_PROXY_CONFIG"].(string)), &proxy); err != nil {
		t.Fatal(err)
	}
	apps := proxy["apps"].(map[string]interface{})
	if apps["tls"] == nil {
		t.Fatal("Caddy has no issuer")
	}
	if len(toStringList(svcs["open-webui"].(M)["ports"])) != 0 || len(toStringList(svcs["vllm"].(M)["ports"])) != 0 {
		t.Fatal("private APIs were published directly")
	}
	files := PrivateAIProjectFiles(c)
	if !strings.Contains(files["GUIDE-IA.md"], env["WEBUI_URL"].(string)+"/api") || !strings.Contains(files["ia-api.py"], "image_url") {
		t.Fatal("API kit targets a different URL or lacks vision")
	}
	for _, file := range files {
		if strings.Contains(file, env["WEBUI_ADMIN_PASSWORD"].(string)) || strings.Contains(file, env["OPENAI_API_KEY"].(string)) {
			t.Fatal("API kit embeds an infrastructure credential")
		}
	}
	path := filepath.Join(t.TempDir(), "compose.yaml")
	raw, _ := yaml.Marshal(c)
	_ = os.WriteFile(path, raw, 0600)
	if _, err := exec.LookPath("docker"); err == nil {
		if out, err := exec.Command("docker", "compose", "-p", "private-ai-test", "-f", path, "config", "--quiet").CombinedOutput(); err != nil {
			t.Fatalf("Compose invalid: %s %v", out, err)
		}
	}
}

func TestPrivateAIPlatformRejectsUnsupportedCapacityAndTLS(t *testing.T) {
	for _, change := range []func(*BuildComposePayload){func(p *BuildComposePayload) { p.AIStack.MaxContext = -1 }, func(p *BuildComposePayload) { p.AIStack.Concurrency = 100 }, func(p *BuildComposePayload) { p.AIStack.GPUMemory = 1.5 }, func(p *BuildComposePayload) { p.AIStack.AdminEmail = "bad\nmail" }, func(p *BuildComposePayload) { p.Ingress.Proxy = "traefik" }, func(p *BuildComposePayload) { p.Ingress.TLS = "fake" }, func(p *BuildComposePayload) { p.AIStack.Model = "Qwen/Qwen2.5-3B-Instruct" }} {
		p := privatePlatformPayload()
		change(&p)
		if ValidateInfrastructureOptions(p) == nil {
			t.Fatal("invalid private profile accepted")
		}
	}
}

func TestPrivateAIPlatformCaddyJSONValidatesWithCachedOfficialImage(t *testing.T) {
	if os.Getenv("AUTODOCKER_CADDY_VALIDATE") != "1" {
		t.Skip("container-side validation is opt-in")
	}
	if exec.Command("docker", "image", "inspect", "caddy:2-alpine").Run() != nil {
		t.Skip("cached official image unavailable")
	}
	c := BuildCompose(privatePlatformPayload(), "private-caddy-check")
	content := c["services"].(M)["caddy"].(M)["environment"].(M)["AUTODOCKER_PROXY_CONFIG"].(string)
	path := filepath.Join(t.TempDir(), "caddy.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", "run", "--rm", "--network", "none", "--mount", "type=bind,source="+path+",target=/etc/caddy/check.json,readonly", "caddy:2-alpine", "caddy", "validate", "--config", "/etc/caddy/check.json")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Caddy rejects generated HTTPS config: %s %v", out, err)
	}
}

func TestPrivateAIAPIClientUsesPersonalKeyImageSchemaAndVerifiedTLS(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("Python unavailable")
	}
	directory := t.TempDir()
	script := filepath.Join(directory, "ia-api.py")
	_ = os.WriteFile(script, []byte(privateAIClient), 0600)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/chat/completions" || r.Header.Get("Authorization") != "Bearer user-test-key" {
			t.Error("client targets wrong route or credential")
		}
		var body M
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "autodocker-llm" {
			t.Error("incorrect served model")
		}
		messages := body["messages"].([]interface{})
		content := messages[0].(map[string]interface{})["content"]
		if _, ok := content.([]interface{}); !ok {
			t.Error("image was not encoded as multimodal content")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Image reçue"}}]}`))
	}))
	defer server.Close()
	cert, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(directory, "ca.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600)
	picture := filepath.Join(directory, "image.png")
	var pngData bytes.Buffer
	_ = png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 32, 32)))
	_ = os.WriteFile(picture, pngData.Bytes(), 0600)
	cmd := exec.Command(python, "-X", "utf8", script, "Décris cette image", "--image", picture)
	cmd.Env = append(os.Environ(), "AUTODOCKER_API_BASE_URL="+server.URL+"/api", "AUTODOCKER_API_KEY=user-test-key", "AUTODOCKER_CA_FILE="+ca)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Image reçue") || calls.Load() != 1 {
		t.Fatalf("API client failed: %s %v", out, err)
	}
	// Removing the trusted CA must fail before the request reaches the service.
	cmd = exec.Command(python, "-X", "utf8", script, "test")
	cmd.Env = append(os.Environ(), "AUTODOCKER_API_BASE_URL="+server.URL+"/api", "AUTODOCKER_API_KEY=user-test-key", "AUTODOCKER_CA_FILE=")
	if cmd.Run() == nil || calls.Load() != 1 {
		t.Fatal("client bypassed TLS verification")
	}
}
