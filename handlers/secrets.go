package handlers

import (
	"net/http"
	"time"

	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/gin-gonic/gin"
)

// RegisterSecretsRoutes registers secrets management endpoints.
func RegisterSecretsRoutes(api *gin.RouterGroup) {
	s := api.Group("/secrets")
	{
		s.GET("/:project", handleListSecrets)
		s.POST("/:project", middleware.AdminRequired(), handleAddSecret)
		s.GET("/:project/:key/reveal", middleware.AdminRequired(), handleRevealSecret)
		s.DELETE("/:project/:key", middleware.AdminRequired(), handleDeleteSecret)
	}
}

// GET /api/secrets/:project — list secrets (values masked)
func handleListSecrets(c *gin.Context) {
	project := c.Param("project")
	rows, err := db.DB.Query("SELECT id, key_name, created_by, created_at FROM secrets WHERE project = ? ORDER BY key_name", project)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	secrets := make([]gin.H, 0)
	for rows.Next() {
		var id int
		var keyName, createdBy, createdAt string
		if rows.Scan(&id, &keyName, &createdBy, &createdAt) == nil {
			secrets = append(secrets, gin.H{
				"id":         id,
				"key_name":   keyName,
				"value":      "********",
				"created_by": createdBy,
				"created_at": createdAt,
			})
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "secrets": secrets})
}

// POST /api/secrets/:project — add or update a secret
func handleAddSecret(c *gin.Context) {
	project := c.Param("project")
	var req struct {
		KeyName string `json:"key_name"`
		Value   string `json:"value"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.KeyName == "" || req.Value == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key_name et value requis"})
		return
	}

	encrypted, err := services.EncryptSecret(req.Value)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur chiffrement: " + err.Error()})
		return
	}

	user, _ := middleware.GetSessionUser(c)
	now := time.Now().Format("2006-01-02 15:04:05")

	_, err = db.DB.Exec(
		`INSERT INTO secrets (project, key_name, encrypted_value, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(project, key_name) DO UPDATE SET encrypted_value=?, created_by=?, created_at=?`,
		project, req.KeyName, encrypted, user, now,
		encrypted, user, now,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	db.LogActivity(user, "secret_add", project, req.KeyName)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Secret '" + req.KeyName + "' enregistre"})
}

// GET /api/secrets/:project/:key/reveal — decrypt and return value (admin only)
func handleRevealSecret(c *gin.Context) {
	project := c.Param("project")
	keyName := c.Param("key")

	var encrypted string
	err := db.DB.QueryRow("SELECT encrypted_value FROM secrets WHERE project = ? AND key_name = ?", project, keyName).Scan(&encrypted)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Secret introuvable"})
		return
	}

	plaintext, err := services.DecryptSecret(encrypted)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Dechiffrement echoue"})
		return
	}

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "secret_reveal", project, keyName)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "value": plaintext})
}

// DELETE /api/secrets/:project/:key — remove a secret
func handleDeleteSecret(c *gin.Context) {
	project := c.Param("project")
	keyName := c.Param("key")

	result, err := db.DB.Exec("DELETE FROM secrets WHERE project = ? AND key_name = ?", project, keyName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Secret introuvable"})
		return
	}

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "secret_delete", project, keyName)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
