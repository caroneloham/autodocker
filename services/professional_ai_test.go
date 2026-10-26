package services

import (
	"context"
	"encoding/json"
	"github.com/goccy/go-yaml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func aiTestPayload(engine string) BuildComposePayload {
	p := BuildComposePayload{AIStack: &AIStackConfig{Engine: engine}, Addressing: AddressingConfig{Networks: []NetworkDef{{Name: "app", CIDR: "172.29.55.0/24", Gateway: "172.29.55.1"}}}, Services: []ServiceEntry{{Key: "open-webui", Network: "app"}, {Key: "anythingllm", Network: "app"}, {Key: "hermes-agent", Network: "app"}, {Key: "openclaw", Network: "app"}}}
	if engine == "vllm" {
		p.Services = append(p.Services, ServiceEntry{Key: "vllm", Network: "app"})
	}
	return p
}

func TestProfessionalAIConnectsActualClientsWithMatchingModelsAndCredentials(t *testing.T) {
	for _, engine := range []string{"vllm", "ollama"} {
		t.Run(engine, func(t *testing.T) {
			p := aiTestPayload(engine)
			if err := ValidateInfrastructureOptions(p); err != nil {
				t.Fatal(err)
			}
			c := BuildCompose(p, "ai-connections")
			svcs := c["services"].(M)
			meta := c["x-autodocker"].(M)["ai"].(M)
			model := meta["served_model"].(string)
			base := "http://" + engine + map[string]string{"vllm": ":8000/v1", "ollama": ":11434/v1"}[engine]
			if engine == "vllm" {
				if svcs["ollama"] != nil || svcs["ollama-model-init"] != nil {
					t.Fatal("vLLM acquired an unwanted Ollama runtime")
				}
				if svcs[engine].(M)["deploy"] == nil {
					t.Fatal("GPU reservation missing")
				}
			}
			llmEnv := svcs["anythingllm"].(M)["environment"].(M)
			web := svcs["open-webui"].(M)["environment"].(M)
			if llmEnv["GENERIC_OPEN_AI_BASE_PATH"] != base || llmEnv["GENERIC_OPEN_AI_MODEL_PREF"] != model || web["DEFAULT_MODELS"] != model {
				t.Fatal("clients target a different model or endpoint")
			}
			if engine == "vllm" && (web["OPENAI_API_KEY"] != svcs[engine].(M)["environment"].(M)["VLLM_API_KEY"] || web["OPENAI_API_KEY"] != llmEnv["GENERIC_OPEN_AI_API_KEY"]) {
				t.Fatal("inference API keys differ")
			}
			if web["WEBUI_AUTH"] != "True" {
				t.Fatal("UI authentication disabled")
			}
			qKey := svcs["qdrant"].(M)["environment"].(M)["QDRANT__SERVICE__API_KEY"]
			if qKey == "" || qKey != llmEnv["QDRANT_API_KEY"] || qKey != web["QDRANT_API_KEY"] {
				t.Fatal("vector credentials differ")
			}
			eKey := svcs["tei"].(M)["environment"].(M)["API_KEY"]
			if eKey == "" || eKey != llmEnv["GENERIC_OPEN_AI_EMBEDDING_API_KEY"] || eKey != web["RAG_OPENAI_API_KEY"] {
				t.Fatal("embedding credentials differ")
			}
			for _, key := range []string{engine, "qdrant", "tei"} {
				if len(toStringList(svcs[key].(M)["ports"])) != 0 {
					t.Fatalf("private backend %s exposes host ports", key)
				}
			}
			configs := c["configs"].(M)
			var hermes, claw M
			if err := json.Unmarshal([]byte(configs["hermes-agent_config"].(M)["content"].(string)), &hermes); err != nil {
				t.Fatal(err)
			}
			if hermes["model"].(map[string]interface{})["base_url"] != base || hermes["model"].(map[string]interface{})["model"] != model {
				t.Fatal("Hermes provider not wired")
			}
			if err := json.Unmarshal([]byte(configs["openclaw_config"].(M)["content"].(string)), &claw); err != nil {
				t.Fatal(err)
			}
			gateway := claw["gateway"].(map[string]interface{})
			if gateway["auth"].(map[string]interface{})["token"] != svcs["openclaw"].(M)["environment"].(M)["OPENCLAW_GATEWAY_TOKEN"] {
				t.Fatal("OpenClaw token differs")
			}
			if claw["agents"].(map[string]interface{})["defaults"].(map[string]interface{})["model"].(map[string]interface{})["primary"] != engine+"/"+model {
				t.Fatal("OpenClaw model differs")
			}
			// All rendered integration edges must really exist on a shared network and
			// have startup ordering. A shared bridge alone does not prove integration.
			for _, edge := range meta["connections"].([]M) {
				from, to := edge["from"].(string), edge["to"].(string)
				a, b := svcs[from].(M), svcs[to].(M)
				if a["networks"].(M)["autodocker-ai"] == nil || b["networks"].(M)["autodocker-ai"] == nil {
					t.Fatal("unreachable connection")
				}
				if a["depends_on"].(M)[to] == nil {
					t.Fatal("missing readiness dependency")
				}
			}
			for key, value := range svcs {
				svc := value.(M)
				for _, bind := range toStringList(svc["volumes"]) {
					if strings.Contains(bind, "docker.sock") {
						t.Fatal("agent has host Docker access")
					}
				}
				if svc["privileged"] != nil {
					t.Fatal("privileged AI service")
				}
				if strings.HasSuffix(key, "-autodocker-init") && svc["network_mode"] != "none" {
					t.Fatal("bootstrap has network access")
				}
			}
			// Compose parses the actual artifact, with inline configs and GPU reservation.
			if _, err := exec.LookPath("docker"); err == nil {
				raw, _ := yaml.Marshal(c)
				path := filepath.Join(t.TempDir(), "compose.yaml")
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("docker", "compose", "-p", "ai-check", "-f", path, "config", "--quiet")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("invalid Compose: %s %v", out, err)
				}
			}
		})
	}
}

