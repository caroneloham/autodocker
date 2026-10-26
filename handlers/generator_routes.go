package handlers

import "github.com/gin-gonic/gin"

// RegisterGeneratorRoutes exposes the infrastructure creation flow only. The
// historical terminal, host management and other removed tools are not active.
func RegisterGeneratorRoutes(api *gin.RouterGroup) {
	api.POST("/generate", handleGenerationExport)
	api.POST("/generate/preview", handleGenerationPreview)
	// Compatibility for older clients, without writing a saved project.
	api.POST("/deploy", handleGenerationExport)
	api.POST("/deploy/preview", handleGenerationPreview)
	api.POST("/ai-generate", handleAIGenerate)
	registerInfrastructureJobRoutes(api)
	registerCloudToolRoutes(api)
	api.GET("/used-cidrs", handleUsedCIDRs)
	api.GET("/catalogue", handleCatalogueList)
	api.GET("/docker/status", handleDockerStatus)
	api.GET("/project/:name/compose", handleProjectCompose)
	api.POST("/project/:name/export", handleProjectExport)
}
