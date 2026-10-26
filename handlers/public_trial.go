package handlers

import "github.com/gin-gonic/gin"

// RegisterPublicTrialRoutes deliberately exposes only stateless generation.
// It must be mounted by an isolated process, never on the account server.
func RegisterPublicTrialRoutes(api *gin.RouterGroup) {
	api.POST("/generate/preview", handleGenerationPreview)
	api.POST("/generate", handleGenerationExport)
}
