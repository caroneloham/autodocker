package handlers

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/gin-gonic/gin"
)

// RegisterContainerRoutes registers all container-level API routes.
func RegisterContainerRoutes(api *gin.RouterGroup) {
	api.GET("/container/:cid/logs", handleContainerLogs)
	api.GET("/container/:cid/files", handleContainerFiles)
	api.GET("/container/:cid/file", handleContainerFileRead)
	api.POST("/container/:cid/file", handleContainerFileWrite)
	api.GET("/container/:cid/browse", handleContainerBrowse)
	api.POST("/container/:cid/upload", handleContainerUpload)
	api.POST("/container/:cid/ai-edit", handleContainerFileAIEdit)
	api.POST("/action", handleContainerAction)
	api.GET("/container/:cid/inspect", handleContainerInspect)
	api.GET("/container/:cid/stats", handleContainerStats)
	api.GET("/project/:name/compose", handleProjectCompose)
	api.POST("/project/:name/export", handleProjectExport)
}

// ---------------------------------------------------------------------------
// AI file edit — modify file content with a natural language instruction.
// Called by the "IA Go" button of the container file editor (Outils > Fichiers).
// ---------------------------------------------------------------------------

func handleContainerFileAIEdit(c *gin.Context) {
	var req struct {
		Content     string `json:"content"`
		Instruction string `json:"instruction"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Instruction) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Contenu et instruction requis"})
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Contenu du fichier vide"})
		return
	}

	edited, err := services.EditFileWithAI(req.Content, req.Instruction)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur IA: " + err.Error()})
		return
	}

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "file-ai-edit", c.Param("cid"), truncate(req.Instruction, 120))
	c.JSON(http.StatusOK, gin.H{"status": "ok", "content": edited})
}

// ---------------------------------------------------------------------------
// Logs
// ---------------------------------------------------------------------------

func handleContainerLogs(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}

	cid := c.Param("cid")
	tail := c.DefaultQuery("tail", "200")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reader, err := cli.ContainerLogs(ctx, cid, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tail,
		Timestamps: true,
	})
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Conteneur introuvable"})
		return
	}
	defer reader.Close()

	data, _ := io.ReadAll(reader)
	// Strip Docker log header bytes (8-byte prefix per line)
	logs := stripDockerLogHeaders(data)

	// Get container name
	info, err := cli.ContainerInspect(ctx, cid)
	name := cid
	if err == nil {
		name = strings.TrimPrefix(info.Name, "/")
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "name": name, "logs": logs})
}

// stripDockerLogHeaders removes the 8-byte Docker log stream header from each line.
func stripDockerLogHeaders(data []byte) string {
	var result strings.Builder
	for len(data) > 0 {
		if len(data) < 8 {
			result.Write(data)
			break
		}
		// Docker multiplexed stream: first byte is stream type, bytes 4-7 are payload size (big-endian)
		size := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
		data = data[8:]
		if size > len(data) {
			size = len(data)
		}
		result.Write(data[:size])
		data = data[size:]
	}
	return result.String()
}

// ---------------------------------------------------------------------------
// Files — list known config files for a container
// ---------------------------------------------------------------------------

// fileDB maps image name substrings to lists of important config files.
var fileDB = map[string][]string{
	"nginx":      {"/etc/nginx/nginx.conf", "/etc/nginx/conf.d/default.conf", "/usr/share/nginx/html/index.html", "/usr/share/nginx/html/styles.css", "/usr/share/nginx/html/script.js"},
	"wordpress":  {"/var/www/html/wp-config.php", "/var/www/html/index.php", "/var/www/html/.htaccess"},
	"mariadb":    {"/etc/mysql/my.cnf"},
	"postgres":   {"/var/lib/postgresql/data/postgresql.conf"},
	"nextcloud":  {"/var/www/html/config/config.php"},
	"traefik":    {"/etc/traefik/traefik.yml"},
	"prometheus": {"/etc/prometheus/prometheus.yml"},
	"grafana":    {"/etc/grafana/grafana.ini"},
	"apache":     {"/etc/apache2/apache2.conf", "/var/www/html/index.html", "/var/www/html/styles.css"},
	"httpd":      {"/usr/local/apache2/conf/httpd.conf", "/usr/local/apache2/htdocs/index.html"},
}

func handleContainerFiles(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}

	cid := c.Param("cid")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := cli.ContainerInspect(ctx, cid)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Conteneur introuvable"})
		return
	}

	img := strings.ToLower(info.Config.Image)
	files := []string{"/etc/hosts"}

	for key, paths := range fileDB {
		if strings.Contains(img, key) {
			files = append(paths, files...)
			break
		}
	}

	// Include the compose config path if available
	if info.Config.Labels != nil {
		if cfgPath, ok := info.Config.Labels["com.docker.compose.project.config_files"]; ok {
			parts := strings.Split(cfgPath, ",")
			if len(parts) > 0 && parts[0] != "" {
				if _, err := os.Stat(parts[0]); err == nil {
					files = append([]string{parts[0]}, files...)
				}
			}
		}
	}

	name := strings.TrimPrefix(info.Name, "/")
	c.JSON(http.StatusOK, gin.H{"status": "ok", "name": name, "image": img, "files": files})
}

// ---------------------------------------------------------------------------
// File read
// ---------------------------------------------------------------------------

func handleContainerFileRead(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}

	cid := c.Param("cid")
	filePath := c.Query("path")
	if filePath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Chemin manquant"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check if it's a local file in the projects dir
	if isSafeLocalPath(filePath) {
		if data, err := os.ReadFile(filePath); err == nil {
			c.JSON(http.StatusOK, gin.H{"status": "ok", "content": string(data), "source": "local"})
			return
		}
	}

	// Read from container using tar archive
	reader, _, err := cli.CopyFromContainer(ctx, cid, filePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer reader.Close()

	tr := tar.NewReader(reader)
	if _, err := tr.Next(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur lecture archive tar"})
		return
	}
	content, err := io.ReadAll(tr)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur lecture fichier"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "content": string(content), "source": "container"})
}

// ---------------------------------------------------------------------------
// File write
// ---------------------------------------------------------------------------

func handleContainerFileWrite(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		writeLifecycleDockerUnavailable(c)
		return
	}
	handleContainerFileWriteUsing(c, cli)
}

type containerFileWriteClient interface {
	CopyToContainer(context.Context, string, string, io.Reader, container.CopyToContainerOptions) error
	ContainerRestart(context.Context, string, container.StopOptions) error
}

func handleContainerFileWriteUsing(c *gin.Context, cli containerFileWriteClient) {

	cid := c.Param("cid")
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Chemin manquant"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// If local file in projects dir
	if isSafeLocalPath(req.Path) {
		if _, err := os.Stat(req.Path); err == nil {
			if err := os.WriteFile(req.Path, []byte(req.Content), 0644); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Fichier local sauvegarde"})
			return
		}
	}

	// Write to container via tar
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	encoded := []byte(req.Content)
	hdr := &tar.Header{
		Name: filepath.Base(req.Path),
		Mode: 0644,
		Size: int64(len(encoded)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if _, err := tw.Write(encoded); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	tw.Close()

	dir := filepath.Dir(req.Path)
	if dir == "" || dir == "." {
		dir = "/"
	}
	// Use forward slashes for container paths
	dir = strings.ReplaceAll(dir, "\\", "/")

	if err := cli.CopyToContainer(ctx, cid, dir, &buf, container.CopyToContainerOptions{}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Restart gets its own bounded operation window after copying the file.
	// Reusing the 10-second copy deadline can cancel a normal graceful restart.
	cancel()
	restartCtx, cancelRestart := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancelRestart()
	timeout := 10
	if err := cli.ContainerRestart(restartCtx, cid, container.StopOptions{Timeout: &timeout}); err != nil {
		c.JSON(lifecycleErrorStatus(err), gin.H{
			"status": "partial", "code": "container_restart_failed",
			"error": "Fichier sauvegarde, mais le redemarrage du conteneur a echoue : " + err.Error(),
			"saved": true, "restarted": false,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Fichier sauvegarde et conteneur redemarre", "saved": true, "restarted": true})
}

// ---------------------------------------------------------------------------
// Container actions (start/stop/restart/remove)
// ---------------------------------------------------------------------------

func handleContainerAction(c *gin.Context) {
	handleContainerActionUsing(c, func() containerActionClient {
		if cli := services.EnsureDocker(); cli != nil {
			return cli
		}
		return nil
	})
}

type containerActionClient interface {
	ContainerInspect(context.Context, string) (types.ContainerJSON, error)
	ContainerStart(context.Context, string, container.StartOptions) error
	ContainerStop(context.Context, string, container.StopOptions) error
	ContainerRestart(context.Context, string, container.StopOptions) error
	ContainerRemove(context.Context, string, container.RemoveOptions) error
}

func handleContainerActionUsing(c *gin.Context, resolveDocker func() containerActionClient) {
	var req struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "JSON invalide"})
		return
	}

	req.ID = strings.TrimSpace(req.ID)
	validActions := map[string]bool{"start": true, "stop": true, "restart": true, "remove": true}
	if req.ID == "" || !validActions[req.Action] {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Conteneur et action valides requis"})
		return
	}
	if err := c.Request.Context().Err(); err != nil {
		writeLifecycleActionError(c, err, "Action annulee")
		return
	}
	cli := resolveDocker()
	if cli == nil {
		writeLifecycleDockerUnavailable(c)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	// Get container name before action for logging
	containerName := req.ID
	info, err := cli.ContainerInspect(ctx, req.ID)
	if err != nil {
		writeLifecycleActionError(c, err, "Lecture de l'etat du conteneur impossible")
		return
	}
	if info.ContainerJSONBase == nil {
		writeLifecycleActionError(c, errors.New("reponse Docker incomplete"), "Lecture de l'etat du conteneur impossible")
		return
	}
	containerName = strings.TrimPrefix(info.Name, "/")

	// Check project-level permissions
	user, role := middleware.GetSessionUser(c)
	if !middleware.IsAdminOrAbove(role) {
		if info.Config != nil && info.Config.Labels != nil {
			if proj, ok := info.Config.Labels["com.docker.compose.project"]; ok && proj != "" {
				projRole := getUserRoleOnProject(proj, user)
				if projRole != "write" {
					c.JSON(http.StatusForbidden, gin.H{"error": "Permission refusee sur ce projet"})
					return
				}
			}
		}
	}

	timeout := 10
	switch req.Action {
	case "start":
		err = cli.ContainerStart(ctx, req.ID, container.StartOptions{})
	case "stop":
		err = cli.ContainerStop(ctx, req.ID, container.StopOptions{Timeout: &timeout})
	case "restart":
		err = cli.ContainerRestart(ctx, req.ID, container.StopOptions{Timeout: &timeout})
	case "remove":
		err = cli.ContainerRemove(ctx, req.ID, container.RemoveOptions{Force: true})
	}

	if err != nil {
		writeLifecycleActionError(c, err, "Action impossible")
		return
	}

	logTarget := containerName
	if req.Action == "remove" {
		logTarget = req.ID
	}
	db.LogActivity(user, req.Action, logTarget, "")
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func lifecycleErrorStatus(err error) int {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout
	case errdefs.IsNotFound(err):
		return http.StatusNotFound
	case errdefs.IsConflict(err):
		return http.StatusConflict
	case errdefs.IsInvalidParameter(err):
		return http.StatusBadRequest
	case errdefs.IsUnavailable(err):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

func writeLifecycleActionError(c *gin.Context, err error, message string) {
	c.JSON(lifecycleErrorStatus(err), gin.H{"status": "error", "code": "container_action_failed", "error": message + " : " + err.Error()})
}

func writeLifecycleDockerUnavailable(c *gin.Context) {
	payload := dockerUnavailablePayload()
	payload["status"] = "error"
	payload["error"] = payload["message"]
	c.JSON(http.StatusServiceUnavailable, payload)
}

type containerBulkClient interface {
	ContainerList(context.Context, container.ListOptions) ([]types.Container, error)
	ContainerStart(context.Context, string, container.StartOptions) error
	ContainerStop(context.Context, string, container.StopOptions) error
	ContainerRestart(context.Context, string, container.StopOptions) error
}

func handleBulkActionUsing(c *gin.Context, resolveDocker func() containerBulkClient) {
	var req struct {
		Project string `json:"project"`
		Action  string `json:"action"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "JSON invalide"})
		return
	}
	project := strings.ToLower(strings.TrimSpace(req.Project))
	action := req.Action
	if project == "" || (action != "start" && action != "stop" && action != "restart") {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Parametres invalides"})
		return
	}
	if err := c.Request.Context().Err(); err != nil {
		writeLifecycleActionError(c, err, "Action annulee")
		return
	}
	cli := resolveDocker()
	if cli == nil {
		writeLifecycleDockerUnavailable(c)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		writeLifecycleActionError(c, err, "Liste des conteneurs indisponible")
		return
	}
	changed, skipped, failed := 0, 0, 0
	results := []gin.H{}
	var firstErr error
	for _, ctr := range containers {
		if !strings.EqualFold(ctr.Labels["com.docker.compose.project"], project) {
			continue
		}
		item := gin.H{"id": ctr.ID, "name": readableContainerName(ctr.Names, ctr.ID)}
		if (action == "start" && ctr.State == "running") || (action == "stop" && ctr.State != "running" && ctr.State != "paused") {
			skipped++
			item["status"] = "unchanged"
			results = append(results, item)
			continue
		}
		actionErr := ctx.Err()
		if actionErr == nil {
			timeout := 10
			switch action {
			case "start":
				actionErr = cli.ContainerStart(ctx, ctr.ID, container.StartOptions{})
			case "stop":
				actionErr = cli.ContainerStop(ctx, ctr.ID, container.StopOptions{Timeout: &timeout})
			case "restart":
				actionErr = cli.ContainerRestart(ctx, ctr.ID, container.StopOptions{Timeout: &timeout})
			}
		}
		if actionErr != nil {
			failed++
			item["status"] = "error"
			item["error"] = actionErr.Error()
			if firstErr == nil {
				firstErr = actionErr
			}
		} else {
			changed++
			item["status"] = "ok"
		}
		results = append(results, item)
	}
	if len(results) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "code": "project_has_no_containers", "error": "Aucun conteneur trouve pour ce projet. Preparez ou demarrez sa configuration depuis la fiche projet.", "message": "Aucun conteneur trouve pour ce projet."})
		return
	}
	status, statusCode := "ok", http.StatusOK
	verb := map[string]string{"start": "demarre(s)", "stop": "arrete(s)", "restart": "redemarre(s)"}[action]
	message := fmt.Sprintf("%d conteneur(s) %s, %d deja dans l'etat demande.", changed, verb, skipped)
	if failed > 0 {
		status, statusCode = "partial", http.StatusMultiStatus
		message = fmt.Sprintf("%d action(s) reussie(s), %d en echec, %d sans changement.", changed, failed, skipped)
		if changed == 0 {
			status, statusCode = "error", lifecycleErrorStatus(firstErr)
		}
	}
	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "bulk-"+action, project, message)
	payload := gin.H{"status": status, "message": message, "project": project, "action": action, "changed": changed, "skipped": skipped, "failed": failed, "results": results}
	if failed > 0 {
		payload["error"] = message
		payload["code"] = "container_action_failed"
	}
	c.JSON(statusCode, payload)
}

