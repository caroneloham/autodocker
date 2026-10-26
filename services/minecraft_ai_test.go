package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestMinecraftChatActuallyConnectsModelAndGameWithPrivateRCON(t *testing.T) {
	for _, engine := range []string{"vllm", "ollama"} {
		t.Run(engine, func(t *testing.T) {
			mc := infrastructureReferenceServices["minecraft"]
			mc.Network = "app"
			model := "qwen3:4b"
			if engine == "vllm" {
				model = "Qwen/Qwen2.5-3B-Instruct"
			}
			p := BuildComposePayload{Services: []ServiceEntry{mc, {Key: "minecraft-ai", Network: "app"}, {Key: engine, Network: "app"}, {Key: "open-webui", Network: "app"}}, AIStack: &AIStackConfig{Engine: engine, Model: model, MaxContext: 8192}}
			if err := ValidateInfrastructureOptions(p); err != nil {
				t.Fatal(err)
			}
			compose := BuildCompose(p, "minecraft-chat-test")
			svcs := compose["services"].(M)
			game := svcs["minecraft"].(M)
			bridge := svcs["minecraft-ai"].(M)
			g := game["environment"].(M)
			b := bridge["environment"].(M)
			if g["EULA"] != "FALSE" || g["ENABLE_RCON"] != "TRUE" || g["RCON_PASSWORD"] != b["RCON_PASSWORD"] || len(g["RCON_PASSWORD"].(string)) < 32 {
				t.Fatal("game wiring or EULA changed incorrectly")
			}
			if bridge["ports"] != nil || strings.Contains(strings.Join(toStringList(game["ports"]), ","), "25575") {
				t.Fatal("RCON is publicly exposed")
			}
			if bridge["read_only"] != true || !strings.Contains(strings.Join(toStringList(bridge["volumes"]), ","), ":/minecraft:ro") {
				t.Fatal("bridge can write game data")
			}
			if bridge["depends_on"].(M)[engine] == nil || bridge["depends_on"].(M)["minecraft"] == nil {
				t.Fatal("startup dependency missing")
			}
			if !strings.Contains(b["AI_BASE_URL"].(string), engine) {
				t.Fatal("bridge uses wrong engine")
			}
			if engine == "vllm" {
				if b["AI_MODEL"] != "autodocker-llm" || b["AI_API_KEY"] != svcs[engine].(M)["environment"].(M)["VLLM_API_KEY"] {
					t.Fatal("wrong model alias or API key")
				}
				if !strings.Contains(strings.Join(svcs[engine].(M)["command"].([]string), " "), "--model "+model) {
					t.Fatal("vLLM does not load a model")
				}
			}
			diagram := ArchitecturePreview(compose)
			encoded, _ := json.Marshal(diagram)
			if strings.Contains(string(encoded), g["RCON_PASSWORD"].(string)) || strings.Contains(string(encoded), b["AI_API_KEY"].(string)) || strings.Contains(string(encoded), "AI_API_KEY") {
				t.Fatal("diagram leaked a credential")
			}
			found := false
			for _, svc := range diagram["services"].([]M) {
				if svc["key"] == engine {
					found = containsInfrastructureString(toStringList(svc["networks"]), "autodocker-ai")
				}
			}
			if !found {
				t.Fatal("actual private model network missing from diagram")
			}
			metadata := compose["x-autodocker"].(M)["ai"].(M)
			chatLink := false
			for _, link := range metadata["connections"].([]M) {
				if link["kind"] == "minecraft-chat" && link["to"] == "minecraft" {
					chatLink = true
				}
			}
			if !chatLink || metadata["model"] != model {
				t.Fatal("model or chat connection is not described")
			}
		})
	}
}

