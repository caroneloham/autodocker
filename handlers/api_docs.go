package handlers

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// RegisterAPIDocsRoutes registers the Swagger UI and OpenAPI spec endpoints.
func RegisterAPIDocsRoutes(r *gin.Engine) {
	r.GET("/api/docs", swaggerUI)
	r.GET("/api/docs/spec", openAPISpec)
}

func swaggerUI(c *gin.Context) {
	html := `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>AutoDocker API</title>
<link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
<style>
body{margin:0;background:#0f172a;}
.swagger-ui .topbar{background:#1e293b !important; padding:8px 20px;}
.swagger-ui .topbar .download-url-wrapper{display:none;}
.swagger-ui .info .title{color:#38bdf8 !important; font-size:2rem !important;}
.swagger-ui .info .description p{color:#94a3b8 !important;}
.swagger-ui .scheme-container{background:#1e293b !important; box-shadow:none !important;}
.swagger-ui .opblock-tag{color:#e2e8f0 !important; border-color:rgba(255,255,255,0.06) !important;}
.swagger-ui .opblock .opblock-summary{border-color:rgba(255,255,255,0.06) !important;}
.swagger-ui .opblock .opblock-summary-description{color:#94a3b8 !important;}
.swagger-ui .btn{border-radius:6px !important;}
.swagger-ui section.models{display:none !important;}
#swagger-ui{max-width:1200px;margin:0 auto;padding:20px;}
</style>
</head><body><div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>SwaggerUIBundle({url:'/api/docs/spec',dom_id:'#swagger-ui',deepLinking:true,presets:[SwaggerUIBundle.presets.apis],layout:'BaseLayout',defaultModelsExpandDepth:-1,docExpansion:'list',tagsSorter:'alpha',operationsSorter:'alpha'});</script>
</body></html>`
	c.Header("Content-Type", "text/html")
	c.String(http.StatusOK, html)
}

// Manually documented endpoints with descriptions, params, request/response bodies
type apiEndpoint struct {
	Method      string
	Path        string
	Tag         string
	Summary     string
	Description string
	Params      []apiParam
	Body        map[string]interface{}
	Response    map[string]interface{}
}
type apiParam struct {
	Name     string
	In       string // "query", "path"
	Required bool
	Type     string
	Desc     string
}