// ---------------------------------------------------------------------------
// Inspect
// ---------------------------------------------------------------------------

func handleContainerInspect(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}

	cid := c.Param("cid")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ctr, err := cli.ContainerInspect(ctx, cid)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Conteneur introuvable"})
		return
	}

	// Networks
	networks := make([]gin.H, 0)
	if ctr.NetworkSettings != nil {
		for netName, netCfg := range ctr.NetworkSettings.Networks {
			networks = append(networks, gin.H{
				"name":    netName,
				"ip":      netCfg.IPAddress,
				"gateway": netCfg.Gateway,
				"mac":     netCfg.MacAddress,
			})
		}
	}

	// Ports
	ports := make([]gin.H, 0)
	if ctr.NetworkSettings != nil && ctr.NetworkSettings.Ports != nil {
		for portSpec, bindings := range ctr.NetworkSettings.Ports {
			hostPort := ""
			if len(bindings) > 0 {
				parts := make([]string, 0, len(bindings))
				for _, b := range bindings {
					parts = append(parts, fmt.Sprintf("%s:%s", b.HostIP, b.HostPort))
				}
				hostPort = strings.Join(parts, ", ")
			}
			ports = append(ports, gin.H{
				"container_port": string(portSpec),
				"host_port":      hostPort,
			})
		}
	}

	// Mounts
	mounts := make([]gin.H, 0)
	for _, m := range ctr.Mounts {
		source := m.Source
		if source == "" {
			source = m.Name
		}
		mounts = append(mounts, gin.H{
			"source":      source,
			"destination": m.Destination,
			"type":        string(m.Type),
			"rw":          m.RW,
		})
	}

	// Environment (mask sensitive values)
	sensitiveKeys := []string{"PASSWORD", "SECRET", "TOKEN", "KEY", "CREDENTIAL", "API_KEY"}
	env := make([]string, 0)
	if ctr.Config != nil {
		for i, e := range ctr.Config.Env {
			if i >= 50 {
				break
			}
			varName := strings.ToUpper(strings.SplitN(e, "=", 2)[0])
			isSensitive := false
			for _, s := range sensitiveKeys {
				if strings.Contains(varName, s) {
					isSensitive = true
					break
				}
			}
			if isSensitive {
				env = append(env, strings.SplitN(e, "=", 2)[0]+"=********")
			} else {
				env = append(env, e)
			}
		}
	}

	// Image name
	image := "unknown"
	if ctr.Config != nil && ctr.Config.Image != "" {
		image = ctr.Config.Image
	}

	// Created timestamp
	created := ""
	if len(ctr.Created) >= 19 {
		created = strings.Replace(ctr.Created[:19], "T", " ", 1)
	}

	// Restart policy
	restartPolicy := "none"
	if ctr.HostConfig != nil {
		restartPolicy = string(ctr.HostConfig.RestartPolicy.Name)
	}

	// Short ID
	shortID := ctr.ID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}

	// Cmd
	cmd := []string{}
	if ctr.Config != nil && ctr.Config.Cmd != nil {
		cmd = ctr.Config.Cmd
	}

	// Labels (limit to 20)
	labels := make(map[string]string)
	if ctr.Config != nil && ctr.Config.Labels != nil {
		count := 0
		for k, v := range ctr.Config.Labels {
			if count >= 20 {
				break
			}
			labels[k] = v
			count++
		}
	}

	info := gin.H{
		"name":           strings.TrimPrefix(ctr.Name, "/"),
		"id":             shortID,
		"image":          image,
		"status":         ctr.State.Status,
		"created":        created,
		"restart_policy": restartPolicy,
		"platform":       ctr.Platform,
		"networks":       networks,
		"ports":          ports,
		"mounts":         mounts,
		"env":            env,
		"cmd":            cmd,
		"labels":         labels,
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "info": info})
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

