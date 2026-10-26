package handlers

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
)

// rollbackInfo stores the previous image info for a container after an update.
type rollbackInfo struct {
	OldImage   string
	OldImageID string
	Project    string
	Service    string
	ProjectDir string
	Timestamp  string
}

var (
	rollbackStore   = make(map[string]*rollbackInfo)
	rollbackStoreMu sync.RWMutex
)

// RegisterUpdateRoutes registers container update and rollback routes.
func RegisterUpdateRoutes(api *gin.RouterGroup) {
	api.POST("/container/:cid/update", middleware.AdminRequired(), handleUpdateContainer)
	api.POST("/container/:cid/rollback", middleware.AdminRequired(), handleRollbackContainer)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func findProjectAndService(labels map[string]string) (string, string) {
	return labels["com.docker.compose.project"], labels["com.docker.compose.service"]
}

func getProjectDir(projectName string) string {
	dir := filepath.Join(config.ProjectsDir, projectName)
	if _, err := os.Stat(dir); err == nil {
		return dir
	}
	// Case-insensitive search
	entries, err := os.ReadDir(config.ProjectsDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() && strings.EqualFold(e.Name(), projectName) {
			return filepath.Join(config.ProjectsDir, e.Name())
		}
	}
	return ""
}

func runComposeCmd(projectDir string, args []string) (int, string, string) {
	for _, cmdBase := range [][]string{{"docker", "compose"}, {"docker-compose"}} {
		fullArgs := append(cmdBase, args...)
		cmd := exec.Command(fullArgs[0], fullArgs[1:]...)
		cmd.Dir = projectDir

		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				// Command not found — try fallback
				continue
			}
		}
		return exitCode, stdout.String(), stderr.String()
	}
	return -1, "", "Ni 'docker compose' ni 'docker-compose' n'est disponible"
}

func waitForContainer(containerName string, maxAttempts int, interval time.Duration) bool {
	cli := services.EnsureDocker()
	if cli == nil {
		return false
	}

	for i := 0; i < maxAttempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
		cancel()
		if err == nil {
			for _, ctr := range containers {
				name := ""
				if len(ctr.Names) > 0 {
					name = strings.TrimPrefix(ctr.Names[0], "/")
				}
				if name == containerName && ctr.State == "running" {
					return true
				}
			}
		}
		time.Sleep(interval)
	}
	return false
}