func documentedEndpoints() []apiEndpoint {
	return []apiEndpoint{
		// ---- Auth ----
		{Method: "post", Path: "/login", Tag: "🔐 Authentification", Summary: "Connexion", Description: "Authentifie un utilisateur avec identifiant et mot de passe. Retourne un cookie de session.",
			Body: jsonBody("username", "password"), Response: jsonResp("Redirect vers /")},
		{Method: "get", Path: "/logout", Tag: "🔐 Authentification", Summary: "Deconnexion", Description: "Detruit la session et redirige vers /login."},
		{Method: "get", Path: "/api/check-session", Tag: "🔐 Authentification", Summary: "Verifier la session", Description: "Retourne l'utilisateur connecte, son role et un token CSRF.",
			Response: jsonFields("user", "role", "csrf")},

		// ---- Dashboard ----
		{Method: "get", Path: "/api/refresh", Tag: "📊 Dashboard", Summary: "Liste des projets et conteneurs", Description: "Retourne tous les projets avec leurs conteneurs, etats, images et ports. Filtre par RBAC.",
			Response: jsonArray("name", "status", "containers[]")},
		{Method: "get", Path: "/api/docker/status", Tag: "📊 Dashboard", Summary: "Etat Docker Desktop", Description: "Indique si Docker est disponible et retourne les actions conseillees quand il ne repond pas.",
			Response: jsonFields("status", "available", "title", "message", "actions")},
		{Method: "post", Path: "/api/docker/open-desktop", Tag: "📊 Dashboard", Summary: "Ouvrir Docker Desktop", Description: "Tente de lancer Docker Desktop depuis l'installation Windows locale.",
			Response: jsonFields("status", "message", "path")},
		{Method: "get", Path: "/api/project/{name}/log-summary", Tag: "📊 Dashboard", Summary: "Résumé des logs projet", Description: "Resume les derniers logs centralises d'un projet en compteurs lisibles et lignes importantes.",
			Response: jsonFields("summary", "counts", "highlights", "recommended_action")},
		{Method: "post", Path: "/api/delete_project", Tag: "📊 Dashboard", Summary: "Supprimer un projet", Description: "Supprime le dossier local, les permissions et les conteneurs du projet quand Docker est disponible. Les volumes Docker sont conserves sauf si remove_volumes vaut true.",
			Body: jsonBody("name", "remove_volumes"), Response: jsonFields("status", "docker_available", "containers_removed", "volumes_removed", "volumes_requested")},
		{Method: "post", Path: "/api/bulk-action", Tag: "📊 Dashboard", Summary: "Action groupee", Description: "Start ou stop tous les conteneurs d'un projet.",
			Body: jsonBody("project", "action")},

		// ---- Builder / Deploy ----
		{Method: "post", Path: "/api/deploy", Tag: "🚀 Builder", Summary: "Deployer une infrastructure", Description: "Genere un compose.yaml et deploie les conteneurs. Accepte un payload avec services, reseaux et adresses.",
			Body: jsonBody("project", "payload", "deploy"), Response: jsonFields("status", "project", "path", "logs")},
		{Method: "post", Path: "/api/deploy/preview", Tag: "🚀 Builder", Summary: "Preview avant deploiement", Description: "Affiche un diff des changements sans deployer.",
			Body: jsonBody("project", "payload"), Response: jsonFields("is_new", "diff", "summary")},
		{Method: "post", Path: "/api/ai-generate", Tag: "🚀 Builder", Summary: "Generer une architecture via IA", Description: "Envoie un prompt a Gemini qui retourne un payload deployable.",
			Body: jsonBody("prompt"), Response: jsonFields("status", "payload")},
		{Method: "post", Path: "/api/ai-provision", Tag: "🚀 Builder", Summary: "Configurer les services via IA", Description: "L'IA genere et injecte des fichiers de configuration (HTML, CSS, SQL...) dans les conteneurs d'un projet deploye.",
			Body: jsonBody("project", "prompt"), Response: jsonFields("status", "summary", "files[]", "total")},
		{Method: "get", Path: "/api/catalogue", Tag: "🚀 Builder", Summary: "Catalogue des services", Description: "Liste tous les services disponibles (base + custom).",
			Response: jsonFields("base[]", "custom[]")},
		{Method: "post", Path: "/api/catalogue/add", Tag: "🚀 Builder", Summary: "Ajouter un service custom", Description: "Ajoute un service au catalogue (admin).",
			Body: jsonBody("key", "image", "category", "description")},
		{Method: "post", Path: "/api/canvas/import", Tag: "🚀 Builder", Summary: "Importer un projet sur le canvas", Description: "Parse le compose.yaml d'un projet et retourne les noeuds et liens pour le canvas visuel.",
			Body: jsonBody("project"), Response: jsonFields("nodes[]", "links[]")},

		// ---- Containers ----
		{Method: "get", Path: "/api/container/{cid}/logs", Tag: "🐳 Conteneurs", Summary: "Logs d'un conteneur", Description: "Retourne les dernieres lignes de logs.",
			Params:   []apiParam{{Name: "cid", In: "path", Required: true, Type: "string", Desc: "ID du conteneur"}},
			Response: jsonFields("name", "logs")},
		{Method: "get", Path: "/api/container/{cid}/stats", Tag: "🐳 Conteneurs", Summary: "Stats CPU/RAM", Description: "Retourne CPU%, RAM%, RAM MB d'un conteneur.",
			Response: jsonFields("cpu_percent", "mem_percent", "mem_mb")},
		{Method: "get", Path: "/api/container/{cid}/inspect", Tag: "🐳 Conteneurs", Summary: "Inspection detaillee", Description: "Informations completes : reseaux, ports, volumes, env vars, labels.",
			Response: jsonFields("info{}")},
		{Method: "post", Path: "/api/action", Tag: "🐳 Conteneurs", Summary: "Action sur un conteneur", Description: "Start, stop, restart ou remove un conteneur.",
			Body: jsonBody("id", "action")},

		// ---- Supervision ----
		{Method: "get", Path: "/api/supervision", Tag: "📈 Supervision", Summary: "Metriques temps reel", Description: "CPU, RAM, reseau de tous les conteneurs + resume global + agents.",
			Response: jsonFields("summary{}", "containers[]", "agents[]")},
		{Method: "get", Path: "/api/supervision/rules", Tag: "📈 Supervision", Summary: "Regles d'alertes", Description: "Liste les regles de supervision configurees."},
		{Method: "post", Path: "/api/supervision/rules", Tag: "📈 Supervision", Summary: "Creer/modifier une regle", Description: "Ajoute ou modifie une regle d'alerte.",
			Body: jsonBody("id", "name", "enabled", "target", "conditions[]", "actions[]", "severity")},
		{Method: "post", Path: "/api/agents/generate", Tag: "📈 Supervision", Summary: "Generer un agent", Description: "Cree un agent de supervision et retourne un .exe ou .py a installer sur une machine distante.",
			Body: jsonBody("name", "format", "server_url")},

		// ---- Copilot ----
		{Method: "post", Path: "/api/copilot/chat", Tag: "🤖 Copilot IA", Summary: "Chat avec le copilot", Description: "Envoie un message au copilot IA qui connait toute l'infrastructure. Providers: local, groq, mistral, openrouter.",
			Body: jsonBody("message", "provider"), Response: jsonFields("response", "provider")},
		{Method: "get", Path: "/api/copilot/status", Tag: "🤖 Copilot IA", Summary: "Status des providers IA", Description: "Retourne quels providers IA sont configures.",
			Response: jsonFields("providers{}")},

		// ---- Security ----
		{Method: "post", Path: "/api/tools/vuln-scan", Tag: "🛡️ Securite", Summary: "Scanner de vulnerabilites (Trivy)", Description: "Scanne toutes les images Docker avec Trivy et retourne les CVE par image.",
			Response: jsonFields("mode", "version", "results[]")},
		{Method: "get", Path: "/api/tools/audit-history", Tag: "🛡️ Securite", Summary: "Historique des audits CIS", Description: "Liste les audits de securite CIS Docker Benchmark."},
		{Method: "get", Path: "/api/secrets/{project}", Tag: "🛡️ Securite", Summary: "Lister les secrets d'un projet", Description: "Retourne les secrets (valeurs masquees) d'un projet.",
			Params: []apiParam{{Name: "project", In: "path", Required: true, Type: "string", Desc: "Nom du projet"}}},
		{Method: "post", Path: "/api/secrets/{project}", Tag: "🛡️ Securite", Summary: "Ajouter un secret", Description: "Chiffre et stocke un secret pour un projet.",
			Body: jsonBody("key_name", "value")},
		{Method: "get", Path: "/api/anomalies", Tag: "🛡️ Securite", Summary: "Anomalies detectees", Description: "Liste les anomalies CPU/RAM basees sur l'analyse statistique (z-score).",
			Response: jsonFields("anomalies[]", "count")},

		// ---- Network ----
		{Method: "get", Path: "/api/network-map", Tag: "🔌 Reseau", Summary: "Carte reseau", Description: "Retourne la topologie reseau Docker (reseaux, conteneurs, connexions).",
			Response: jsonFields("networks[]", "containers[]")},
		{Method: "post", Path: "/api/trace", Tag: "🔌 Reseau", Summary: "Tracing distribue", Description: "Mesure la latence entre les services d'un projet (waterfall).",
			Body: jsonBody("project"), Response: jsonFields("hops[]", "total_services")},

		// ---- Infra ----
		{Method: "get", Path: "/api/images", Tag: "📦 Infrastructure", Summary: "Lister les images Docker", Description: "Retourne toutes les images Docker locales."},
		{Method: "get", Path: "/api/images/check-updates", Tag: "📦 Infrastructure", Summary: "Verifier les mises a jour", Description: "Compare les images locales avec Docker Hub."},
		{Method: "get", Path: "/api/project/{name}/versions", Tag: "📦 Infrastructure", Summary: "Historique des versions", Description: "Liste les snapshots compose.yaml d'un projet.",
			Params: []apiParam{{Name: "name", In: "path", Required: true, Type: "string", Desc: "Nom du projet"}}},
		{Method: "post", Path: "/api/project/{name}/rollback", Tag: "📦 Infrastructure", Summary: "Rollback", Description: "Restaure une version anterieure du compose et redeploie.",
			Body: jsonBody("filename")},

		// ---- Monitoring ----
		{Method: "get", Path: "/api/logs/search", Tag: "📊 Monitoring", Summary: "Recherche de logs", Description: "Recherche full-text dans les logs centralises avec filtres.",
			Params: []apiParam{
				{Name: "q", In: "query", Type: "string", Desc: "Texte a rechercher"},
				{Name: "container", In: "query", Type: "string", Desc: "Filtre par conteneur"},
				{Name: "severity", In: "query", Type: "string", Desc: "info, warning, error"},
				{Name: "since", In: "query", Type: "string", Desc: "Date debut"},
			}},
		{Method: "get", Path: "/api/logs/correlate", Tag: "📊 Monitoring", Summary: "Correlation de logs", Description: "Trouve les logs des autres conteneurs du meme projet autour d'un timestamp donne.",
			Params: []apiParam{
				{Name: "container", In: "query", Required: true, Type: "string", Desc: "Nom du conteneur source"},
				{Name: "timestamp", In: "query", Required: true, Type: "string", Desc: "Timestamp (2006-01-02 15:04:05)"},
				{Name: "window", In: "query", Type: "string", Desc: "Fenetre en secondes (defaut: 5)"},
			}},
		{Method: "get", Path: "/api/metrics/history", Tag: "📊 Monitoring", Summary: "Historique metriques", Description: "Time-series CPU/RAM/reseau pour un conteneur.",
			Params: []apiParam{
				{Name: "source_id", In: "query", Required: true, Type: "string", Desc: "ID du conteneur"},
				{Name: "range", In: "query", Type: "string", Desc: "1h, 6h, 24h, 7d, 30d"},
			}},
		{Method: "get", Path: "/api/health/dashboard", Tag: "📊 Monitoring", Summary: "Dashboard sante", Description: "Score de sante par projet (vert, orange, rouge).",
			Response: jsonFields("data[]")},
		{Method: "get", Path: "/api/scheduled-tasks", Tag: "📊 Monitoring", Summary: "Taches planifiees", Description: "Liste les taches cron planifiees.",
			Params: []apiParam{{Name: "project", In: "query", Type: "string", Desc: "Filtre par projet"}}},
		{Method: "post", Path: "/api/scheduled-tasks", Tag: "📊 Monitoring", Summary: "Creer une tache", Description: "Planifie une tache (backup, restart, nettoyage) avec un preset horaire.",
			Body: jsonBody("project", "container_name", "task_type", "schedule_preset")},

		// ---- QR / Magic ----
		{Method: "get", Path: "/api/magic/qrcode", Tag: "✨ Magic", Summary: "Generer un QR Code", Description: "Retourne une image PNG du QR code pour l'URL donnee.",
			Params: []apiParam{{Name: "url", In: "query", Required: true, Type: "string", Desc: "URL a encoder"}}},
		{Method: "post", Path: "/api/magic/upload-image", Tag: "✨ Magic", Summary: "Uploader des images", Description: "Upload des images dans le conteneur nginx d'un projet.",
			Body: jsonBody("project", "images (multipart)")},

		// ---- Admin ----
		{Method: "get", Path: "/api/admin/list-users", Tag: "👤 Admin", Summary: "Lister les utilisateurs", Description: "Retourne tous les utilisateurs avec leurs roles."},
		{Method: "post", Path: "/api/admin/add-user", Tag: "👤 Admin", Summary: "Creer un utilisateur", Description: "Cree un nouveau compte.",
			Body: jsonBody("username", "password", "role")},
		{Method: "post", Path: "/api/admin/delete-user", Tag: "👤 Admin", Summary: "Supprimer un utilisateur",
			Body: jsonBody("username")},
	}
}

