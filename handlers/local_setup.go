package handlers

import (
	"net/http"

	"autodocker-go/middleware"
	"autodocker-go/services"
	"github.com/gin-gonic/gin"
)

func registerLocalSetupRoutes(api *gin.RouterGroup) {
	owner := api.Group("/account/setup", middleware.AccountOwnerRequired())
	owner.GET("", handleLocalSetupGet)
	owner.POST("", handleLocalSetupSave)
	owner.POST("/test-ai", handleLocalSetupTestAI)
	owner.GET("/docker", handleLocalSetupDocker)
}

func handleLocalSetupGet(c *gin.Context) {
	setup, err := services.ReadLocalSetup()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Réglages locaux indisponibles."})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, setup)
}

func handleLocalSetupSave(c *gin.Context) {
	var input services.LocalSetupInput
	if !decodeAccountRequest(c, &input) {
		return
	}
	previous, err := services.ReadLocalSetup()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Réglages locaux indisponibles."})
		return
	}
	setup, err := services.SaveLocalSetup(input)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if previous.DockerHost != setup.DockerHost {
		services.ReconnectDocker()
	}
	c.JSON(http.StatusOK, setup)
}

func handleLocalSetupTestAI(c *gin.Context) {
	var input services.LocalSetupInput
	if !decodeAccountRequest(c, &input) {
		return
	}
	ok, message := services.ProbeLocalAIInput(input)
	c.JSON(http.StatusOK, gin.H{"available": ok, "message": message})
}

func handleLocalSetupDocker(c *gin.Context) {
	available := services.EnsureDocker() != nil
	message := "Docker local détecté."
	if !available {
		message = "Docker local introuvable. Démarrez Docker Desktop ou choisissez un socket local."
	}
	c.JSON(http.StatusOK, gin.H{"available": available, "message": message})
}