func TestProfessionalAIIngressPreservesMetadataAndKeepsInferencePrivate(t *testing.T) {
	p := aiTestPayload("vllm")
	p.Ingress = &IngressConfig{Proxy: "caddy", Domain: "localhost", Port: 18088}
	p.Services = append(p.Services, ServiceEntry{Key: "caddy", Image: "caddy:2-alpine", Network: "app"})
	c := BuildCompose(p, "ai-ingress")
	meta := c["x-autodocker"].(M)
	if meta["ai"] == nil || meta["ingress"] == nil {
		t.Fatal("ingress erased AI metadata")
	}
	routes := meta["ingress"].(M)["routes"].([]M)
	seen := map[string]bool{}
	for _, r := range routes {
		seen[r["service"].(string)] = true
	}
	if !seen["anythingllm"] || !seen["open-webui"] || seen["qdrant"] || seen["tei"] || seen["vllm"] {
		t.Fatal("incorrect public/private AI routes")
	}
	for _, key := range []string{"anythingllm", "hermes-agent", "open-webui"} {
		if len(toStringList(c["services"].(M)[key].(M)["ports"])) > 0 {
			t.Fatal("proxy application retains direct published port")
		}
	}
}

func TestProfessionalAIRejectsInvalidModelsAndConflictingRuntime(t *testing.T) {
	for _, model := range []string{"../model", "https://huggingface.co/Qwen/model", "author/model;touch-file", "author/model-GGUF", "author/model/file.gguf"} {
		p := aiTestPayload("vllm")
		p.AIStack.Model = model
		if ValidateInfrastructureOptions(p) == nil {
			t.Fatal("unsafe or incompatible model accepted")
		}
	}
	p := aiTestPayload("vllm")
	p.AIStack.Engine = "ollama"
	if ValidateInfrastructureOptions(p) == nil {
		t.Fatal("conflicting runtime accepted")
	}
}

func TestProfessionalAIStandaloneEmbeddingsDoNotClaimAnInferenceRuntime(t *testing.T) {
	p := BuildComposePayload{Services: []ServiceEntry{{Key: "tei"}}}
	c := BuildCompose(p, "embedding-only")
	meta := c["x-autodocker"].(M)["ai"].(M)
	if meta["engine"] != "" || meta["served_model"] != "" || c["services"].(M)["ollama"] != nil {
		t.Fatal("embedding-only stack claims a phantom LLM")
	}
	p.Services = []ServiceEntry{{Key: "open-webui"}, {Key: "qdrant"}}
	c = BuildCompose(p, "rag-webui")
	if c["services"].(M)["tei"] == nil || c["services"].(M)["open-webui"].(M)["environment"].(M)["VECTOR_DB"] != "qdrant" {
		t.Fatal("Open WebUI RAG was not wired")
	}
}

func TestProfessionalAIProposalUsesSelectedEngineWithoutPhantomDependencies(t *testing.T) {
	answer := `{"services":[{"key":"vllm"},{"key":"open-webui"},{"key":"anythingllm"},{"key":"hermes-agent"},{"key":"openclaw"}]}`
	provider := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) { return answer, nil }}
	result, err := generateInfrastructureWithProvider(context.Background(), "vllm avec open-webui, anythingllm, hermes-agent et openclaw", provider)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result.Payload)
	var p BuildComposePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Services {
		if s.Key == "ollama" {
			t.Fatal("proposal added unwanted Ollama")
		}
	}
	c := BuildCompose(p, "ai-proposal")
	if c["services"].(M)["tei"] == nil || c["services"].(M)["qdrant"] == nil {
		t.Fatal("proposal lost RAG dependencies")
	}
}

func TestProfessionalAILiveQwenDesign(t *testing.T) {
	if os.Getenv("AUTODOCKER_QWEN_LIVE") != "1" {
		t.Skip("LM Studio live test is opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 140*time.Second)
	defer cancel()
	p := infrastructureProvider{name: "lmstudio-qwen", complete: func(ctx context.Context, system, prompt string) (string, error) {
		model := os.Getenv("AUTODOCKER_QWEN_TEST_MODEL")
		if model == "" {
			model = "qwen3.5-9b"
		}
		return requestInfrastructureModel(ctx, "http://127.0.0.1:1234/v1/chat/completions", model, "", "lmstudio", system, prompt)
	}}
	result, err := generateInfrastructureWithProvider(ctx, "Une infrastructure IA avec vLLM, Open WebUI, AnythingLLM pour mes documents, Hermes Agent et OpenClaw. Utilise les profils intégrés et leurs connexions internes. Pas de modèle externe payant.", p)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result.Payload)
	var payload BuildComposePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Services) == 0 {
		t.Fatal("Qwen did not propose an architecture")
	}
	if err := ValidateInfrastructureOptions(payload); err != nil {
		t.Fatal(err)
	}
	c := BuildCompose(payload, "qwen-ai-pro")
	meta, ok := c["x-autodocker"].(M)
	if !ok || meta["ai"] == nil {
		t.Fatal("Qwen proposal has no verified AI integration")
	}
	t.Logf("Qwen proposal validated: %d services, %d internal connections", len(c["services"].(M)), len(meta["ai"].(M)["connections"].([]M)))
}