func performRollback(cid string) bool {
	rollbackStoreMu.RLock()
	info := rollbackStore[cid]
	rollbackStoreMu.RUnlock()

	if info == nil || info.OldImage == "" {
		return false
	}

	projectDir := info.ProjectDir
	service := info.Service
	oldImage := info.OldImage

	// Find compose file
	composePath := ""
	for _, name := range []string{"compose.yaml", "docker-compose.yaml", "docker-compose.yml"} {
		p := filepath.Join(projectDir, name)
		if _, err := os.Stat(p); err == nil {
			composePath = p
			break
		}
	}
	if composePath == "" {
		return false
	}

	// Modify compose file to restore old image
	data, err := os.ReadFile(composePath)
	if err != nil {
		return false
	}

	var composeContent map[string]interface{}
	if err := yaml.Unmarshal(data, &composeContent); err != nil {
		return false
	}

	svcs, ok := composeContent["services"].(map[string]interface{})
	if !ok {
		return false
	}
	svcConf, ok := svcs[service].(map[string]interface{})
	if !ok {
		return false
	}
	svcConf["image"] = oldImage

	newData, err := yaml.Marshal(composeContent)
	if err != nil {
		return false
	}
	if err := os.WriteFile(composePath, newData, 0644); err != nil {
		return false
	}

	exitCode, _, _ := runComposeCmd(projectDir, []string{"up", "-d", "--no-deps", service})
	return exitCode == 0
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

func handleUpdateContainer(c *gin.Context) {
	cid := c.Param("cid")
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Client Docker non disponible"})
		return
	}

	// Get container
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ctrJSON, err := cli.ContainerInspect(ctx, cid)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "Conteneur introuvable"})
		return
	}

	project, service := findProjectAndService(ctrJSON.Config.Labels)
	if project == "" || service == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status":  "error",
			"message": "Impossible d'identifier le projet ou le service (labels Docker Compose manquants)",
		})
		return
	}

	projectDir := getProjectDir(project)
	if projectDir == "" {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": fmt.Sprintf("Repertoire du projet '%s' introuvable", project)})
		return
	}

	// Save old image for rollback
	oldImageTag := ""
	oldImageID := ""
	if ctrJSON.Image != "" {
		oldImageID = ctrJSON.Image
	}
	if ctrJSON.Config != nil && ctrJSON.Config.Image != "" {
		oldImageTag = ctrJSON.Config.Image
	}
	containerName := strings.TrimPrefix(ctrJSON.Name, "/")

	// Perform update
	exitCode, stdout, stderr := runComposeCmd(projectDir, []string{"up", "-d", "--no-deps", "--pull", "always", service})
	if exitCode != 0 {
		errMsg := strings.TrimSpace(stderr)
		if errMsg == "" {
			errMsg = strings.TrimSpace(stdout)
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":             "error",
			"message":            fmt.Sprintf("Echec de la mise a jour : %s", errMsg),
			"old_image":          oldImageTag,
			"rollback_available": false,
		})
		return
	}

	// Wait for container to be running
	healthy := waitForContainer(containerName, 10, 3*time.Second)

	// Get new image info
	newImageTag := ""
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	containers, err := cli.ContainerList(ctx2, container.ListOptions{All: true})
	cancel2()
	if err == nil {
		for _, ctr := range containers {
			name := ""
			if len(ctr.Names) > 0 {
				name = strings.TrimPrefix(ctr.Names[0], "/")
			}
			if name == containerName {
				newImageTag = ctr.Image
				break
			}
		}
	}

	// Store rollback info
	rollbackStoreMu.Lock()
	rollbackStore[cid] = &rollbackInfo{
		OldImage:   oldImageTag,
		OldImageID: oldImageID,
		Project:    project,
		Service:    service,
		ProjectDir: projectDir,
		Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
	}
	rollbackStoreMu.Unlock()

	s := sessions.Default(c)
	username, _ := s.Get("user").(string)

	if !healthy {
		// Try auto-rollback
		rollbackSuccess := performRollback(cid)
		db.LogActivity(username, "container_update_failed", containerName,
			fmt.Sprintf("Mise a jour echouee, rollback %s", map[bool]string{true: "reussi", false: "echoue"}[rollbackSuccess]))
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":             "error",
			"message":            "Le conteneur n'est pas devenu operationnel apres la mise a jour. Rollback automatique tente.",
			"old_image":          oldImageTag,
			"new_image":          newImageTag,
			"rollback_available": true,
			"rollback_attempted": true,
			"rollback_success":   rollbackSuccess,
		})
		return
	}

	db.LogActivity(username, "container_update", containerName,
		fmt.Sprintf("Image mise a jour : %s -> %s", oldImageTag, newImageTag))

	c.JSON(http.StatusOK, gin.H{
		"status":             "ok",
		"message":            fmt.Sprintf("Conteneur '%s' mis a jour avec succes", containerName),
		"old_image":          oldImageTag,
		"new_image":          newImageTag,
		"rollback_available": true,
	})
}

func handleRollbackContainer(c *gin.Context) {
	cid := c.Param("cid")
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Client Docker non disponible"})
		return
	}

	rollbackStoreMu.RLock()
	info := rollbackStore[cid]
	rollbackStoreMu.RUnlock()

	if info == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"status":  "error",
			"message": "Aucune information de rollback disponible pour ce conteneur",
		})
		return
	}

	if info.OldImage == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Image precedente inconnue"})
		return
	}

	s := sessions.Default(c)
	username, _ := s.Get("user").(string)

	success := performRollback(cid)
	if !success {
		db.LogActivity(username, "container_rollback_failed",
			fmt.Sprintf("%s/%s", info.Project, info.Service),
			fmt.Sprintf("Echec du rollback vers %s", info.OldImage))
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Echec du rollback"})
		return
	}

	// Clean up rollback info
	rollbackStoreMu.Lock()
	delete(rollbackStore, cid)
	rollbackStoreMu.Unlock()

	db.LogActivity(username, "container_rollback",
		fmt.Sprintf("%s/%s", info.Project, info.Service),
		fmt.Sprintf("Rollback vers %s", info.OldImage))

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"message": fmt.Sprintf("Rollback du service '%s' vers l'image '%s' effectue", info.Service, info.OldImage),
	})
}
