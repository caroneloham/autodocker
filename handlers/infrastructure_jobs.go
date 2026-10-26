package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"autodocker-go/config"
	"autodocker-go/middleware"
	"autodocker-go/services"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

var infrastructureJobQueue = services.InfrastructureJobs

func registerInfrastructureJobRoutes(api *gin.RouterGroup) {
	api.POST("/ai-jobs", handleInfrastructureJobCreate)
	api.GET("/ai-jobs/:id", handleInfrastructureJobGet)
	api.POST("/ai-jobs/:id/cancel", handleInfrastructureJobCancel)
}

func infrastructureJobOwner(c *gin.Context, create bool) (string, bool) {
	user, _ := middleware.GetSessionUser(c)
	if user == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"status": "error", "error": "Connexion requise"})
		return "", false
	}
	if !config.AuthBypass && !config.DemoMode {
		return "user:" + user, true
	}
	// Local workshop users share an account, but each encrypted browser session
	// owns its jobs. Normal authenticated accounts are isolated by username.
	session := sessions.Default(c)
	client, _ := session.Get("infrastructure_job_client").(string)
	if client == "" && create {
		data := make([]byte, 16)
		if _, err := rand.Read(data); err != nil {
			c.JSON(503, gin.H{"status": "error", "error": "Session IA indisponible"})
			return "", false
		}
		client = hex.EncodeToString(data)
		session.Set("infrastructure_job_client", client)
		if session.Save() != nil {
			c.JSON(503, gin.H{"status": "error", "error": "Session IA indisponible"})
			return "", false
		}
	}
	return "workshop:" + user + ":" + client, true
}

func infrastructureResultResponse(result *services.MagicAIResult) gin.H {
	return gin.H{"status": "ok", "payload": result.Payload, "summary": result.Summary, "reasons": result.Reasons, "questions": result.Questions, "attempts": result.Attempts, "preset": result.Source == "preset", "intent": result.Intent, "confidence": result.Confidence, "source": result.Source, "model": result.Model, "warnings": result.Warnings, "design_checks": result.DesignChecks, "manual_steps": result.ManualSteps}
}

func infrastructureJobResponse(view services.InfrastructureJobView) gin.H {
	response := gin.H{"status": "ok", "job": view}
	if view.Result != nil {
		response["result"] = infrastructureResultResponse(view.Result)
	}
	return response
}

func submitInfrastructureJob(c *gin.Context) (string, services.InfrastructureJobView, bool) {
	owner, ok := infrastructureJobOwner(c, true)
	if !ok {
		return "", services.InfrastructureJobView{}, false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	var req struct {
		Prompt string `json:"prompt"`
	}
	if c.ShouldBindJSON(&req) != nil || strings.TrimSpace(req.Prompt) == "" {
		c.JSON(400, gin.H{"status": "error", "error": "Prompt requis"})
		return "", services.InfrastructureJobView{}, false
	}
	providerContext, err := services.CaptureLocalSetupProvider()
	if err != nil {
		c.JSON(503, gin.H{"status": "error", "error": "Configuration IA locale indisponible"})
		return "", services.InfrastructureJobView{}, false
	}
	view, _, err := infrastructureJobQueue().SubmitWithStatusContext(providerContext, owner, req.Prompt)
	if err != nil {
		status := 400
		if errors.Is(err, services.ErrInfrastructureUserBusy) {
			status = 409
		}
		if errors.Is(err, services.ErrInfrastructureQueueFull) {
			status = 503
			c.Header("Retry-After", "5")
		}
		c.JSON(status, gin.H{"status": "error", "error": err.Error()})
		return "", services.InfrastructureJobView{}, false
	}
	c.Header("Cache-Control", "no-store")
	return owner, view, true
}

func handleInfrastructureJobCreate(c *gin.Context) {
	_, view, ok := submitInfrastructureJob(c)
	if ok {
		c.JSON(http.StatusAccepted, infrastructureJobResponse(view))
	}
}

func infrastructureRequestedJob(c *gin.Context) (string, string, bool) {
	owner, ok := infrastructureJobOwner(c, false)
	if !ok {
		return "", "", false
	}
	id := c.Param("id")
	if len(id) != 48 {
		c.JSON(404, gin.H{"status": "error", "error": "Demande introuvable ou expirée"})
		return "", "", false
	}
	if _, err := hex.DecodeString(id); err != nil {
		c.JSON(404, gin.H{"status": "error", "error": "Demande introuvable ou expirée"})
		return "", "", false
	}
	c.Header("Cache-Control", "no-store")
	return owner, id, true
}

func handleInfrastructureJobGet(c *gin.Context) {
	owner, id, ok := infrastructureRequestedJob(c)
	if !ok {
		return
	}
	view, err := infrastructureJobQueue().Get(owner, id)
	if err != nil {
		c.JSON(404, gin.H{"status": "error", "error": "Demande introuvable ou expirée"})
		return
	}
	c.JSON(200, infrastructureJobResponse(view))
}

func handleInfrastructureJobCancel(c *gin.Context) {
	owner, id, ok := infrastructureRequestedJob(c)
	if !ok {
		return
	}
	view, err := infrastructureJobQueue().Cancel(owner, id)
	if err != nil {
		c.JSON(404, gin.H{"status": "error", "error": "Demande introuvable ou expirée"})
		return
	}
	c.JSON(200, infrastructureJobResponse(view))
}

// Legacy callers use the same queue and ownership limits; keeping the old API
// must not create an alternative path around global/provider concurrency.
func waitInfrastructureJob(ctx context.Context, owner, id string) (services.InfrastructureJobView, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		view, err := infrastructureJobQueue().Get(owner, id)
		if err != nil {
			return view, err
		}
		if view.State == "succeeded" || view.State == "failed" || view.State == "cancelled" {
			return view, nil
		}
		select {
		case <-ctx.Done():
			infrastructureJobQueue().Cancel(owner, id)
			return services.InfrastructureJobView{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
