package services

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Fail closed on broken delivered topology. Messages expose only service names,
// never connection strings or credentials supplied by users or models.
func ValidateGeneratedConnections(compose M) error {
	svcs, _ := compose["services"].(M)
	for key, raw := range svcs {
		svc, ok := raw.(M)
		if !ok {
			return fmt.Errorf("Service %s invalide", key)
		}
		deps := toStringList(svc["depends_on"])
		if entries, ok := svc["depends_on"].(M); ok {
			deps = nil
			for target := range entries {
				deps = append(deps, target)
			}
		}
		for _, target := range deps {
			if _, exists := svcs[target]; !exists {
				return fmt.Errorf("%s : dépendance %s absente", key, target)
			}
		}
		env, _ := svc["environment"].(M)
		for _, name := range []string{"WORDPRESS_DB_HOST", "MYSQL_HOST", "GITEA__database__HOST", "REDIS_HOST", "OLLAMA_BASE_URL", "OPENAI_API_BASE_URL", "QDRANT_URI", "RAG_OPENAI_API_BASE_URL", "GENERIC_OPEN_AI_BASE_PATH", "EMBEDDING_BASE_PATH", "QDRANT_ENDPOINT"} {
			value, _ := env[name].(string)
			if value == "" {
				continue
			}
			host := value
			if strings.Contains(value, "://") {
				parsed, err := url.Parse(value)
				if err != nil {
					return fmt.Errorf("%s : adresse interne %s invalide", key, name)
				}
				host = parsed.Hostname()
			} else if h, _, err := net.SplitHostPort(value); err == nil {
				host = h
			}
			backend, exists := svcs[host].(M)
			if !exists {
				if privateGeneratedServices[host] {
					return fmt.Errorf("%s : service interne %s absent", key, host)
				}
				continue
			} // Explicit external endpoints are outside this stack.
			if applicationUsesNetworkMode(svc) || applicationUsesNetworkMode(backend) {
				return fmt.Errorf("%s : réseau incompatible avec %s", key, host)
			}
			shared := false
			for network := range applicationNetworkMap(svc) {
				if _, ok := applicationNetworkMap(backend)[network]; ok {
					shared = true
				}
			}
			if !shared {
				return fmt.Errorf("%s et %s : aucun réseau commun", key, host)
			}
			if !containsInfrastructureString(deps, host) {
				return fmt.Errorf("%s : dépendance applicative %s non déclarée", key, host)
			}
		}
	}
	if web, ok := svcs["open-webui"].(M); ok {
		env, _ := web["environment"].(M)
		if engine, ok := svcs["vllm"].(M); ok && env["OPENAI_API_BASE_URL"] == "http://vllm:8000/v1" {
			key, _ := engine["environment"].(M)
			if env["OPENAI_API_KEY"] != key["VLLM_API_KEY"] || env["DEFAULT_MODELS"] != applicationCommandOption(engine, "--served-model-name") {
				return fmt.Errorf("Open WebUI et vLLM : modèle ou clé incompatibles")
			}
		}
	}
	return nil
}
