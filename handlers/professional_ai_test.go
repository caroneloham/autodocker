package handlers

import (
	"autodocker-go/services"
	"context"
	"encoding/json"
	"github.com/goccy/go-yaml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfessionalAIPreviewAndSaveKeepConnectionsWithoutStartingContainers(t *testing.T) {
	calls := 0
	router, root := builderTestRouter(t, func(_ context.Context, _ string, args ...string) (string, error) {
		calls++
		if len(args) < 2 || strings.Join(args[len(args)-2:], " ") != "config --quiet" {
			t.Fatal("attempted to start containers")
		}
		return "", nil
	})
	payload := services.BuildComposePayload{AIStack: &services.AIStackConfig{Engine: "vllm"}, Services: []services.ServiceEntry{{Key: "vllm", Network: "app"}, {Key: "open-webui", Network: "app"}, {Key: "anythingllm", Network: "app"}, {Key: "hermes-agent", Network: "app"}, {Key: "openclaw", Network: "app"}}, Addressing: services.AddressingConfig{Networks: []services.NetworkDef{{Name: "app", CIDR: "172.29.55.0/24", Gateway: "172.29.55.1"}}}}
	body, _ := json.Marshal(map[string]interface{}{"project": "ai-wired", "payload": payload, "deploy": false})
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s returned %d", path, w.Code)
		}
		return w
	}
	preview := request("/api/deploy/preview")
	var response struct {
		YAML     string `json:"new_yaml"`
		Metadata struct {
			AI struct {
				Version     int          `json:"version"`
				Connections []services.M `json:"connections"`
			} `json:"ai"`
		} `json:"ingress"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Metadata.AI.Version < 1 || len(response.Metadata.AI.Connections) != 8 {
		t.Fatal("preview missing actual AI connection metadata")
	}
	if _, err := os.Stat(filepath.Join(root, "ai-wired")); !os.IsNotExist(err) {
		t.Fatal("preview created files")
	}
	request("/api/deploy")
	saved, err := os.ReadFile(filepath.Join(root, "ai-wired", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var c services.M
	if err := yaml.Unmarshal(saved, &c); err != nil {
		t.Fatal(err)
	}
	// Secrets are newly generated for the saved project; compare actual topology,
	// never require preview credentials to be copied into a final deployment.
	metaRaw, _ := json.Marshal(c["x-autodocker"])
	var meta struct {
		AI struct {
			Engine      string       `json:"engine"`
			Connections []services.M `json:"connections"`
		} `json:"ai"`
	}
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.AI.Engine != "vllm" || len(meta.AI.Connections) != 8 || calls != 1 {
		t.Fatal("saved project lost topology or executed extra operations")
	}
}

func TestProfessionalAIInvalidModelRejectedBeforePreviewOrSave(t *testing.T) {
	router, root := builderTestRouter(t, func(context.Context, string, ...string) (string, error) {
		t.Fatal("invalid input reached Docker")
		return "", nil
	})
	body := `{"project":"ai-invalid","payload":{"ai_stack":{"engine":"vllm","model":"author/model;bad"},"services":[{"key":"vllm"}]},"deploy":false}`
	for _, path := range []string{"/api/deploy/preview", "/api/deploy"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal("invalid model accepted")
		}
	}
	if _, err := os.Stat(filepath.Join(root, "ai-invalid")); !os.IsNotExist(err) {
		t.Fatal("invalid request created files")
	}
}

func TestPrivatePlatformSaveIncludesPortableGuideAndAPIClient(t *testing.T) {
	router, root := builderTestRouter(t, func(context.Context, string, ...string) (string, error) { return "", nil })
	payload := services.BuildComposePayload{AIStack: &services.AIStackConfig{Engine: "vllm", Model: "Qwen/Qwen2.5-VL-3B-Instruct", Vision: true, PrivatePlatform: true, MaxContext: 8192}, Ingress: &services.IngressConfig{Proxy: "caddy", Domain: "localhost", Port: 18443, TLS: "internal"}, Services: []services.ServiceEntry{{Key: "vllm", Network: "app"}, {Key: "open-webui", Network: "app"}, {Key: "tei", Network: "app"}, {Key: "qdrant", Network: "app"}, {Key: "caddy", Network: "app", Image: "caddy:2-alpine"}}, Addressing: services.AddressingConfig{Networks: []services.NetworkDef{{Name: "app", CIDR: "172.29.57.0/24", Gateway: "172.29.57.1"}}}}
	data, _ := json.Marshal(map[string]interface{}{"project": "private-platform", "payload": payload, "deploy": false})
	request := httptest.NewRequest(http.MethodPost, "/api/deploy", strings.NewReader(string(data)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("save returned %d", response.Code)
	}
	for _, name := range []string{"GUIDE-IA.md", "ia-api.py"} {
		content, err := os.ReadFile(filepath.Join(root, "private-platform", name))
		if err != nil || len(content) < 100 {
			t.Fatalf("missing project file %s", name)
		}
	}
	guide, _ := os.ReadFile(filepath.Join(root, "private-platform", "GUIDE-IA.md"))
	if !strings.Contains(string(guide), "https://open-webui.localhost:18443/api") {
		t.Fatal("guide differs from actual HTTPS route")
	}
}
