package handlers

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/services"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// builtinTemplates defines the set of built-in infrastructure templates.
var builtinTemplates = map[string]map[string]interface{}{
	"lamp.json": {
		"name":        "LAMP Stack",
		"description": "Linux, Apache (Nginx), MariaDB, PHP (WordPress)",
		"category":    "web",
		"icon":        "web",
		"template": map[string]interface{}{
			"services": []map[string]string{
				{"key": "nginx", "network": "web"},
				{"key": "wordpress", "network": "web"},
				{"key": "mariadb", "network": "db"},
				{"key": "adminer", "network": "db"},
			},
		},
	},
	"monitoring.json": {
		"name":        "Monitoring Stack",
		"description": "Grafana + Prometheus + Traefik pour supervision",
		"category":    "monitoring",
		"icon":        "chart",
		"template": map[string]interface{}{
			"services": []map[string]string{
				{"key": "grafana", "network": "monitoring"},
				{"key": "prometheus", "network": "monitoring"},
				{"key": "traefik", "network": "monitoring"},
			},
		},
	},
	"collaboration.json": {
		"name":        "Suite Collaborative",
		"description": "Nextcloud + MariaDB + Portainer",
		"category":    "collaboration",
		"icon":        "users",
		"template": map[string]interface{}{
			"services": []map[string]string{
				{"key": "nextcloud", "network": "collab"},
				{"key": "mariadb", "network": "collab"},
				{"key": "portainer", "network": "admin"},
			},
		},
	},
	"local-ai.json": {
		"name":        "IA Locale",
		"description": "Ollama + Open WebUI avec modele Mistral leger",
		"category":    "ai",
		"icon":        "bot",
		"template": map[string]interface{}{
			"services": []map[string]interface{}{
				{"key": "portainer", "network": "back"},
				{"key": "traefik", "network": "front"},
				{"key": "ollama", "network": "back", "extra": map[string]string{"model": services.DefaultOllamaModel}},
				{"key": "open-webui", "network": "front"},
			},
		},
	},
	"local-ai-fast.json": {
		"name":        "IA Locale Rapide",
		"description": "Ollama + Open WebUI avec TinyLlama pour tester vite",
		"category":    "ai",
		"icon":        "bot",
		"template": map[string]interface{}{
			"services": []map[string]interface{}{
				{"key": "portainer", "network": "back"},
				{"key": "traefik", "network": "front"},
				{"key": "ollama", "network": "back", "extra": map[string]string{"model": services.FastOllamaModel}},
				{"key": "open-webui", "network": "front"},
			},
		},
	},
	"security.json": {
		"name":        "Security Stack",
		"description": "Traefik + Fail2Ban + Portainer pour securite",
		"category":    "security",
		"icon":        "shield",
		"template": map[string]interface{}{
			"services": []map[string]string{
				{"key": "traefik", "network": "dmz"},
				{"key": "fail2ban", "network": "security"},
				{"key": "portainer", "network": "admin"},
			},
		},
	},
	"full.json": {
		"name":        "Infra Complete",
		"description": "Stack complete : Web + DB + Monitoring + Security + Admin",
		"category":    "enterprise",
		"icon":        "server",
		"template": map[string]interface{}{
			"services": []map[string]string{
				{"key": "traefik", "network": "web"},
				{"key": "nginx", "network": "web"},
				{"key": "wordpress", "network": "web"},
				{"key": "mariadb", "network": "db"},
				{"key": "postgres", "network": "db"},
				{"key": "adminer", "network": "db"},
				{"key": "nextcloud", "network": "collab"},
				{"key": "grafana", "network": "monitoring"},
				{"key": "prometheus", "network": "monitoring"},
				{"key": "portainer", "network": "admin"},
				{"key": "fail2ban", "network": "security"},
			},
		},
	},
}

var builtinFilenames map[string]bool

func init() {
	builtinFilenames = make(map[string]bool)
	for k := range builtinTemplates {
		builtinFilenames[k] = true
	}
	// ensureBuiltinTemplates() n'est PAS appele ici : l'init de package tourne
	// avant config.Init(), donc TemplatesDir serait vide et les fichiers
	// atterriraient dans le repertoire courant du process. L'appel se fait
	// dans RegisterTemplateRoutes, une fois la configuration prete.
}