func handleContainerStats(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}

	cid := c.Param("cid")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stats, err := services.CollectContainerStats(ctx, cli, cid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	memPercent := 0.0
	if stats.MemLimMB > 0 {
		memPercent = (stats.MemMB / stats.MemLimMB) * 100.0
	}

	c.JSON(http.StatusOK, gin.H{
		"status":      "ok",
		"cpu_percent": math.Round(stats.CPU*10) / 10,
		"mem_percent": math.Round(memPercent*10) / 10,
		"mem_mb":      math.Round(stats.MemMB*10) / 10,
	})
}

// ---------------------------------------------------------------------------
// Project compose content
// ---------------------------------------------------------------------------

func authorizeProjectDownload(c *gin.Context, name string) bool {
	user, role := middleware.GetSessionUser(c)
	if user == "" || !middleware.IsAdminOrAbove(role) && getUserRoleOnProject(name, user) == "none" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Accès refusé à ce projet"})
		return false
	}
	root := filepath.Join(config.ProjectsDir, name)
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Dossier projet indisponible"})
		return false
	}
	return true
}

func handleProjectCompose(c *gin.Context) {
	name := c.Param("name")
	if !isSafeProjectName(name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nom de projet invalide"})
		return
	}
	if !authorizeProjectDownload(c, name) {
		return
	}

	composePath := filepath.Join(config.ProjectsDir, name, "compose.yaml")
	if info, err := os.Lstat(composePath); err != nil || !info.Mode().IsRegular() {
		c.JSON(http.StatusNotFound, gin.H{"error": "Fichier Compose indisponible"})
		return
	}
	data, err := os.ReadFile(composePath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "compose.yaml introuvable"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "content": string(data)})
}

