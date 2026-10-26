package handlers

import (
	"context"
	"fmt"
	"strings"

	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"
	"github.com/gin-gonic/gin"
)

// RegisterCopilotRoutes registers the AI copilot chat endpoint.
func RegisterCopilotRoutes(api *gin.RouterGroup) {
	api.POST("/copilot/chat", handleCopilotChat)
	api.GET("/copilot/status", handleCopilotStatus)
}

// GET /api/copilot/status — returns available AI providers
func handleCopilotStatus(c *gin.Context) {
	c.JSON(200, gin.H{
		"status":    "ok",
		"providers": services.AIProvidersStatus(),
	})
}

// POST /api/copilot/chat — AI copilot with full infra context
func handleCopilotChat(c *gin.Context) {
	var req struct {
		Message  string `json:"message"`
		Provider string `json:"provider"` // "auto", "local", "groq", "mistral", "openrouter", "gemini"
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Message == "" {
		c.JSON(400, gin.H{"error": "message requis"})
		return
	}

	user, _ := middleware.GetSessionUser(c)
	if req.Provider == "" {
		req.Provider = "auto"
	}

	// Build infrastructure context
	infraContext := buildInfraContext()

	systemPrompt := `Tu es le Copilot AutoDocker, un assistant IA expert en Docker et infrastructure.
Tu as acces au contexte complet de l'infrastructure de l'utilisateur (conteneurs, metriques, alertes, logs).

REGLES:
- Reponds en francais, de maniere concise et actionnable
- Si tu detectes un probleme, propose des ACTIONS concretes
- Formate tes reponses en Markdown
- Pour les actions, utilise ce format: [ACTION:type:param] (ex: [ACTION:restart:container-name])
- Sois proactif: si tu vois un probleme dans le contexte, mentionne-le meme si on ne te l'a pas demande
- Utilise des emojis pour rendre les reponses lisibles

CONTEXTE INFRASTRUCTURE ACTUEL:
` + infraContext

	// Choose provider
	var response string
	var err error
	var usedProvider string

	switch req.Provider {
	case "local":
		response, err = services.CallLocalAIChat(systemPrompt, req.Message)
		usedProvider = "local"
	case "groq":
		response, err = services.CallGroq(systemPrompt, req.Message)
		usedProvider = "groq"
	case "mistral":
		response, err = services.CallMistral(systemPrompt, req.Message)
		usedProvider = "mistral"
	case "openrouter":
		response, err = services.CallOpenRouter(systemPrompt, req.Message)
		usedProvider = "openrouter"
	case "gemini":
		response, err = services.CallGeminiChat(systemPrompt, req.Message)
		usedProvider = "gemini"
	default: // auto: Groq → Mistral → OpenRouter → Gemini
		if services.LocalAIEnabled() {
			response, err = services.CallLocalAIChat(systemPrompt, req.Message)
			usedProvider = "local"
		}
		if err != nil || response == "" {
			if services.GroqAvailable() {
				response, err = services.CallGroq(systemPrompt, req.Message)
				usedProvider = "groq"
			}
		}
		if err != nil || response == "" {
			if services.MistralAvailable() {
				response, err = services.CallMistral(systemPrompt, req.Message)
				usedProvider = "mistral"
			}
		}
		if err != nil || response == "" {
			if services.OpenRouterAvailable() {
				response, err = services.CallOpenRouter(systemPrompt, req.Message)
				usedProvider = "openrouter"
			}
		}
		if err != nil || response == "" {
			if services.AIAvailable() {
				response, err = services.CallGeminiChat(systemPrompt, req.Message)
				usedProvider = "gemini"
			}
		}
	}

	if err != nil {
		c.JSON(500, gin.H{"error": "Erreur IA: " + err.Error(), "provider": usedProvider})
		return
	}

	db.LogActivity(user, "copilot_chat", usedProvider, req.Message[:minCopilot(len(req.Message), 100)])

	c.JSON(200, gin.H{
		"status":   "ok",
		"response": response,
		"provider": usedProvider,
	})
}

func minCopilot(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// buildInfraContext collects current infrastructure state for the copilot.
func buildInfraContext() string {
	var sb strings.Builder

	cli := services.EnsureDocker()
	if cli == nil {
		sb.WriteString("Docker: Non connecte\n")
		return sb.String()
	}

	// Running containers
	ctx := context.Background()
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err == nil {
		running := 0
		stopped := 0
		var details []string
		for _, c := range containers {
			name := ""
			if len(c.Names) > 0 {
				name = strings.TrimPrefix(c.Names[0], "/")
			}
			state := c.State
			if state == "running" {
				running++
			} else {
				stopped++
			}

			ports := ""
			for _, p := range c.Ports {
				if p.PublicPort > 0 {
					ports += fmt.Sprintf("%d->%d ", p.PublicPort, p.PrivatePort)
				}
			}
			details = append(details, fmt.Sprintf("- %s [%s] image=%s ports=%s", name, state, c.Image, ports))
		}

		sb.WriteString(fmt.Sprintf("Conteneurs: %d running, %d stopped\n", running, stopped))
		for _, d := range details {
			sb.WriteString(d + "\n")
		}
	}

	// Recent alerts from supervision rules
	sb.WriteString("\nAlertes recentes: ")
	var alertCount int
	db.DB.QueryRow("SELECT COUNT(*) FROM activity_log WHERE action LIKE '%alert%' AND timestamp > datetime('now', '-1 hour')").Scan(&alertCount)
	sb.WriteString(fmt.Sprintf("%d dans la derniere heure\n", alertCount))

	// Recent logs with errors
	rows, err := db.DB.Query("SELECT container_name, message FROM container_logs WHERE severity = 'error' ORDER BY timestamp DESC LIMIT 5")
	if err == nil {
		defer rows.Close()
		sb.WriteString("\nDerniers logs d'erreur:\n")
		count := 0
		for rows.Next() {
			var name, msg string
			if rows.Scan(&name, &msg) == nil {
				sb.WriteString(fmt.Sprintf("- [%s] %s\n", name, msg[:minCopilot(len(msg), 150)]))
				count++
			}
		}
		if count == 0 {
			sb.WriteString("- Aucune erreur recente\n")
		}
	}

	return sb.String()
}
