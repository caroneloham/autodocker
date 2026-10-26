package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type infrastructureProvider struct {
	name     string
	model    string
	complete func(context.Context, string, string) (string, error)
}

// Provider selection is explicit and sequential: no racing several paid APIs,
// no substituting a preset for the model the user selected.
func selectedInfrastructureProvider() (infrastructureProvider, error) {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("AUTODOCKER_AI_PROVIDER")))
	if name == "" {
		switch {
		case strings.TrimSpace(os.Getenv("LM_STUDIO_MODEL")) != "":
			name = "lmstudio"
		case strings.TrimSpace(os.Getenv("AUTODOCKER_AI_URL")) != "":
			name = "compatible"
		case LocalAIEnabled():
			name = "local"
		case AIAvailable():
			name = "gemini"
		case OpenRouterAvailable():
			name = "openrouter"
		case MistralAvailable():
			name = "mistral"
		case GroqAvailable():
			name = "groq"
		default:
			return infrastructureProvider{}, fmt.Errorf("aucun modèle IA configuré ; configurez un fournisseur ou choisissez vos services dans le catalogue")
		}
	}
	if name == "ollama" {
		name = "local"
	}
	return infrastructureProviderForName(name), nil
}

func infrastructureProviderForName(name string) infrastructureProvider {
	if provider, known := cloudInfrastructureProvider(name); known {
		return provider
	}
	p := infrastructureProvider{name: name}
	p.complete = func(ctx context.Context, system, message string) (string, error) {
		endpoint, model, key, format := "", "", "", "openai"
		switch name {
		case "lmstudio":
			format = "lmstudio"
			model = strings.TrimSpace(os.Getenv("LM_STUDIO_MODEL"))
			if model == "" {
				return "", fmt.Errorf("aucun modèle LM Studio configuré")
			}
			base := strings.TrimSpace(os.Getenv("LM_STUDIO_URL"))
			if base == "" {
				base = "http://127.0.0.1:1234"
			}
			if err := validateInfrastructureEndpoint(base, true); err != nil {
				return "", err
			}
			endpoint = strings.TrimRight(base, "/") + "/v1/chat/completions"
		case "local":
			endpoint, model, format = LocalAIBaseURL()+"/api/chat", LocalAIModel(), "ollama"
			if err := validateInfrastructureEndpoint(endpoint, false); err != nil {
				return "", err
			}
		case "gemini":
			key = nextInfrastructureCredential("gemini")
			if key == "" {
				return "", fmt.Errorf("Gemini non configuré")
			}
			endpoint, model, format = "https://generativelanguage.googleapis.com/v1beta/models/"+url.PathEscape(GeminiModel())+":generateContent", GeminiModel(), "gemini"
		case "openrouter":
			if !OpenRouterAvailable() {
				return "", fmt.Errorf("OpenRouter non configuré")
			}
			endpoint, model, key = "https://openrouter.ai/api/v1/chat/completions", OpenRouterModel(), getNextOpenRouterKey()
		case "mistral":
			if !MistralAvailable() {
				return "", fmt.Errorf("Mistral non configuré")
			}
			endpoint, model, key = "https://api.mistral.ai/v1/chat/completions", "mistral-small-latest", getNextMistralKey()
		case "groq":
			key = nextInfrastructureCredential("groq")
			if key == "" {
				return "", fmt.Errorf("Groq non configuré")
			}
			endpoint = infrastructureEnvDefault("GROQ_BASE_URL", "https://api.groq.com/openai/v1")
			model = infrastructureEnvDefault("GROQ_MODEL", "llama-3.3-70b-versatile")
			endpoint = strings.TrimRight(endpoint, "/") + "/chat/completions"
		case "nvidia":
			format = "nvidia"
			key = nextInfrastructureCredential("nvidia")
			endpoint = strings.TrimRight(infrastructureEnvDefault("NVIDIA_BASE_URL", "https://integrate.api.nvidia.com/v1"), "/") + "/chat/completions"
			model = strings.TrimSpace(os.Getenv("NVIDIA_MODEL"))
			if key == "" || model == "" {
				return "", fmt.Errorf("NVIDIA : clé et modèle requis")
			}
		case "cloudflare":
			format = "cloudflare"
			key = nextInfrastructureCredential("cloudflare")
			base := strings.TrimSpace(os.Getenv("CLOUDFLARE_BASE_URL"))
			if base == "" {
				account := strings.TrimSpace(os.Getenv("CLOUDFLARE_ACCOUNT_ID"))
				if len(account) != 32 || strings.ContainsAny(account, "/?&# ") {
					return "", fmt.Errorf("identifiant de compte Cloudflare requis")
				}
				base = "https://api.cloudflare.com/client/v4/accounts/" + account + "/ai/v1"
			}
			endpoint = strings.TrimRight(base, "/") + "/chat/completions"
			model = strings.TrimSpace(os.Getenv("CLOUDFLARE_MODEL"))
			if key == "" || model == "" {
				return "", fmt.Errorf("Cloudflare : clé et modèle requis")
			}
		case "compatible", "openai-compatible":
			base := strings.TrimRight(strings.TrimSpace(os.Getenv("AUTODOCKER_AI_URL")), "/")
			if err := validateInfrastructureEndpoint(base, false); err != nil {
				return "", err
			}
			endpoint = base
			if !strings.HasSuffix(endpoint, "/chat/completions") {
				endpoint += "/chat/completions"
			}
			model, key = strings.TrimSpace(os.Getenv("AUTODOCKER_AI_MODEL")), strings.TrimSpace(os.Getenv("AUTODOCKER_AI_KEY"))
			if model == "" {
				return "", fmt.Errorf("AUTODOCKER_AI_MODEL est requis pour ce fournisseur")
			}
		default:
			return "", fmt.Errorf("fournisseur IA inconnu : %s", name)
		}
		if err := validateInfrastructureEndpoint(endpoint, false); err != nil {
			return "", err
		}
		return requestInfrastructureModel(ctx, endpoint, model, key, format, system, message)
	}
	return p
}