// ---------------------------------------------------------------------------
// Project export as zip
// ---------------------------------------------------------------------------

func handleProjectExport(c *gin.Context) {
	name := c.Param("name")
	if !isSafeProjectName(name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nom de projet invalide"})
		return
	}
	if !authorizeProjectDownload(c, name) {
		return
	}

	projDir := filepath.Join(config.ProjectsDir, name)
	if _, err := os.Stat(projDir); os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Projet introuvable"})
		return
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var total int64

	err := filepath.Walk(projDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("les liens et fichiers spéciaux ne sont pas exportés")
		}
		total += info.Size()
		if total > 128<<20 {
			return fmt.Errorf("l’archive dépasse 128 Mio ; retirez les fichiers de données du dossier de génération")
		}
		relPath, _ := filepath.Rel(projDir, path)
		// Normalize to forward slashes for zip
		relPath = strings.ReplaceAll(relPath, "\\", "/")
		w, err := zw.Create(relPath)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})

	zw.Close()

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur d'export"})
		return
	}

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "export", name, "")

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s.zip", name))
	c.Data(http.StatusOK, "application/zip", buf.Bytes())
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// isSafeLocalPath checks that the path is within the projects directory.
func isSafeLocalPath(path string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absProjects, err := filepath.Abs(config.ProjectsDir)
	if err != nil {
		return false
	}
	return strings.HasPrefix(absPath, absProjects)
}