func jsonBody(fields ...string) map[string]interface{} {
	props := map[string]interface{}{}
	for _, f := range fields {
		props[f] = map[string]string{"type": "string"}
	}
	return map[string]interface{}{
		"content": map[string]interface{}{
			"application/json": map[string]interface{}{
				"schema": map[string]interface{}{"type": "object", "properties": props},
			},
		},
	}
}

func jsonResp(desc string) map[string]interface{} {
	return map[string]interface{}{"200": map[string]string{"description": desc}}
}

func jsonFields(fields ...string) map[string]interface{} {
	return map[string]interface{}{"200": map[string]interface{}{
		"description": "Succes",
		"content": map[string]interface{}{
			"application/json": map[string]interface{}{
				"schema": map[string]interface{}{"type": "object",
					"properties": func() map[string]interface{} {
						p := map[string]interface{}{}
						for _, f := range fields {
							p[f] = map[string]string{"type": "string"}
						}
						return p
					}()},
			},
		},
	}}
}

func jsonArray(fields ...string) map[string]interface{} {
	return jsonFields(fields...)
}

func openAPISpec(c *gin.Context) {
	endpoints := documentedEndpoints()

	// Also add auto-discovered routes not in the manual list
	engine, _ := c.Get("gin_engine")
	autoRoutes := map[string]bool{}
	for _, ep := range endpoints {
		autoRoutes[ep.Method+":"+ep.Path] = true
	}

	if eng, ok := engine.(*gin.Engine); ok {
		for _, route := range eng.Routes() {
			if !strings.HasPrefix(route.Path, "/api/") {
				continue
			}
			path := route.Path
			path = strings.ReplaceAll(path, ":cid", "{cid}")
			path = strings.ReplaceAll(path, ":id", "{id}")
			path = strings.ReplaceAll(path, ":name", "{name}")
			path = strings.ReplaceAll(path, ":key", "{key}")
			path = strings.ReplaceAll(path, ":project", "{project}")
			path = strings.ReplaceAll(path, ":host_id", "{host_id}")
			path = strings.ReplaceAll(path, ":chain", "{chain}")
			path = strings.ReplaceAll(path, ":num", "{num}")
			path = strings.ReplaceAll(path, ":username", "{username}")
			path = strings.ReplaceAll(path, ":port", "{port}")
			method := strings.ToLower(route.Method)
			key := method + ":" + path
			if autoRoutes[key] {
				continue
			}
			if path == "/api/docs" || path == "/api/docs/spec" {
				continue
			}

			handler := route.Handler
			if idx := strings.LastIndex(handler, "."); idx >= 0 {
				handler = handler[idx+1:]
			}

			endpoints = append(endpoints, apiEndpoint{
				Method: method, Path: path, Tag: "🔧 Autre",
				Summary: handler, Description: "Endpoint auto-detecte.",
			})
		}
	}

	// Build OpenAPI paths
	paths := map[string]interface{}{}
	for _, ep := range endpoints {
		if paths[ep.Path] == nil {
			paths[ep.Path] = map[string]interface{}{}
		}
		pathMap := paths[ep.Path].(map[string]interface{})

		operation := map[string]interface{}{
			"summary":     ep.Summary,
			"description": ep.Description,
			"tags":        []string{ep.Tag},
			"responses":   map[string]interface{}{"200": map[string]string{"description": "Succes"}},
		}

		if len(ep.Params) > 0 {
			params := []map[string]interface{}{}
			for _, p := range ep.Params {
				params = append(params, map[string]interface{}{
					"name": p.Name, "in": p.In, "required": p.Required,
					"description": p.Desc,
					"schema":      map[string]string{"type": p.Type},
				})
			}
			operation["parameters"] = params
		}
		if ep.Body != nil {
			operation["requestBody"] = ep.Body
		}
		if ep.Response != nil {
			operation["responses"] = ep.Response
		}

		pathMap[ep.Method] = operation
	}

	// Sort
	sortedPaths := map[string]interface{}{}
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sortedPaths[k] = paths[k]
	}

	spec := gin.H{
		"openapi": "3.0.3",
		"info": gin.H{
			"title":       "AutoDocker API",
			"description": "API REST complete pour la gestion d'infrastructures Docker.\n\n**Features:** Deploy, Canvas, IA (Gemini/Groq/Mistral), Supervision, Securite (Trivy), Copilot, Backups, Multi-Host, Tracing, Anomalies.\n\n**Auth:** Cookie de session via POST /login",
			"version":     "2.1.0",
			"contact": gin.H{
				"name": "AutoDocker",
			},
		},
		"servers": []gin.H{
			{"url": "/", "description": "Serveur local"},
		},
		"paths": sortedPaths,
	}
	c.JSON(http.StatusOK, spec)
}
