package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Multi-AI provider support: Local, Groq (fast), Mistral (french), OpenRouter (fallback)
// Keys are loaded from environment or secrets DB. Used ONLY on explicit user action.

var (
	groqKeys       []string
	groqKeyIdx     int
	mistralKeys    []string
	mistralKeyIdx  int
	openrouterKeys []string
	openrouterIdx  int
	openrouterMu   sync.Mutex
)

const DefaultOpenRouterModel = "openai/gpt-5.4-pro"

// SetGroqKeys sets multiple Groq API keys for rotation.
func SetGroqKeys(keys []string) {
	groqKeys = nil
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k != "" {
			groqKeys = append(groqKeys, k)
		}
	}
}

// SetGroqKey sets a single Groq key (backward compat).
func SetGroqKey(key string) {
	k := strings.TrimSpace(key)
	if k != "" {
		SetGroqKeys([]string{k})
	}
}

// SetMistralKeys sets the Mistral API keys for rotation.
func SetMistralKeys(keys []string) {
	mistralKeys = nil
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k != "" {
			mistralKeys = append(mistralKeys, k)
		}
	}
}

// SetOpenRouterKeys sets multiple OpenRouter API keys for rotation.
func SetOpenRouterKeys(keys []string) {
	openrouterMu.Lock()
	defer openrouterMu.Unlock()
	openrouterKeys = nil
	openrouterIdx = 0
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k != "" {
			openrouterKeys = append(openrouterKeys, k)
		}
	}
}

// SetOpenRouterKey sets a single OpenRouter API key for backward compatibility.
func SetOpenRouterKey(key string) { SetOpenRouterKeys([]string{key}) }

// GroqAvailable returns true if Groq is configured.
func GroqAvailable() bool { return len(groqKeys) > 0 }

// getNextGroqKey rotates through Groq keys.
func getNextGroqKey() string {
	if len(groqKeys) == 0 {
		return ""
	}
	key := groqKeys[groqKeyIdx%len(groqKeys)]
	groqKeyIdx++
	return key
}

// getNextMistralKey rotates through Mistral keys.
func getNextMistralKey() string {
	if len(mistralKeys) == 0 {
		return ""
	}
	key := mistralKeys[mistralKeyIdx%len(mistralKeys)]
	mistralKeyIdx++
	return key
}

// MistralAvailable returns true if Mistral is configured.
func MistralAvailable() bool { return len(mistralKeys) > 0 }

// OpenRouterAvailable returns true if OpenRouter is configured.
func OpenRouterAvailable() bool { return OpenRouterKeyCount() > 0 }

// OpenRouterKeyCount returns the configured OpenRouter key count without exposing values.
func OpenRouterKeyCount() int {
	openrouterMu.Lock()
	defer openrouterMu.Unlock()
	return len(openrouterKeys)
}

func getNextOpenRouterKey() string {
	openrouterMu.Lock()
	defer openrouterMu.Unlock()
	if len(openrouterKeys) == 0 {
		return ""
	}
	key := openrouterKeys[openrouterIdx%len(openrouterKeys)]
	openrouterIdx++
	return key
}

func OpenRouterModel() string {
	model := strings.TrimSpace(os.Getenv("OPENROUTER_MODEL"))
	if model == "" {
		return DefaultOpenRouterModel
	}
	return model
}

func OpenRouterFallbackModels() []string {
	configured := strings.TrimSpace(os.Getenv("OPENROUTER_FALLBACK_MODELS"))
	if configured == "" {
		configured = "anthropic/claude-sonnet-4.6,google/gemini-3.1-pro-preview"
	}
	var models []string
	for _, model := range strings.Split(configured, ",") {
		model = strings.TrimSpace(model)
		if model != "" {
			models = append(models, model)
		}
	}
	return models
}

// CallGroq sends a prompt to Groq (Llama 3.3 70B) and returns the response text.
// Ultra-fast: <1s response time. Best for: log analysis, quick questions.
func CallGroq(systemPrompt, userPrompt string) (string, error) {
	if !GroqAvailable() {
		return "", fmt.Errorf("Groq non configure")
	}

	key := getNextGroqKey()
	body := map[string]interface{}{
		"model": "llama-3.3-70b-versatile",
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature": 0.3,
		"max_tokens":  2000,
	}

	return callOpenAICompatible("https://api.groq.com/openai/v1/chat/completions", key, body, 30*time.Second)
}

// CallMistral sends a prompt to Mistral API. Best for: French text, CVE explanations, documentation.
func CallMistral(systemPrompt, userPrompt string) (string, error) {
	if !MistralAvailable() {
		return "", fmt.Errorf("Mistral non configure")
	}

	key := getNextMistralKey()

	body := map[string]interface{}{
		"model": "mistral-small-latest",
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature": 0.3,
		"max_tokens":  2000,
	}

	return callOpenAICompatible("https://api.mistral.ai/v1/chat/completions", key, body, 60*time.Second)
}

// CallOpenRouter sends a prompt via OpenRouter (access to Claude, GPT, Llama, etc.)
func CallOpenRouter(systemPrompt, userPrompt string) (string, error) {
	if !OpenRouterAvailable() {
		return "", fmt.Errorf("OpenRouter non configure")
	}

	models := uniqueOpenRouterModels(append([]string{OpenRouterModel()}, OpenRouterFallbackModels()...))
	body := map[string]interface{}{
		"models": models,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature": 0.3,
		"max_tokens":  2000,
	}

	return callOpenAICompatible("https://openrouter.ai/api/v1/chat/completions", getNextOpenRouterKey(), body, 120*time.Second)
}

func uniqueOpenRouterModels(models []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 3)
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		out = append(out, model)
		seen[model] = true
		if len(out) == 3 {
			break
		}
	}
	return out
}

// callOpenAICompatible calls any OpenAI-compatible API endpoint.
func callOpenAICompatible(url, apiKey string, body map[string]interface{}, timeout time.Duration) (string, error) {
	jsonBody, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("requete echouee: %v", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("API status %d: %s", resp.StatusCode, string(data)[:min200(len(data))])
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("JSON parse error: %v", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("reponse vide")
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

func min200(n int) int {
	if n > 200 {
		return 200
	}
	return n
}

// AIProvidersStatus returns the status of all configured AI providers.
func AIProvidersStatus() map[string]interface{} {
	return map[string]interface{}{
		"local":            LocalAIEnabled(),
		"local_model":      LocalAIModel(),
		"local_ollama":     LocalOllamaReachable(500 * time.Millisecond),
		"gemini":           AIAvailable(),
		"groq":             GroqAvailable(),
		"mistral":          MistralAvailable(),
		"openrouter":       OpenRouterAvailable(),
		"openrouter_keys":  OpenRouterKeyCount(),
		"openrouter_model": OpenRouterModel(),
	}
}
