package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const DefaultLocalAIModel = FastOllamaModel

func LocalAIEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AUTODOCKER_LOCAL_AI")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func LocalAIModel() string {
	model := strings.TrimSpace(os.Getenv("LOCAL_AI_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("OLLAMA_MODEL"))
	}
	if model == "" {
		model = DefaultLocalAIModel
	}
	return model
}

func LocalAIBaseURL() string {
	url := strings.TrimSpace(os.Getenv("LOCAL_AI_URL"))
	if url == "" {
		url = strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	}
	if url == "" {
		return "http://localhost:11434"
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}
	return strings.TrimRight(url, "/")
}

func LocalOllamaReachable(timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(LocalAIBaseURL() + "/api/tags")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func GenerateLocalConfig(prompt string) (map[string]interface{}, error) {
	return generateInfrastructurePayload(prompt, "local")
}

func CallLocalAIChat(systemPrompt, userMessage string) (string, error) {
	if LocalOllamaReachable(700 * time.Millisecond) {
		if text, err := callOllamaChat(systemPrompt, userMessage); err == nil && strings.TrimSpace(text) != "" {
			return text, nil
		}
	}
	return localCopilotReply(userMessage), nil
}

func callOllamaChat(systemPrompt, userMessage string) (string, error) {
	prompt := strings.TrimSpace(systemPrompt) + "\n\nUtilisateur: " + strings.TrimSpace(userMessage) + "\nAssistant:"
	body := map[string]interface{}{
		"model":  LocalAIModel(),
		"prompt": prompt,
		"stream": false,
		"options": map[string]interface{}{
			"temperature": 0.25,
		},
	}
	data, _ := json.Marshal(body)
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Post(LocalAIBaseURL()+"/api/generate", "application/json", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Ollama status %d", resp.StatusCode)
	}
	var result struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Response), nil
}

func localCopilotReply(message string) string {
	p := strings.ToLower(message)
	switch {
	case strings.Contains(p, "anomal") || strings.Contains(p, "erreur") || strings.Contains(p, "logs"):
		return "Mode local actif.\n\n- Regarde d'abord les conteneurs en `exited` dans le dashboard.\n- Ouvre les logs du service concerne avec le bouton `Logs`.\n- Si un port est occupe, redemarre la stack ou change le port expose.\n\n[ACTION:status:all]"
	case strings.Contains(p, "restart") || strings.Contains(p, "redemarr"):
		return "Mode local actif. Pour redemarrer un service, utilise le bouton `Restart` sur la ligne du conteneur, ou tape `/restart nom-du-conteneur` dans le Copilot."
	case strings.Contains(p, "deplo") || strings.Contains(p, "stack") || strings.Contains(p, "infra"):
		return "Mode local actif. Pour une demo rapide, lance `Mode Magic`, nomme le projet, puis demande une stack simple comme `portfolio nginx`, `monitoring`, `boutique`, ou `ia locale rapide`."
	case strings.Contains(p, "resume") || strings.Contains(p, "statut") || strings.Contains(p, "status"):
		return "Mode local actif. Le dashboard centralise les projets, les conteneurs, les ports, les logs, les actions Start/Stop/Restart et l'acces aux previews web."
	default:
		return "Mode local actif. Je peux aider sur le diagnostic Docker, les ports, les logs, les stacks, les previews et les deploiements AutoDocker."
	}
}