func validateInfrastructureEndpoint(raw string, localOnly bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("adresse du fournisseur IA invalide")
	}
	loopback := strings.EqualFold(u.Hostname(), "localhost")
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if localOnly && !loopback {
		return fmt.Errorf("LM Studio doit utiliser une adresse locale")
	}
	if !loopback && u.Scheme != "https" {
		return fmt.Errorf("un fournisseur distant doit utiliser HTTPS")
	}
	return nil
}

func requestInfrastructureModel(ctx context.Context, endpoint, model, key, format, system, message string) (string, error) {
	body := map[string]interface{}{
		"model": model, "messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": message}},
		"temperature": 0.1, "max_tokens": 3072, "stream": false,
	}
	if format == "ollama" {
		delete(body, "max_tokens")
		delete(body, "temperature")
		body["format"] = "json"
		body["options"] = map[string]interface{}{"temperature": 0.1, "num_predict": 3072, "num_ctx": 8192}
	} else if format == "gemini" {
		body = map[string]interface{}{
			"system_instruction": map[string]interface{}{"parts": []map[string]string{{"text": system}}},
			"contents":           []map[string]interface{}{{"parts": []map[string]string{{"text": message}}}},
			"generationConfig":   map[string]interface{}{"temperature": 0.1, "maxOutputTokens": 3072, "responseMimeType": "application/json"},
		}
	} else if format == "lmstudio" {
		reasoning := lmStudioInfrastructureReasoning(ctx, strings.TrimSuffix(endpoint, "/v1/chat/completions"), model)
		if reasoning == "off" {
			body["reasoning_effort"] = "none"
		} else if reasoning != "" {
			body["reasoning_effort"] = reasoning
		}
		body["response_format"] = map[string]interface{}{"type": "json_schema", "json_schema": map[string]interface{}{"name": "autodocker_infrastructure", "strict": true, "schema": infrastructureResponseSchema()}}
	}
	if format == "nvidia" && strings.Contains(strings.ToLower(model), "nemotron-3-") {
		body["chat_template_kwargs"] = map[string]interface{}{"enable_thinking": false}
	}
	if format == "cloudflare" {
		body["options"] = map[string]interface{}{"rejectIfBusy": true}
	}
	if format == "cohere" {
		body["reasoning_effort"] = "none"
	}
	if format == "zai" {
		body["reasoning_effort"] = "low"
	}
	if format == "aion" && !strings.Contains(model, "3.5") {
		body["reasoning_effort"] = "none"
	}
	if (format == "groq" || format == "ovh") && strings.Contains(model, "gpt-oss") {
		body["reasoning_effort"] = "low"
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "AutoDocker/1.0")
	if key != "" {
		if format == "gemini" {
			req.Header.Set("x-goog-api-key", key)
		} else {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	client := &http.Client{Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &infrastructureProviderError{message: "le fournisseur IA ne répond pas ; vérifiez sa disponibilité et le modèle demandé"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &infrastructureProviderError{status: resp.StatusCode, retryAfter: infrastructureRetryAfter(resp.Header.Get("Retry-After"), time.Now()), message: fmt.Sprintf("le fournisseur IA a refusé la demande (HTTP %d)", resp.StatusCode)}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		return "", fmt.Errorf("réponse du fournisseur trop volumineuse ou interrompue")
	}
	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("réponse du fournisseur invalide")
	}
	if format == "cloudflare" {
		if success, ok := result["success"].(bool); ok && !success {
			return "", fmt.Errorf("Cloudflare n'a pas pu produire de réponse")
		}
		if nested, ok := result["result"].(map[string]interface{}); ok {
			result = nested
		}
	}
	reported, _ := result["model"].(string)
	if reported == "" {
		reported, _ = result["modelVersion"].(string)
	}
	if observer, ok := ctx.Value(infrastructureModelObserverKey{}).(func(string)); ok && cloudModelID.MatchString(reported) && (key == "" || !strings.Contains(reported, key)) {
		observer(reported)
	}
	var text string
	switch format {
	case "gemini":
		text = extractTextFromGemini(result)
	case "ollama":
		msg, _ := result["message"].(map[string]interface{})
		text, _ = msg["content"].(string)
	default:
		choices, _ := result["choices"].([]interface{})
		if len(choices) > 0 {
			choice, _ := choices[0].(map[string]interface{})
			msg, _ := choice["message"].(map[string]interface{})
			text, _ = msg["content"].(string)
			if format == "cloudflare" && text == "" {
				// Workers AI can return an already-decoded JSON object as content.
				// Serialize it back through the same grounding/validation pipeline.
				if object, ok := msg["content"].(map[string]interface{}); ok {
					if plain, ok := object["text"].(string); ok {
						text = plain
					} else {
						encoded, _ := json.Marshal(object)
						text = string(encoded)
					}
				}
			}
		}
	}
	if format == "cloudflare" && text == "" {
		text, _ = result["response"].(string)
	}
	if end := strings.Index(text, "</think>"); end >= 0 {
		text = text[end+len("</think>"):]
	}
	if strings.TrimSpace(text) == "" {
		return "", &infrastructureProposalError{message: "le modèle n'a renvoyé aucune réponse exploitable"}
	}
	return text, nil
}

// Deliberately exclude the remote body, URL and request from errors: any of them
// can contain a credential or customer data.
type infrastructureProviderError struct {
	status     int
	retryAfter time.Duration
	message    string
}

func (e *infrastructureProviderError) Error() string { return e.message }

func infrastructureRetryAfter(raw string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if until, err := http.ParseTime(raw); err == nil && until.After(now) {
		return until.Sub(now)
	}
	return 0
}

func infrastructureCooldown(err error) time.Duration {
	var proposal *infrastructureProposalError
	if errors.As(err, &proposal) {
		return 0
	}
	var remote *infrastructureProviderError
	if errors.As(err, &remote) {
		if remote.retryAfter > 0 {
			return remote.retryAfter
		}
		switch remote.status {
		case 401, 402, 403, 404, 410:
			return 5 * time.Minute
		case 429:
			return time.Minute
		}
	}
	return 30 * time.Second
}