// tailN returns the last n lines of a string.
func tailN(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// parseInt safely parses a string to int with a default.
func parseInt(s string, def int) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

// ---------------------------------------------------------------------------
// Browse — list files in a container directory
// ---------------------------------------------------------------------------

func handleContainerBrowse(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}
	cid := c.Param("cid")
	dir := c.DefaultQuery("path", "/usr/share/nginx/html")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use exec to list files
	exec, err := cli.ContainerExecCreate(ctx, cid, container.ExecOptions{
		Cmd:          []string{"find", dir, "-maxdepth", "2", "-type", "f"},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	resp, err := cli.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer resp.Close()

	output, _ := io.ReadAll(resp.Reader)
	// Clean docker multiplexed stream
	cleaned := cleanDockerOutput(output)
	lines := strings.Split(strings.TrimSpace(cleaned), "\n")

	var files []gin.H
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ext := strings.ToLower(filepath.Ext(line))
		isEditable := ext == ".html" || ext == ".css" || ext == ".js" || ext == ".json" ||
			ext == ".php" || ext == ".conf" || ext == ".yml" || ext == ".yaml" ||
			ext == ".txt" || ext == ".md" || ext == ".xml" || ext == ".sh" || ext == ".sql" || ext == ""
		isImage := ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif" || ext == ".svg" || ext == ".ico" || ext == ".webp"
		files = append(files, gin.H{
			"path":     line,
			"name":     filepath.Base(line),
			"ext":      ext,
			"editable": isEditable,
			"image":    isImage,
		})
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "path": dir, "files": files})
}

func cleanDockerOutput(data []byte) string {
	var result strings.Builder
	for len(data) >= 8 {
		size := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
		data = data[8:]
		if size > len(data) {
			size = len(data)
		}
		result.Write(data[:size])
		data = data[size:]
	}
	if result.Len() == 0 {
		return string(data)
	}
	return result.String()
}

// ---------------------------------------------------------------------------
// Upload — upload a file into a container
// ---------------------------------------------------------------------------

func handleContainerUpload(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}
	cid := c.Param("cid")
	destPath := c.PostForm("path")
	if destPath == "" {
		destPath = "/usr/share/nginx/html"
	}

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Fichier manquant"})
		return
	}
	defer file.Close()

	// Create a tar archive with the file
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	fileData, _ := io.ReadAll(file)

	tw.WriteHeader(&tar.Header{
		Name: header.Filename,
		Mode: 0644,
		Size: int64(len(fileData)),
	})
	tw.Write(fileData)
	tw.Close()

	// Copy to container
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = cli.CopyToContainer(ctx, cid, destPath, &buf, container.CopyToContainerOptions{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"message": fmt.Sprintf("Fichier %s uploade dans %s/%s", header.Filename, destPath, header.Filename),
	})
}
