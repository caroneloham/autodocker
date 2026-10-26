package services

import (
	"encoding/json"
	"strings"
	"testing"
)

func proPayload(proxy string) BuildComposePayload {
	image := map[string]string{"caddy": "caddy:2-alpine", "haproxy": "haproxy:3.0-alpine"}[proxy]
	return BuildComposePayload{Ingress: &IngressConfig{Proxy: proxy, Domain: "localhost", Port: 18088}, Services: []ServiceEntry{
		{Key: proxy, Image: image, Network: "front", Ports: []string{"18088:80"}},
		{Key: "wordpress", Network: "front"}, {Key: "mariadb", Network: "back"},
		{Key: "grafana", Network: "observ"}, {Key: "minecraft", Image: "itzg/minecraft-server:latest", Network: "back", Ports: []string{"25565:25565"}},
	}, Addressing: AddressingConfig{Networks: []NetworkDef{{Name: "front", CIDR: "172.28.10.0/24", Gateway: "172.28.10.1"}, {Name: "back", CIDR: "172.28.11.0/24", Gateway: "172.28.11.1"}, {Name: "observ", CIDR: "172.28.12.0/24", Gateway: "172.28.12.1"}}}}
}

func TestProGatewaysRouteActualHTTPServicesAndRemoveTheirDirectPorts(t *testing.T) {
	for _, key := range []string{"caddy", "traefik", "haproxy"} {
		t.Run(key, func(t *testing.T) {
			payload := proPayload(key)
			if err := ValidateInfrastructureOptions(payload); err != nil {
				t.Fatal(err)
			}
			c := BuildCompose(payload, "pro-test")
			svcs := c["services"].(M)
			proxy := svcs[key].(M)
			config := proxy["environment"].(M)["AUTODOCKER_PROXY_CONFIG"].(string)
			for _, expected := range []string{"wordpress.localhost", "grafana.localhost", "wordpress:80", "grafana:3000"} {
				if !strings.Contains(config, expected) {
					t.Errorf("missing %s in %s", expected, config)
				}
			}
			if strings.Contains(config, "mariadb:") || strings.Contains(config, "minecraft:") {
				t.Fatal("non-HTTP protocol was routed through HTTP")
			}
			for _, app := range []string{"wordpress", "grafana"} {
				if len(toStringList(svcs[app].(M)["ports"])) != 0 {
					t.Fatalf("direct HTTP exposure remains for %s", app)
				}
			}
			if len(toStringList(svcs["minecraft"].(M)["ports"])) != 1 {
				t.Fatal("game protocol was removed")
			}
			if proxy["networks"].(M)["observ"] == nil {
				t.Fatal("proxy cannot reach app on other network")
			}
			if proxy["networks"].(M)["back"] != nil {
				t.Fatal("proxy unnecessarily attached to database/game network")
			}
			if len(toStringList(proxy["ports"])) != 1 {
				t.Fatal("gateway has duplicate ports")
			}
			if key == "traefik" && proxy["volumes"] != nil {
				t.Fatal("file-based Traefik retained Docker socket")
			}
			command := strings.Join(toStringList(proxy["command"]), " ")
			if !strings.Contains(command, "$$AUTODOCKER_PROXY_CONFIG") {
				t.Fatal("Compose interpolation consumes runtime config variable")
			}
			ingress := c["x-autodocker"].(M)["ingress"].(M)
			if len(ingress["routes"].([]M)) != 2 {
				t.Fatal("metadata does not describe actual routes")
			}
			if key != "haproxy" {
				var parsed M
				if err := json.Unmarshal([]byte(config), &parsed); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestProGatewayValidationRejectsAmbiguousOrInjectedSettings(t *testing.T) {
	for _, domain := range []string{"example.com; touch bad", "https://example.com", "local\nbackend hacked", "a/../../b", "-bad.localhost"} {
		p := proPayload("caddy")
		p.Ingress.Domain = domain
		if ValidateInfrastructureOptions(p) == nil {
			t.Errorf("accepted %q", domain)
		}
	}
	p := proPayload("caddy")
	p.Services = append(p.Services, ServiceEntry{Key: "traefik"})
	if ValidateInfrastructureOptions(p) == nil {
		t.Fatal("accepted two gateways")
	}
	p = proPayload("caddy")
	p.Ingress.Port = 80
	if ValidateInfrastructureOptions(p) == nil {
		t.Fatal("accepted unsupported privileged host port")
	}
	p = proPayload("caddy")
	p.Ingress.Proxy = "shell"
	if ValidateInfrastructureOptions(p) == nil {
		t.Fatal("accepted unknown proxy")
	}
}

func TestSelectedHuggingFaceModelReachesDownloadAndOpenWebUI(t *testing.T) {
	model := "hf.co/bartowski/Llama-3.2-1B-Instruct-GGUF:Llama-3.2-1B-Instruct-Q4_K_M.gguf"
	p := BuildComposePayload{Services: []ServiceEntry{{Key: "ollama", Extra: map[string]string{"model": model}}, {Key: "open-webui", Extra: map[string]string{"model": model}, Env: map[string]string{"DEFAULT_MODELS": "stale-ai-choice", "WEBUI_AUTH": "False"}}}}
	if err := ValidateInfrastructureOptions(p); err != nil {
		t.Fatal(err)
	}
	c := BuildCompose(p, "hf-test")
	svcs := c["services"].(M)
	env := svcs["open-webui"].(M)["environment"].(M)
	if env["DEFAULT_MODELS"] != model || env["DEFAULT_PINNED_MODELS"] != model {
		t.Fatalf("model/auth overridden: %#v", env)
	}
	init := svcs["ollama-model-init"].(M)
	cmd := toStringList(init["command"])
	if len(cmd) != 2 || cmd[0] != "pull" || cmd[1] != model {
		t.Fatalf("incorrect download command: %#v", cmd)
	}
	for _, bad := range []string{"model;curl example.com", "qwen\nwhoami", "--help", "../model"} {
		p.Services[0].Extra["model"] = bad
		if ValidateInfrastructureOptions(p) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestNoGatewayKeepsExistingPorts(t *testing.T) {
	p := proPayload("caddy")
	p.Ingress = nil
	c := BuildCompose(p, "classic")
	svcs := c["services"].(M)
	if len(toStringList(svcs["wordpress"].(M)["ports"])) == 0 {
		t.Fatal("HTTP port removed without selecting automatic routing")
	}
	if c["x-autodocker"] != nil {
		t.Fatal("invented ingress without configuration")
	}
}

func TestProAssistantKeepsOllamaInternalAndEnablesWebUIAuthentication(t *testing.T) {
	p := proPayload("caddy")
	p.Services = append(p.Services, ServiceEntry{Key: "open-webui", Network: "front", Extra: map[string]string{"model": "qwen3:4b"}})
	c := BuildCompose(p, "private-assistant")
	svcs := c["services"].(M)
	if len(toStringList(svcs["ollama"].(M)["ports"])) != 0 {
		t.Fatal("private model API is directly published")
	}
	env := svcs["open-webui"].(M)["environment"].(M)
	if env["WEBUI_AUTH"] != "True" || env["ENABLE_LOGIN_FORM"] != "True" {
		t.Fatal("proxied assistant does not require login")
	}
	init := svcs["ollama-model-init"].(M)
	if toStringList(init["command"])[1] != "qwen3:4b" {
		t.Fatal("selected model not pulled")
	}
}