func TestMinecraftChatRequirementCannotBecomeUnconnectedCoHosting(t *testing.T) {
	p := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) {
		return infrastructureAnswer("minecraft", "vllm", "open-webui", "caddy"), nil
	}}
	result, err := generateInfrastructureWithProvider(context.Background(), "J’aimerais une infra IA connectée à Minecraft exposée sur Internet. Précision : via le chat elle doit interagir, HTTPS Caddy.", p)
	if err != nil {
		t.Fatal(err)
	}
	if !infrastructureKeys(result)["minecraft-ai"] {
		t.Fatal("functional connection silently omitted")
	}
	ai, ok := result.Payload["ai_stack"].(map[string]interface{})
	if !ok || ai["model"] == "" {
		t.Fatal("resolved model omitted from proposal")
	}
	encoded, _ := json.Marshal(result.DesignChecks)
	if !strings.Contains(string(encoded), "Chat Minecraft") || !strings.Contains(strings.Join(result.ManualSteps, " "), "Internet") {
		t.Fatal("integration or internet limitations hidden")
	}
	if minecraftAIChatRequested("Minecraft et vLLM séparés sans chat") {
		t.Fatal("unrequested bridge")
	}
	if infrastructurePreflightQuestion("Une IA qui interagit avec Minecraft") == "" {
		t.Fatal("chat vs player clarification missing")
	}
}

func TestVLLMTextModelExplicitlyRequestedSurvivesOmittedAIOptions(t *testing.T) {
	p := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) {
		return infrastructureAnswer("vllm", "open-webui"), nil
	}}
	result, err := generateInfrastructureWithProvider(context.Background(), "vLLM avec le modèle Qwen/Qwen2.5-7B-Instruct et Open WebUI, sans vision", p)
	if err != nil {
		t.Fatal(err)
	}
	ai := result.Payload["ai_stack"].(map[string]interface{})
	if ai["model"] != "Qwen/Qwen2.5-7B-Instruct" || ai["vision"] == true {
		t.Fatal("explicit text model lost")
	}
}

func TestMinecraftManagedRecipeUsesConfiguredFactsInExplanations(t *testing.T) {
	answer := `{"summary":"Déjà exposée en HTTPS public","warnings":["Configurer Open WebUI puis développer la passerelle RCON."],"services":[{"key":"minecraft"},{"key":"vllm"},{"key":"open-webui"},{"key":"caddy"}]}`
	p := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) { return answer, nil }}
	result, err := generateInfrastructureWithProvider(context.Background(), "Minecraft Java connecté à une IA dans le chat via vLLM, Open WebUI et Caddy sur Internet", p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Summary, "exposée") || !strings.Contains(result.Summary, "après déploiement") || strings.Contains(strings.Join(result.Warnings, " "), "développer la passerelle") {
		t.Fatal("model prose contradicts generated wiring or deployment state")
	}
	if !strings.Contains(strings.Join(result.Warnings, " "), "EULA") {
		t.Fatal("trusted manual prerequisite lost")
	}
	if managedMinecraftChatProposal(map[string]interface{}{"services": []map[string]interface{}{{"key": "minecraft-ai"}, {"key": "custom-app"}}}) {
		t.Fatal("custom service warnings must not be suppressed")
	}
}

func TestExplicitVLLMLoadedChatModelOverridesHallucinatedVision(t *testing.T) {
	answer := `{"services":[{"key":"vllm"},{"key":"open-webui"}],"ai_stack":{"engine":"vllm","model":"Qwen/Qwen2.5-VL-3B-Instruct","vision":true}}`
	p := infrastructureProvider{name: "test", complete: func(context.Context, string, string) (string, error) { return answer, nil }}
	result, err := generateInfrastructureWithProvider(context.Background(), "vLLM charge Qwen/Qwen2.5-3B-Instruct et Open WebUI pour discuter", p)
	if err != nil {
		t.Fatal(err)
	}
	ai := result.Payload["ai_stack"].(map[string]interface{})
	if ai["model"] != "Qwen/Qwen2.5-3B-Instruct" || ai["vision"] == true {
		t.Fatal("requested chat model replaced by a different vision model")
	}
}
