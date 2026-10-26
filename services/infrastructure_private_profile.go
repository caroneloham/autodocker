package services

import (
	"regexp"
	"strconv"
	"strings"
)

func normalizeExplicitIngressIntent(prompt string, payload *BuildComposePayload) {
	hasCaddy := false
	for _, service := range payload.Services {
		hasCaddy = hasCaddy || service.Key == "caddy"
	}
	if !hasCaddy {
		return
	}
	words := infrastructureWords(prompt)
	http := regexp.MustCompile(`\b(?:en|entree|acces) http\b`).MatchString(words) && !strings.Contains(words, "https")
	https := strings.Contains(words, "https")
	routing := regexp.MustCompile(`\b(?:reverse proxy|unique proxy|seul proxy|routage)\b`).MatchString(words)
	if regexp.MustCompile(`\b(?:sans|pas de|aucun) (?:reverse proxy|proxy|routage)\b`).MatchString(words) {
		routing = false
	}
	// Missing optional model JSON must not leave an explicitly requested
	// gateway disconnected. Managed HTTPS uses an internal certificate; public
	// trust/DNS still require the explicit setup steps and warning below.
	if !http && !https && !routing {
		return
	}
	if payload.Ingress == nil {
		payload.Ingress = &IngressConfig{Proxy: "caddy", Domain: "localhost", Port: 8088}
	}
	if payload.Ingress.Proxy != "caddy" {
		return
	}
	if https {
		payload.Ingress.TLS = "internal"
		payload.Ingress.Port = 8443
	} else if http {
		payload.Ingress.TLS = ""
	}
	addresses := regexp.MustCompile(`(?i)\b((?:[a-z0-9-]+\.)+[a-z]{2,}|localhost):([0-9]{2,5})\b`).FindAllStringSubmatch(prompt, -1)
	if len(addresses) == 1 {
		payload.Ingress.Domain = strings.ToLower(addresses[0][1])
		payload.Ingress.Port, _ = strconv.Atoi(addresses[0][2])
	} else if port := infrastructureExplicitSettings(prompt, infrastructureCatalogue())["caddy"].HostPort; port > 0 {
		payload.Ingress.Port = port
	}
	if domain := regexp.MustCompile(`(?i)\bdomaine\s+([a-z0-9][a-z0-9.-]+\.[a-z]{2,})\b`).FindStringSubmatch(prompt); len(domain) > 1 {
		payload.Ingress.Domain = strings.ToLower(domain[1])
	}
}

func infrastructureDataService(key string) bool {
	switch key {
	case "postgres", "mariadb", "mysql", "mongo", "redis", "valkey", "memcached", "qdrant":
		return true
	}
	return false
}

func infrastructurePrivateDataRequested(prompt string) bool {
	words := infrastructureWords(prompt)
	return regexp.MustCompile(`\b(?:sans ports? publie|aucun port publie|restent internes|reste interne)\b|\bn expose\w* aucune base|\bne\s+\w*expose\w* aucune base`).MatchString(words)
}

func infrastructureVisionRequested(words string) bool {
	for _, negative := range []string{"sans vision", "sans image", "pas d image", "pas de vision"} {
		if strings.Contains(words, negative) {
			return false
		}
	}
	if regexp.MustCompile(`\b(?:vision|multimodal|multimodale|multimodaux)\b`).MatchString(words) {
		return true
	}
	visualWords := regexp.MustCompile(`\bimages?\s+(?:docker|vllm|de conteneur|conteneur)\b`).ReplaceAllString(words, "")
	return regexp.MustCompile(`\b(?:images?|photos?|captures?)\b`).MatchString(visualWords)
}

// Functional requirements belong to the generator's contract, independently of
// the provider's ability to emit optional JSON fields. Only explicit requests
// are applied; the model's choice of services is preserved.
func normalizeProfessionalAIIntent(prompt string, payload *BuildComposePayload) []string {
	words := infrastructureWords(prompt)
	hasVLLM, hasWeb, hasCaddy := false, false, false
	for _, s := range payload.Services {
		hasVLLM = hasVLLM || s.Key == "vllm"
		hasWeb = hasWeb || s.Key == "open-webui"
		hasCaddy = hasCaddy || s.Key == "caddy"
	}
	if !hasVLLM && !hasWeb {
		return nil
	}
	vision := hasVLLM && infrastructureVisionRequested(words)
	visionExcluded := hasVLLM && (strings.Contains(words, "sans vision") || strings.Contains(words, "sans image") || strings.Contains(words, "pas d image") || strings.Contains(words, "pas de vision"))
	private := hasWeb && (strings.Contains(words, "plusieurs utilisateurs") || strings.Contains(words, "comptes utilisateurs") || strings.Contains(words, "plateforme ia privee"))
	tls := hasCaddy && strings.Contains(words, "https") && (strings.Contains(words, "prive") || strings.Contains(words, "local"))
	context := regexp.MustCompile(`\bcontexte\s+(?:de\s+)?([0-9]+)\b`).FindStringSubmatch(words)
	parallel := regexp.MustCompile(`\b([0-9]+)\s+requetes?\s+simultane`).FindStringSubmatch(words)
	repo := regexp.MustCompile(`(?i)(?:mod[eè]le|model|avec|utilis(?:e|er|era)|charg(?:e|er|era)|ex[eé]cut(?:e|er|era))\s+(?:le\s+mod[eè]le\s+)?(?:https://huggingface\.co/)?([a-z0-9][a-z0-9_.-]*/[a-z0-9][a-z0-9_.-]*)`).FindStringSubmatch(prompt)
	if !vision && !visionExcluded && !private && !tls && len(context) == 0 && len(parallel) == 0 && len(repo) < 2 {
		return nil
	}
	config, _ := professionalAIConfig(*payload)
	if visionExcluded {
		config.Vision = false
	}
	if vision {
		config.Vision = true
		if payload.AIStack == nil || payload.AIStack.Model == "" {
			config.Model = "Qwen/Qwen2.5-VL-3B-Instruct"
		}
	}
	// A text-only model explicitly named by the user also overrides defaults.
	if hasVLLM && len(repo) > 1 {
		config.Model = strings.TrimRight(repo[1], ".,;")
		config.Vision = vision // Do not turn a named chat model into a vision stack.
	}
	if private {
		config.PrivatePlatform = true
	}
	if len(context) > 1 {
		config.MaxContext, _ = strconv.Atoi(context[1])
	}
	if len(parallel) > 1 {
		config.Concurrency, _ = strconv.Atoi(parallel[1])
	}
	payload.AIStack = &config
	if tls {
		if payload.Ingress == nil {
			port := 8443
			if setting := infrastructureExplicitSettings(prompt, infrastructureCatalogue())["caddy"]; setting.HostPort > 0 {
				port = setting.HostPort
			}
			payload.Ingress = &IngressConfig{Proxy: "caddy", Domain: "localhost", Port: port}
		}
		payload.Ingress.TLS = "internal"
	}
	return []string{"Les demandes explicites de vision, de comptes, de capacité et de HTTPS privé sont appliquées par le générateur, puis vérifiées avec les services proposés."}
}
