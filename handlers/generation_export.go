package handlers

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"

	"autodocker-go/middleware"
	"autodocker-go/services"
	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
)

// Downloads are built in request memory. No project, owner record, temporary
// folder or server-side download token is created or looked up by name.
var generationExportSlots = make(chan struct{}, 3)

type generationExportRequest struct {
	Project string                       `json:"project"`
	Payload services.BuildComposePayload `json:"payload"`
	Deploy  bool                         `json:"deploy"`
}

func prepareGeneration(c *gin.Context) (string, services.M, []byte, bool) {
	user, _ := middleware.GetSessionUser(c)
	if user == "" {
		c.JSON(401, gin.H{"status": "error", "error": "Connexion requise"})
		return "", nil, nil, false
	}
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
	var request generationExportRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(400, gin.H{"status": "error", "error": "Demande invalide ou trop volumineuse"})
		return "", nil, nil, false
	}
	if request.Deploy {
		c.JSON(400, gin.H{"status": "error", "error": "AutoDocker génère les fichiers ; démarrez votre infrastructure sur le serveur de votre choix."})
		return "", nil, nil, false
	}
	project, err := normalizeBuilderProjectName(request.Project)
	if err != nil || len(project) > 63 || strings.ContainsAny(project[:min(len(project), 1)], "-_") {
		c.JSON(400, gin.H{"status": "error", "error": "Nom invalide : 1 à 63 minuscules, chiffres, tirets ou underscores, en commençant par une lettre ou un chiffre."})
		return "", nil, nil, false
	}
	if len(request.Payload.Services) == 0 || len(request.Payload.Services) > 100 {
		c.JSON(400, gin.H{"status": "error", "error": "Choisissez entre 1 et 100 services."})
		return "", nil, nil, false
	}
	if err := services.ValidateInfrastructureOptions(request.Payload); err != nil {
		c.JSON(400, gin.H{"status": "error", "error": err.Error()})
		return "", nil, nil, false
	}
	// Only conflicts inside this archive matter, never those of a previous
	// generation, a project on the host, or a different visitor's infrastructure.
	payload := prepareBuilderPayload(request.Payload, map[string]bool{})
	compose := services.BuildComposeForExport(payload, project)
	if svcs, _ := compose["services"].(services.M); len(svcs) == 0 {
		c.JSON(400, gin.H{"status": "error", "error": "Choisissez au moins un service valide."})
		return "", nil, nil, false
	}
	if err := services.ValidateGeneratedConnections(compose); err != nil {
		c.JSON(422, gin.H{"status": "error", "error": err.Error()})
		return "", nil, nil, false
	}
	data, err := yaml.Marshal(compose)
	if err != nil {
		c.JSON(500, gin.H{"status": "error", "error": "Impossible de générer le fichier Compose."})
		return "", nil, nil, false
	}
	return project, compose, data, true
}

func reserveGeneration(c *gin.Context) bool {
	select {
	case generationExportSlots <- struct{}{}:
		return true
	default:
		c.Header("Retry-After", "2")
		c.JSON(429, gin.H{"status": "error", "error": "La génération est occupée. Réessayez dans un instant."})
		return false
	}
}

func handleGenerationPreview(c *gin.Context) {
	if !reserveGeneration(c) {
		return
	}
	defer func() { <-generationExportSlots }()
	_, compose, data, ok := prepareGeneration(c)
	if !ok {
		return
	}
	c.JSON(200, gin.H{"status": "ok", "is_new": true, "retention": "none", "new_yaml": string(data), "ingress": compose["x-autodocker"], "diagram": services.ArchitecturePreview(compose)})
}

func handleGenerationExport(c *gin.Context) {
	if !reserveGeneration(c) {
		return
	}
	defer func() { <-generationExportSlots }()
	project, compose, data, ok := prepareGeneration(c)
	if !ok {
		return
	}
	files := generatedProjectFiles(compose)
	files["compose.yaml"] = string(data)
	files["GUIDE-AUTODOCKER.md"] = "# Votre infrastructure " + project + "\n\nDécompressez cette archive sur votre machine ou serveur cible. Vérifiez les ports, les accès et les prérequis de vos services, puis lancez `docker compose up -d` depuis ce dossier. Les scripts up.sh et down.sh servent à démarrer et arrêter la pile.\n\nAutoDocker a généré les fichiers sans enregistrer de projet ni démarrer de conteneur. Téléchargez cette archive avant de quitter la page. Vous pouvez réutiliser ce nom pour une autre génération. Pour faire tourner plusieurs copies sur une même machine, donnez-leur des noms Compose et des ports distincts.\n"
	files["GUIDE-AUTODOCKER.md"] += services.GeneratedSecurityGuide(compose)
	archive, err := generationArchive(files)
	if err != nil {
		c.JSON(422, gin.H{"status": "error", "error": "L’archive ne peut pas être préparée ou dépasse la taille autorisée."})
		return
	}
	if c.Request.Context().Err() != nil {
		return
	}
	c.JSON(200, gin.H{"status": "ok", "project": project, "deploy": false, "retention": "none", "archive_base64": archive, "compose": string(data)})
}

// The persistent builder and stateless exporter use the same file generators.
func generatedProjectFiles(compose services.M) map[string]string {
	files := services.Scripts()
	for _, more := range []map[string]string{services.ClientDeliveryProjectFiles(compose), services.PrivateAIProjectFiles(compose), services.MinecraftAIProjectFiles(compose)} {
		for name, content := range more {
			files[name] = content
		}
	}
	svcs, _ := compose["services"].(services.M)
	if _, ok := svcs["debian-router"]; ok {
		files["router/bootstrap-router.sh"] = services.RouterScript()
	}
	if _, ok := svcs["fail2ban"]; ok {
		files["jail.local"] = services.Fail2banJail()
		files["auth.log"] = ""
	}
	return files
}

type boundedGenerationBuffer struct{ bytes.Buffer }

func (b *boundedGenerationBuffer) Write(data []byte) (int, error) {
	if b.Len()+len(data) > 8<<20 {
		return 0, io.ErrShortWrite
	}
	return b.Buffer.Write(data)
}

func generationArchive(files map[string]string) ([]byte, error) {
	if len(files) > 256 {
		return nil, fmt.Errorf("too many generated files")
	}
	names := make([]string, 0, len(files))
	total := 0
	for name, content := range files {
		if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:\x00") || name == ".." || strings.HasPrefix(name, "../") {
			return nil, fmt.Errorf("invalid archive entry")
		}
		total += len(content)
		if total > 16<<20 {
			return nil, fmt.Errorf("generated files too large")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var buffer boundedGenerationBuffer
	writer := zip.NewWriter(&buffer)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		mode := os.FileMode(0600)
		if strings.HasSuffix(name, ".sh") {
			mode = 0700
		}
		header.SetMode(mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			return nil, err
		}
		if _, err = io.WriteString(entry, files[name]); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