func ensureBuiltinTemplates() {
	os.MkdirAll(config.TemplatesDir, 0755)
	for filename, data := range builtinTemplates {
		fpath := filepath.Join(config.TemplatesDir, filename)
		if _, err := os.Stat(fpath); err != nil {
			jsonData, _ := json.MarshalIndent(data, "", "  ")
			os.WriteFile(fpath, jsonData, 0644)
		}
	}
}

func slugify(name string) string {
	slug := strings.ToLower(strings.TrimSpace(name))
	re := regexp.MustCompile(`[^a-z0-9]+`)
	slug = re.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "template"
	}
	return slug
}

func readTemplateFile(fpath string) (map[string]interface{}, error) {
	data, err := os.ReadFile(fpath)
	if err != nil {
		return nil, err
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// RegisterTemplateRoutes registers template management routes.
func RegisterTemplateRoutes(api *gin.RouterGroup) {
	ensureBuiltinTemplates()
	g := api.Group("/templates")
	{
		g.GET("", handleListTemplates)
		g.POST("/save", handleSaveTemplate)
		g.DELETE("/:filename", handleDeleteTemplate)
	}
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

func handleListTemplates(c *gin.Context) {
	entries, err := os.ReadDir(config.TemplatesDir)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "templates": []gin.H{}})
		return
	}

	// Sort by name
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var templates []gin.H
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fpath := filepath.Join(config.TemplatesDir, e.Name())
		data, err := readTemplateFile(fpath)
		if err != nil {
			continue
		}

		name, _ := data["name"].(string)
		if name == "" {
			name = strings.TrimSuffix(e.Name(), ".json")
		}
		desc, _ := data["description"].(string)
		cat, _ := data["category"].(string)
		if cat == "" {
			cat = "other"
		}
		icon, _ := data["icon"].(string)
		if icon == "" {
			icon = "file"
		}

		templates = append(templates, gin.H{
			"name":        name,
			"description": desc,
			"category":    cat,
			"icon":        icon,
			"filename":    e.Name(),
			"is_builtin":  builtinFilenames[e.Name()],
		})
	}
	if templates == nil {
		templates = []gin.H{}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "templates": templates})
}

func handleSaveTemplate(c *gin.Context) {
	var body struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		Category    string      `json:"category"`
		Icon        string      `json:"icon"`
		Template    interface{} `json:"template"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Corps JSON requis"})
		return
	}

	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Le champ 'name' est requis"})
		return
	}
	if body.Template == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Le champ 'template' est requis"})
		return
	}

	slug := slugify(body.Name)
	filename := slug + ".json"

	// Prevent overwriting builtin templates
	if builtinFilenames[filename] {
		filename = slug + "-custom.json"
	}

	if body.Category == "" {
		body.Category = "custom"
	}
	if body.Icon == "" {
		body.Icon = "file"
	}

	data := map[string]interface{}{
		"name":        body.Name,
		"description": body.Description,
		"category":    body.Category,
		"icon":        body.Icon,
		"template":    body.Template,
	}

	jsonData, _ := json.MarshalIndent(data, "", "  ")
	fpath := filepath.Join(config.TemplatesDir, filename)
	if err := os.WriteFile(fpath, jsonData, 0644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	s := sessions.Default(c)
	username, _ := s.Get("user").(string)
	db.LogActivity(username, "template_save", filename, "Template '"+body.Name+"' sauvegardee")

	c.JSON(http.StatusOK, gin.H{"status": "ok", "filename": filename, "message": "Template '" + body.Name + "' sauvegardee"})
}

func handleDeleteTemplate(c *gin.Context) {
	filename := c.Param("filename")

	if builtinFilenames[filename] {
		c.JSON(http.StatusForbidden, gin.H{"status": "error", "error": "Impossible de supprimer une template predefinie"})
		return
	}

	fpath := filepath.Join(config.TemplatesDir, filename)
	if !strings.HasSuffix(filename, ".json") {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "Template introuvable"})
		return
	}
	if _, err := os.Stat(fpath); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "Template introuvable"})
		return
	}

	os.Remove(fpath)

	s := sessions.Default(c)
	username, _ := s.Get("user").(string)
	db.LogActivity(username, "template_delete", filename, "Template '"+filename+"' supprimee")

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Template '" + filename + "' supprimee"})
}
