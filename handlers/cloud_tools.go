package handlers

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"autodocker-go/middleware"
	"autodocker-go/services"
	"github.com/gin-gonic/gin"
)

var cloudToolRate = struct {
	sync.Mutex
	last map[string]time.Time
}{last: map[string]time.Time{}}

func registerCloudToolRoutes(api *gin.RouterGroup) {
	api.GET("/ai-capabilities", handleCloudCapabilities)
	api.POST("/ai-tools/search", handleCloudSearch)
	api.POST("/ai-tools/vision", handleCloudVision)
}

func handleCloudCapabilities(c *gin.Context) {
	if user, _ := middleware.GetSessionUser(c); user == "" {
		c.JSON(401, gin.H{"error": "Connexion requise"})
		return
	}
	capabilities, err := services.CloudCapabilities()
	if err != nil {
		c.JSON(503, gin.H{"error": "Configuration API indisponible"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"status": "ok", "providers": capabilities, "billing_verified": false})
}

func cloudToolOwnerAllowed(c *gin.Context) bool {
	owner, ok := infrastructureJobOwner(c, true)
	if !ok {
		return false
	}
	now := time.Now()
	cloudToolRate.Lock()
	defer cloudToolRate.Unlock()
	for key, last := range cloudToolRate.last {
		if now.Sub(last) > time.Minute {
			delete(cloudToolRate.last, key)
		}
	}
	if now.Sub(cloudToolRate.last[owner]) < 5*time.Second || len(cloudToolRate.last) >= 2048 {
		c.Header("Retry-After", "5")
		c.JSON(429, gin.H{"error": "Attendez quelques secondes avant un autre appel d’outil IA"})
		return false
	}
	cloudToolRate.last[owner] = now
	c.Header("Cache-Control", "no-store")
	return true
}

func handleCloudSearch(c *gin.Context) {
	if !cloudToolOwnerAllowed(c) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10)
	var request struct {
		Query    string `json:"query"`
		Provider string `json:"provider"`
	}
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(400, gin.H{"error": "Recherche invalide"})
		return
	}
	result, err := services.CloudSearch(c.Request.Context(), request.Query, request.Provider)
	if err != nil {
		cloudToolFailure(c, err)
		return
	}
	c.JSON(200, gin.H{"status": "ok", "provider": result.Provider, "results": result.Results})
}

func handleCloudVision(c *gin.Context) {
	if !cloudToolOwnerAllowed(c) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, (2<<20)+(8<<10))
	var request struct {
		Image    string `json:"image"`
		Question string `json:"question"`
		Provider string `json:"provider"`
	}
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(400, gin.H{"error": "Image ou question invalide"})
		return
	}
	result, err := services.CloudVision(c.Request.Context(), request.Image, request.Question, request.Provider)
	if err != nil {
		cloudToolFailure(c, err)
		return
	}
	c.JSON(200, gin.H{"status": "ok", "provider": result.Provider, "model": result.Model, "answer": result.Answer})
}

func cloudToolFailure(c *gin.Context, err error) {
	status := 503
	if errors.Is(err, services.ErrCloudToolInput) {
		status = 400
	}
	if errors.Is(err, services.ErrCloudToolBusy) {
		status = 429
		c.Header("Retry-After", "5")
	}
	// Service messages exclude upstream bodies, URLs and credentials.
	c.JSON(status, gin.H{"error": err.Error()})
}
