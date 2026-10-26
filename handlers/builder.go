package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
)

var safeProjectRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

func isSafeProjectName(name string) bool {
	return name != "" && !strings.Contains(name, "..") && safeProjectRe.MatchString(name)
}

// RegisterBuilderRoutes registers all builder/deploy/catalogue API routes.
func RegisterBuilderRoutes(api *gin.RouterGroup) {
	api.POST("/generate", handleGenerationExport)
	api.POST("/generate/preview", handleGenerationPreview)
	api.POST("/deploy", handleDeploy)
	api.POST("/deploy/preview", handleDeployPreview)
	api.POST("/ai-generate", handleAIGenerate)
	registerInfrastructureJobRoutes(api)
	registerCloudToolRoutes(api)
	api.GET("/used-cidrs", handleUsedCIDRs)
	api.GET("/catalogue", handleCatalogueList)
	api.POST("/catalogue/add", middleware.AdminRequired(), handleCatalogueAdd)
	api.POST("/catalogue/remove", middleware.AdminRequired(), handleCatalogueRemove)
	api.POST("/canvas/import", handleCanvasImport)
	api.POST("/ai-provision", handleAIProvision)
	api.POST("/magic/upload-image", handleMagicUploadImage)
	api.GET("/magic/qrcode", handleMagicQRCode)
	api.GET("/project/:name/versions", handleProjectVersions)
	api.POST("/project/:name/rollback", middleware.AdminRequired(), handleProjectRollback)
	api.POST("/project/:name/compose/save", handleComposeSave)
	api.POST("/project/:name/compose/ai-edit", handleComposeAIEdit)
}

// ---------------------------------------------------------------------------
// Deploy
// ---------------------------------------------------------------------------

func handleDeploy(c *gin.Context) {
	handleDeployUsing(c, runComposeCommand, services.GetUsedSubnets)
}

type composeCommandRunner func(context.Context, string, ...string) (string, error)
type builderSubnetResolver func(string, string) map[string]bool

var builderProjectLocks sync.Map

func runComposeCommand(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(out), ctx.Err()
	}
	return string(out), err
}

func normalizeBuilderProjectName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !isSafeProjectName(name) {
		return "", fmt.Errorf("Nom de projet invalide : utilisez des lettres, chiffres, tirets ou underscores")
	}
	return name, nil
}

// A database failure must never be mistaken for permission to create a project.
func builderProjectAccess(project, user string) (bool, error) {
	var owner sql.NullString
	err := db.DB.QueryRow("SELECT owner FROM labs WHERE project = ?", project).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return getUserRoleOnProject(project, user) == "write", nil
}

func handleDeployUsing(c *gin.Context, run composeCommandRunner, resolveSubnets builderSubnetResolver) {
	var req struct {
		Project string                       `json:"project"`
		Payload services.BuildComposePayload `json:"payload"`
		Deploy  bool                         `json:"deploy"`
	}
	steps := make([]gin.H, 0, 6)
	addStep := func(status, title, text string) {
		steps = append(steps, gin.H{"status": status, "title": title, "text": text})
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "JSON invalide"})
		return
	}

	user, _ := middleware.GetSessionUser(c)

	project, err := normalizeBuilderProjectName(req.Project)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": err.Error()})
		return
	}
	lock, _ := builderProjectLocks.LoadOrStore(project, &sync.Mutex{})
	projectLock := lock.(*sync.Mutex)
	projectLock.Lock()
	defer projectLock.Unlock()
	addStep("done", "Preparation", fmt.Sprintf("Projet %s prepare.", project))

	allowed, err := builderProjectAccess(project, user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "Impossible de verifier les droits du projet"})
		return
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"status": "error", "error": "Acces refuse sur ce projet"})
		return
	}

	// Build compose
	if err := services.ValidateInfrastructureOptions(req.Payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": err.Error()})
		return
	}
	payload := prepareBuilderPayload(req.Payload, resolveSubnets(config.ProjectsDir, project))
	compose := services.BuildComposeForLocalDeployment(payload, project)
	projDir := filepath.Join(config.ProjectsDir, project)

	// Build and validate a separate candidate before replacing the saved compose.
	// Preparing a project never stops its containers or erases user files.
	yamlBytes, err := yaml.Marshal(compose)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "Erreur generation YAML"})
		return
	}
	if svcMap, _ := compose["services"].(services.M); len(svcMap) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Choisissez au moins un service valide"})
		return
	}
	validationCtx, cancelValidation := context.WithTimeout(c.Request.Context(), 30*time.Second)
	err = saveBuilderProject(validationCtx, projDir, project, user, compose, yamlBytes, req.Deploy, run)
	cancelValidation()
	if err != nil {
		status := http.StatusInternalServerError
		var validationErr *builderValidationError
		if errors.As(err, &validationErr) {
			status = http.StatusUnprocessableEntity
		}
		addStep("error", "Verification", "Le projet n'a pas ete remplace.")
		c.JSON(status, gin.H{"status": "error", "error": err.Error(), "steps": steps})
		return
	}
	addStep("done", "Verification", "La configuration a ete verifiee avant sauvegarde.")
	addStep("done", "Configuration", "compose.yaml a ete sauvegarde. Les fichiers existants sont conserves.")
	addStep("done", "Fichiers projet", "Les fichiers utiles manquants ont ete crees.")

	// Deploy if requested
	logs := ""
	if req.Deploy {
		deployCtx, cancelDeploy := context.WithTimeout(c.Request.Context(), 5*time.Minute)
		logs, err = run(deployCtx, projDir, "up", "-d", "--remove-orphans")
		cancelDeploy()
		if err != nil {
			addStep("error", "Demarrage", "Docker Compose n'a pas pu demarrer tous les services.")
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "Docker Compose a echoue", "logs": logs, "steps": steps})
			return
		}
		addStep("done", "Demarrage", "Les services ont ete lances par Docker Compose.")
		addStep("done", "Projet pret", "Le projet est pret a etre ouvert depuis Mes projets.")
	} else {
		addStep("done", "Projet prepare", "Le compose est pret. Vous pourrez demarrer le projet plus tard.")
	}

	action := "generate"
	if req.Deploy {
		action = "deploy"
	}
	db.LogActivity(user, action, project, "")

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"project": project,
		"path":    "projects/" + project,
		"deploy":  req.Deploy,
		"logs":    logs,
		"steps":   steps,
	})
}

type builderValidationError struct{ message string }

func (e *builderValidationError) Error() string { return e.message }

// saveBuilderProject keeps all user files intact and installs only a validated
// compose candidate. Failed validation leaves the previous project untouched.
func saveBuilderProject(ctx context.Context, projDir, project, user string, compose services.M, data []byte, mustValidate bool, run composeCommandRunner) (resultErr error) {
	info, statErr := os.Lstat(projDir)
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("Lecture du dossier projet impossible: %w", statErr)
	}
	if statErr == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("Le chemin du projet doit etre un dossier local")
	}
	newDirectory := os.IsNotExist(statErr)
	if err := os.MkdirAll(projDir, 0755); err != nil {
		return fmt.Errorf("Creation du dossier projet impossible: %w", err)
	}
	if newDirectory {
		defer func() {
			if resultErr != nil {
				_ = os.Remove(projDir)
			}
		}()
	}
	composePath := filepath.Join(projDir, "compose.yaml")
	if info, err := os.Lstat(composePath); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("compose.yaml doit etre un fichier ordinaire")
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Lecture compose.yaml impossible: %w", err)
	}
	previous, err := os.ReadFile(composePath)
	hadPrevious := err == nil
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Lecture du compose existant impossible: %w", err)
	}
	candidate, err := os.CreateTemp(projDir, ".autodocker-compose-*.yaml")
	if err != nil {
		return fmt.Errorf("Preparation du compose impossible: %w", err)
	}
	candidatePath := candidate.Name()
	defer os.Remove(candidatePath)
	if err := writeAndCloseBuilderFile(candidate, data); err != nil {
		return fmt.Errorf("Ecriture du compose temporaire impossible: %w", err)
	}
	output, validationErr := run(ctx, projDir, "-p", project, "-f", candidatePath, "config", "--quiet")
	// Preparing a file remains possible without the Docker CLI installed. The
	// generator has already validated the service definitions and emitted YAML.
	if validationErr != nil && (mustValidate || !errors.Is(validationErr, exec.ErrNotFound)) {
		if strings.TrimSpace(output) == "" {
			output = validationErr.Error()
		}
		return &builderValidationError{message: "Configuration Docker Compose invalide: " + truncate(strings.TrimSpace(output), 800)}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// The transaction is opened only after the external validation is complete.
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("Enregistrement du projet impossible: %w", err)
	}
	defer tx.Rollback()
	inserted, err := tx.Exec("INSERT OR IGNORE INTO labs (project, owner) VALUES (?, ?)", project, user)
	if err != nil {
		return fmt.Errorf("Enregistrement du proprietaire impossible: %w", err)
	}
	count, err := inserted.RowsAffected()
	if err != nil {
		return err
	}
	if count > 0 {
		if _, err := tx.Exec("INSERT INTO labs_acl (project, username, role) VALUES (?, ?, 'write')", project, user); err != nil {
			return fmt.Errorf("Enregistrement des droits impossible: %w", err)
		}
	}
	if hadPrevious {
		if err := snapshotBuilderCompose(projDir, previous); err != nil {
			return err
		}
	}
	created := []string{}
	createdDirs := []string{}
	defer func() {
		if resultErr != nil {
			for _, name := range created {
				_ = os.Remove(name)
			}
			for _, name := range createdDirs {
				_ = os.Remove(name) // Only our own newly-created, empty directories.
			}
		}
	}()
	files := generatedProjectFiles(compose)
	for name, content := range files {
		filePath := filepath.Join(projDir, filepath.FromSlash(name))
		if _, err := os.Lstat(filePath); err == nil {
			continue // Existing scripts, configuration and logs belong to the user.
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("Lecture %s impossible: %w", name, err)
		}
		parentInfo, parentErr := os.Lstat(filepath.Dir(filePath))
		if parentErr == nil && parentInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Le dossier de %s ne doit pas etre un lien symbolique", name)
		} else if parentErr != nil && !os.IsNotExist(parentErr) {
			return parentErr
		}
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			return err
		}
		if os.IsNotExist(parentErr) {
			createdDirs = append(createdDirs, filepath.Dir(filePath))
		}
		mode := os.FileMode(0644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0755
		}
		file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return fmt.Errorf("Creation %s impossible: %w", name, err)
		}
		created = append(created, filePath)
		if err := writeAndCloseBuilderFile(file, []byte(content)); err != nil {
			return fmt.Errorf("Ecriture %s impossible: %w", name, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(candidatePath, composePath); err != nil {
		return fmt.Errorf("Remplacement compose.yaml impossible: %w", err)
	}
	if err := tx.Commit(); err != nil {
		// Restore the previous configuration if its ownership could not be saved.
		var restoreErr error
		if hadPrevious {
			restoreErr = os.WriteFile(composePath, previous, 0600)
		} else {
			restoreErr = os.Remove(composePath)
		}
		if restoreErr != nil {
			return fmt.Errorf("Enregistrement echoue (%v); restauration impossible (%v), utilisez la version sauvegardee", err, restoreErr)
		}
		return fmt.Errorf("Enregistrement du projet impossible, configuration precedente restauree: %w", err)
	}
	return nil
}

func writeAndCloseBuilderFile(file *os.File, data []byte) error {
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func snapshotBuilderCompose(projDir string, data []byte) error {
	dir := filepath.Join(projDir, "versions")
	if info, err := os.Lstat(dir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("Le dossier versions est invalide")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("Creation de la sauvegarde impossible: %w", err)
	}
	name := fmt.Sprintf("compose_v%d_%s_*.yaml", countVersions(dir)+1, time.Now().Format("20060102_150405"))
	file, err := os.CreateTemp(dir, name)
	if err != nil {
		return fmt.Errorf("Creation de la sauvegarde impossible: %w", err)
	}
	if err := writeAndCloseBuilderFile(file, data); err != nil {
		_ = os.Remove(file.Name())
		return fmt.Errorf("Ecriture de la sauvegarde impossible: %w", err)
	}
	check, err := os.ReadFile(file.Name())
	if err != nil || !bytes.Equal(check, data) {
		return fmt.Errorf("Verification de la sauvegarde compose impossible")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Deploy Preview
// ---------------------------------------------------------------------------

func handleDeployPreview(c *gin.Context) {
	handleDeployPreviewUsing(c, services.GetUsedSubnets)
}

func handleDeployPreviewUsing(c *gin.Context, resolveSubnets builderSubnetResolver) {
	var req struct {
		Project string                       `json:"project"`
		Payload services.BuildComposePayload `json:"payload"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "JSON invalide"})
		return
	}

	project, err := normalizeBuilderProjectName(req.Project)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": err.Error()})
		return
	}
	user, _ := middleware.GetSessionUser(c)
	allowed, err := builderProjectAccess(project, user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "Impossible de verifier les droits du projet"})
		return
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"status": "error", "error": "Acces refuse sur ce projet"})
		return
	}

	if err := services.ValidateInfrastructureOptions(req.Payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": err.Error()})
		return
	}
	payload := prepareBuilderPayload(req.Payload, resolveSubnets(config.ProjectsDir, project))
	newCompose := services.BuildComposeForLocalDeployment(payload, project)
	if svcMap, _ := newCompose["services"].(services.M); len(svcMap) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Choisissez au moins un service valide"})
		return
	}
	newYAML, err := yaml.Marshal(newCompose)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	composePath := filepath.Join(config.ProjectsDir, project, "compose.yaml")
	if _, err := os.Stat(composePath); os.IsNotExist(err) {
		// New project
		newSvcs := extractKeys(newCompose, "services")
		c.JSON(http.StatusOK, gin.H{
			"status":   "ok",
			"is_new":   true,
			"new_yaml": string(newYAML),
			"ingress":  newCompose["x-autodocker"],
			"diagram":  services.ArchitecturePreview(newCompose),
			"summary": gin.H{
				"new_services":     newSvcs,
				"removed_services": []string{},
				"new_volumes":      extractKeys(newCompose, "volumes"),
				"new_networks":     extractKeys(newCompose, "networks"),
			},
		})
		return
	}

	// Existing project — compute diff
	oldYAMLBytes, err := os.ReadFile(composePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "Erreur lecture compose existant"})
		return
	}

	oldCompose := services.M{}
	_ = yaml.Unmarshal(oldYAMLBytes, &oldCompose)

	oldSvcs := extractKeysFromRaw(oldCompose, "services")
	newSvcs := extractKeys(newCompose, "services")

	diff := simpleDiff(string(oldYAMLBytes), string(newYAML))

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"is_new": false,
		"diff":   diff,
		"summary": gin.H{
			"new_services":     setDifference(newSvcs, oldSvcs),
			"removed_services": setDifference(oldSvcs, newSvcs),
			"kept_services":    setIntersection(oldSvcs, newSvcs),
			"new_volumes":      setDifference(extractKeys(newCompose, "volumes"), extractKeysFromRaw(oldCompose, "volumes")),
			"new_networks":     setDifference(extractKeys(newCompose, "networks"), extractKeysFromRaw(oldCompose, "networks")),
		},
	})
}

// Preview and deployment share subnet allocation, including gateways and any
// explicitly assigned service/router addresses. Never mutate the user's input.
func prepareBuilderPayload(payload services.BuildComposePayload, occupied map[string]bool) services.BuildComposePayload {
	payload.Addressing.Networks = append([]services.NetworkDef(nil), payload.Addressing.Networks...)
	payload.Services = append([]services.ServiceEntry(nil), payload.Services...)
	used := make(map[string]bool, len(occupied))
	for cidr, busy := range occupied {
		used[cidr] = busy
	}
	type subnetChange struct{ before, after string }
	changes := map[string]subnetChange{}
	for i := range payload.Addressing.Networks {
		network := &payload.Addressing.Networks[i]
		previous := network.CIDR
		network.CIDR = services.FindFreeSubnet(network.CIDR, used)
		used[network.CIDR] = true
		network.Gateway = services.GatewayFromCIDR(network.CIDR)
		if previous != network.CIDR {
			changes[network.Name] = subnetChange{previous, network.CIDR}
		}
	}
	for i := range payload.Services {
		service := &payload.Services[i]
		if change, ok := changes[service.Network]; ok {
			service.IP = remapBuilderIP(service.IP, change.before, change.after)
		}
		if service.LAN != nil {
			lan := *service.LAN
			if change, ok := changes[lan.Network]; ok {
				lan.IP = remapBuilderIP(lan.IP, change.before, change.after)
			}
			service.LAN = &lan
		}
		if service.WAN != nil {
			wan := *service.WAN
			if change, ok := changes[wan.Network]; ok {
				wan.IP = remapBuilderIP(wan.IP, change.before, change.after)
			}
			service.WAN = &wan
		}
	}
	return payload
}

func remapBuilderIP(address, previousCIDR, nextCIDR string) string {
	previousIP, previousNetwork, previousErr := net.ParseCIDR(previousCIDR)
	nextIP, nextNetwork, nextErr := net.ParseCIDR(nextCIDR)
	ip := net.ParseIP(address).To4()
	if previousErr != nil || nextErr != nil || ip == nil || previousIP.To4() == nil || nextIP.To4() == nil || !previousNetwork.Contains(ip) {
		return address
	}
	offset := binary.BigEndian.Uint32(ip) - binary.BigEndian.Uint32(previousNetwork.IP.To4())
	remapped := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(remapped, binary.BigEndian.Uint32(nextNetwork.IP.To4())+offset)
	if !nextNetwork.Contains(remapped) {
		return address
	}
	return remapped.String()
}

// ---------------------------------------------------------------------------
// AI Generate: shared provider-independent design and validation.
// ---------------------------------------------------------------------------

func handleAIGenerate(c *gin.Context) {
	owner, job, ok := submitInfrastructureJob(c)
	if !ok {
		return
	}
	view, err := waitInfrastructureJob(c.Request.Context(), owner, job.ID)
	if err != nil {
		c.JSON(http.StatusRequestTimeout, gin.H{"status": "error", "error": "Demande interrompue ou expirée"})
		return
	}
	if view.Result == nil {
		message := view.Error
		if message == "" {
			message = "Demande annulée ou indisponible"
		}
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": message})
		return
	}
	c.JSON(http.StatusOK, infrastructureResultResponse(view.Result))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Used CIDRs
// ---------------------------------------------------------------------------

func handleUsedCIDRs(c *gin.Context) {
	used := services.GetUsedSubnets(config.ProjectsDir, "")
	cidrs := make([]string, 0, len(used))
	for k := range used {
		cidrs = append(cidrs, k)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "used": cidrs})
}

// ---------------------------------------------------------------------------
// Catalogue
// ---------------------------------------------------------------------------

func handleCatalogueList(c *gin.Context) {
	// Load custom catalogue from DB
	loadCustomCatalogueFromDB()

	base := make([]gin.H, 0)
	for key, img := range services.ImagesBase {
		cat := services.CategoryMap[key]
		if cat == "" {
			cat = "custom"
		}
		base = append(base, gin.H{
			"key":         key,
			"image":       img,
			"category":    cat,
			"description": "",
			"builtin":     true,
		})
	}

	customs := loadCustomEntries()
	c.JSON(http.StatusOK, gin.H{"status": "ok", "base": base, "custom": customs})
}

func handleCatalogueAdd(c *gin.Context) {
	var req struct {
		Key         string `json:"key"`
		Image       string `json:"image"`
		Category    string `json:"category"`
		Description string `json:"description"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "JSON invalide"})
		return
	}

	key := regexp.MustCompile(`[^a-z0-9_-]`).ReplaceAllString(strings.TrimSpace(strings.ToLower(req.Key)), "")
	image := strings.TrimSpace(req.Image)
	category := strings.TrimSpace(strings.ToLower(req.Category))
	if category == "" {
		category = "custom"
	}

	if key == "" || image == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cle et image requises"})
		return
	}
	if len(key) > 50 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cle trop longue (max 50 chars)"})
		return
	}
	if _, isBase := services.ImagesBase[key]; isBase {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("La cle '%s' est reservee (service de base)", key)})
		return
	}
	if !services.ValidCategories[category] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Categorie invalide"})
		return
	}

	// Try pulling the image
	if services.EnsureDocker() != nil {
		cmd := exec.Command("docker", "pull", image)
		if out, err := cmd.CombinedOutput(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Impossible de pull '%s': %s", image, string(out))})
			return
		}
	}

	user, _ := middleware.GetSessionUser(c)
	now := time.Now().Format("2006-01-02 15:04:05")

	_, err := db.DB.Exec(`INSERT INTO catalogue_custom (key, image, category, description, added_by, added_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET image=excluded.image, category=excluded.category, description=excluded.description`,
		key, image, category, strings.TrimSpace(req.Description), user, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	loadCustomCatalogueFromDB()
	db.LogActivity(user, "catalogue-add", key, fmt.Sprintf("image=%s cat=%s", image, category))

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": fmt.Sprintf("Service '%s' ajoute au catalogue", key)})
}

func handleCatalogueRemove(c *gin.Context) {
	var req struct {
		Key string `json:"key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "JSON invalide"})
		return
	}

	key := strings.TrimSpace(req.Key)
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cle requise"})
		return
	}
	if _, isBase := services.ImagesBase[key]; isBase {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Impossible de supprimer un service de base"})
		return
	}

	_, _ = db.DB.Exec("DELETE FROM catalogue_custom WHERE key = ?", key)
	loadCustomCatalogueFromDB()

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "catalogue-remove", key, "")

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": fmt.Sprintf("Service '%s' retire du catalogue", key)})
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func writeScript(path, content string) {
	dir := filepath.Dir(path)
	os.MkdirAll(dir, 0755)
	os.WriteFile(path, []byte(content), 0755)
}

func getUserRoleOnProject(project, username string) string {
	// Check direct ACL
	var role string
	err := db.DB.QueryRow("SELECT role FROM labs_acl WHERE project = ? AND username = ?", project, username).Scan(&role)
	if err == nil && role == "write" {
		return "write"
	}

	// Check ownership
	var owner string
	err = db.DB.QueryRow("SELECT owner FROM labs WHERE project = ?", project).Scan(&owner)
	if err == nil && owner == username {
		return "write"
	}

	// Check group ACL
	err = db.DB.QueryRow(`
		SELECT ga.role FROM groups_acl ga
		JOIN user_groups ug ON ga.group_name = ug.group_name
		WHERE ug.username = ? AND ga.project = ?
		ORDER BY CASE ga.role WHEN 'write' THEN 0 ELSE 1 END
		LIMIT 1`, username, project).Scan(&role)
	if err == nil {
		return role
	}

	return "none"
}

func loadCustomCatalogueFromDB() {
	customs := make(map[string]string)
	rows, err := db.DB.Query("SELECT key, image FROM catalogue_custom")
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k, img string
		if rows.Scan(&k, &img) == nil {
			customs[k] = img
		}
	}
	services.MergeCustomCatalogue(customs)
}

func loadCustomEntries() []gin.H {
	entries := make([]gin.H, 0)
	rows, err := db.DB.Query("SELECT key, image, category, description, added_by, added_at FROM catalogue_custom")
	if err != nil {
		return entries
	}
	defer rows.Close()
	for rows.Next() {
		var key, image, category, desc, addedBy, addedAt string
		if rows.Scan(&key, &image, &category, &desc, &addedBy, &addedAt) == nil {
			entries = append(entries, gin.H{
				"key":         key,
				"image":       image,
				"category":    category,
				"description": desc,
				"added_by":    addedBy,
				"added_at":    addedAt,
				"builtin":     false,
			})
		}
	}
	return entries
}

func extractKeys(m services.M, section string) []string {
	sub, ok := m[section].(services.M)
	if !ok {
		return []string{}
	}
	keys := make([]string, 0, len(sub))
	for k := range sub {
		keys = append(keys, k)
	}
	return keys
}

func extractKeysFromRaw(m services.M, section string) []string {
	sub, ok := m[section]
	if !ok {
		return []string{}
	}
	// services.M is an alias for map[string]interface{}
	switch v := sub.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		return keys
	default:
		// Try JSON round-trip
		b, err := json.Marshal(sub)
		if err != nil {
			return []string{}
		}
		var raw map[string]interface{}
		if json.Unmarshal(b, &raw) == nil {
			keys := make([]string, 0, len(raw))
			for k := range raw {
				keys = append(keys, k)
			}
			return keys
		}
		return []string{}
	}
}

func setDifference(a, b []string) []string {
	bSet := make(map[string]bool, len(b))
	for _, v := range b {
		bSet[v] = true
	}
	result := make([]string, 0)
	for _, v := range a {
		if !bSet[v] {
			result = append(result, v)
		}
	}
	return result
}

func setIntersection(a, b []string) []string {
	bSet := make(map[string]bool, len(b))
	for _, v := range b {
		bSet[v] = true
	}
	result := make([]string, 0)
	for _, v := range a {
		if bSet[v] {
			result = append(result, v)
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// AI Provision — generate and inject configuration files into containers
// ---------------------------------------------------------------------------

func handleAIProvision(c *gin.Context) {
	var req struct {
		Prompt  string `json:"prompt"`
		Project string `json:"project"`
		Fast    bool   `json:"fast"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Project == "" || req.Prompt == "" {
		c.JSON(400, gin.H{"error": "prompt et project requis"})
		return
	}

	user, _ := middleware.GetSessionUser(c)
	fmt.Printf("[AI-PROVISION] %s: %s (projet: %s)\n", user, req.Prompt[:min(len(req.Prompt), 60)], req.Project)

	// List running containers for this project
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(503, gin.H{"error": "Docker non connecte"})
		return
	}

	ctx := c.Request.Context()
	allContainers, _ := cli.ContainerList(ctx, containerListOpts())
	prefix := req.Project + "-"
	var containerNames []string
	for _, ctr := range allContainers {
		name := ""
		if len(ctr.Names) > 0 {
			name = strings.TrimPrefix(ctr.Names[0], "/")
		}
		if strings.HasPrefix(name, prefix) {
			containerNames = append(containerNames, name)
		}
	}

	if len(containerNames) == 0 {
		c.JSON(404, gin.H{"error": "Aucun conteneur trouve pour le projet " + req.Project})
		return
	}

	var payload map[string]interface{}
	var aiErr error
	if req.Fast {
		if preset, ok := services.GeneratePresetProvision(req.Prompt, req.Project, containerNames); ok {
			payload = preset
		}
	}
	// AI first when available: it tailors real content to the request
	// ("cours de math" -> vraies lecons de maths). Presets are the fallback.
	if payload == nil && services.AIAvailable() {
		payload, aiErr = services.GenerateAIProvision(req.Prompt, req.Project, containerNames)
	}
	if payload == nil {
		if preset, ok := services.GeneratePresetProvision(req.Prompt, req.Project, containerNames); ok {
			payload = preset
		} else if aiErr != nil {
			c.JSON(500, gin.H{"error": "Erreur IA: " + aiErr.Error()})
			return
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "IA desactivee"})
			return
		}
	}

	// Inject files into containers via docker cp (reliable for large/complex content)
	files, _ := payload["files"].([]interface{})
	results := make([]gin.H, 0)
	for _, f := range files {
		fm, ok := f.(map[string]interface{})
		if !ok {
			continue
		}
		ctrName, _ := fm["container"].(string)
		filePath, _ := fm["path"].(string)
		content, _ := fm["content"].(string)
		desc, _ := fm["description"].(string)

		if ctrName == "" || filePath == "" || content == "" {
			continue
		}

		status := "ok"
		errMsg := ""
		if err := copyContentToContainer(ctrName, filePath, content); err != nil {
			status = "error"
			errMsg = err.Error()
		}

		results = append(results, gin.H{
			"container":   ctrName,
			"path":        filePath,
			"description": desc,
			"status":      status,
			"error":       errMsg,
		})
	}

	// Multi-site: when several sites land in their own subdirectories and no
	// root homepage was provided, inject a selector index.html so the nginx
	// landing page lets the visitor pick a site.
	if selectorHTML, nginxCtr, ok := buildMultiSiteSelector(req.Project, files); ok {
		status := "ok"
		errMsg := ""
		if err := copyContentToContainer(nginxCtr, "/usr/share/nginx/html/index.html", selectorHTML); err != nil {
			status = "error"
			errMsg = err.Error()
		}
		results = append(results, gin.H{
			"container":   nginxCtr,
			"path":        "/usr/share/nginx/html/index.html",
			"description": "Page d'accueil multi-site (selecteur de sites)",
			"status":      status,
			"error":       errMsg,
		})
	}

	// Reload nginx if it was modified
	for _, r := range results {
		ctr, _ := r["container"].(string)
		if strings.Contains(ctr, "nginx") {
			exec.Command("docker", "exec", ctr, "nginx", "-s", "reload").Run()
			break
		}
	}

	summary, _ := payload["summary"].(string)
	db.LogActivity(user, "ai_provision", req.Project, summary)

	c.JSON(200, gin.H{
		"status":  "ok",
		"summary": summary,
		"files":   results,
		"total":   len(results),
	})
}

// ---------------------------------------------------------------------------
// Magic Upload Image — upload images into nginx container
// ---------------------------------------------------------------------------

func handleMagicUploadImage(c *gin.Context) {
	project := c.PostForm("project")
	if project == "" {
		c.JSON(400, gin.H{"error": "project requis"})
		return
	}

	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(400, gin.H{"error": "Formulaire multipart requis"})
		return
	}

	files := form.File["images"]
	if len(files) == 0 {
		c.JSON(400, gin.H{"error": "Aucune image selectionnee"})
		return
	}

	// Find the nginx container for this project
	nginxContainer := project + "-nginx"
	// Ensure images directory exists
	exec.Command("docker", "exec", nginxContainer, "mkdir", "-p", "/usr/share/nginx/html/images").Run()

	uploaded := make([]gin.H, 0)
	for _, file := range files {
		// Save to temp
		tmpFile, err := os.CreateTemp("", "magic-img-*")
		if err != nil {
			continue
		}

		src, err := file.Open()
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
			continue
		}

		data, _ := io.ReadAll(src)
		src.Close()
		os.WriteFile(tmpFile.Name(), data, 0644)
		tmpFile.Close()

		// docker cp into nginx
		destPath := "/usr/share/nginx/html/images/" + file.Filename
		cmd := exec.Command("docker", "cp", tmpFile.Name(), nginxContainer+":"+destPath)
		out, err := cmd.CombinedOutput()
		os.Remove(tmpFile.Name())

		if err != nil {
			uploaded = append(uploaded, gin.H{"filename": file.Filename, "status": "error", "error": string(out)})
		} else {
			uploaded = append(uploaded, gin.H{
				"filename": file.Filename,
				"status":   "ok",
				"url":      "images/" + file.Filename,
			})
		}
	}

	c.JSON(200, gin.H{"status": "ok", "uploaded": uploaded, "total": len(uploaded)})
}

// ---------------------------------------------------------------------------
// Magic QR Code — generate QR code for deployed site URL
// ---------------------------------------------------------------------------

func handleMagicQRCode(c *gin.Context) {
	url := c.Query("url")
	if url == "" {
		c.JSON(400, gin.H{"error": "url requis"})
		return
	}
	png, err := qrcode.Encode(url, qrcode.Medium, 256)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.Data(200, "image/png", png)
}

func containerListOpts() container.ListOptions {
	return container.ListOptions{All: false}
}

func countVersions(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "compose_v") && strings.HasSuffix(e.Name(), ".yaml") {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Versioning: list versions
// ---------------------------------------------------------------------------

func handleProjectVersions(c *gin.Context) {
	name := c.Param("name")
	if !isSafeProjectName(name) {
		c.JSON(400, gin.H{"error": "Nom invalide"})
		return
	}
	versionsDir := filepath.Join(config.ProjectsDir, name, "versions")
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		c.JSON(200, gin.H{"status": "ok", "versions": []gin.H{}})
		return
	}
	versions := make([]gin.H, 0)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "compose_v") {
			continue
		}
		info, _ := e.Info()
		date, size := "", int64(0)
		if info != nil {
			date = info.ModTime().Format("2006-01-02 15:04:05")
			size = info.Size()
		}
		parts := strings.SplitN(strings.TrimPrefix(e.Name(), "compose_v"), "_", 2)
		versions = append(versions, gin.H{"filename": e.Name(), "version": parts[0], "date": date, "size": size})
	}
	// Sort descending by version number
	for i := 0; i < len(versions); i++ {
		for j := i + 1; j < len(versions); j++ {
			vi, _ := fmt.Sscanf(versions[i]["version"].(string), "%d", new(int))
			vj, _ := fmt.Sscanf(versions[j]["version"].(string), "%d", new(int))
			if vj > vi {
				versions[i], versions[j] = versions[j], versions[i]
			}
		}
	}
	c.JSON(200, gin.H{"status": "ok", "versions": versions})
}

// ---------------------------------------------------------------------------
// Versioning: rollback
// ---------------------------------------------------------------------------

func handleProjectRollback(c *gin.Context) {
	name := c.Param("name")
	if !isSafeProjectName(name) {
		c.JSON(400, gin.H{"error": "Nom invalide"})
		return
	}
	var req struct {
		Filename string `json:"filename"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Filename == "" {
		c.JSON(400, gin.H{"error": "filename requis"})
		return
	}
	if strings.Contains(req.Filename, "..") || strings.Contains(req.Filename, "/") || strings.Contains(req.Filename, "\\") {
		c.JSON(400, gin.H{"error": "Nom de fichier invalide"})
		return
	}

	projDir := filepath.Join(config.ProjectsDir, name)
	versionPath := filepath.Join(projDir, "versions", req.Filename)
	versionData, err := os.ReadFile(versionPath)
	if err != nil {
		c.JSON(404, gin.H{"error": "Version introuvable"})
		return
	}

	// Snapshot current before rollback
	currentPath := filepath.Join(projDir, "compose.yaml")
	if currentData, err := os.ReadFile(currentPath); err == nil {
		versionsDir := filepath.Join(projDir, "versions")
		vNum := countVersions(versionsDir) + 1
		ts := time.Now().Format("20060102_150405")
		os.WriteFile(filepath.Join(versionsDir, fmt.Sprintf("compose_v%d_%s.yaml", vNum, ts)), currentData, 0644)
	}

	// Write rollback version as current
	os.WriteFile(currentPath, versionData, 0644)

	// Re-deploy
	cmd := exec.Command("docker", "compose", "up", "-d", "--remove-orphans")
	cmd.Dir = projDir
	out, _ := cmd.CombinedOutput()

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "rollback", name, req.Filename)

	c.JSON(200, gin.H{"status": "ok", "logs": string(out)})
}

// ---------------------------------------------------------------------------
// Canvas Import: reverse-engineer compose.yaml into canvas nodes/links
// ---------------------------------------------------------------------------

func handleCanvasImport(c *gin.Context) {
	var req struct {
		Project string `json:"project"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Project == "" {
		c.JSON(400, gin.H{"error": "Nom de projet requis"})
		return
	}
	if !isSafeProjectName(req.Project) {
		c.JSON(400, gin.H{"error": "Nom invalide"})
		return
	}

	composePath := filepath.Join(config.ProjectsDir, req.Project, "compose.yaml")
	data, err := os.ReadFile(composePath)
	if err != nil {
		c.JSON(404, gin.H{"error": "compose.yaml introuvable"})
		return
	}

	var compose map[string]interface{}
	if err := yaml.Unmarshal(data, &compose); err != nil {
		c.JSON(500, gin.H{"error": "YAML invalide: " + err.Error()})
		return
	}

	nodes, links, err := services.ReverseComposeToCanvas(compose, req.Project)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	c.JSON(200, gin.H{"status": "ok", "nodes": nodes, "links": links, "project": req.Project})
}

// copyContentToContainer writes content to a temp file and docker-cp's it
// into the container, creating the target directory first. Container paths
// are POSIX: path.Dir is used on purpose (filepath.Dir would produce
// backslashes on Windows and break mkdir inside the Linux container).
func copyContentToContainer(ctrName, filePath, content string) error {
	tmpFile, err := os.CreateTemp("", "autodock-inject-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())
	if err := os.WriteFile(tmpFile.Name(), []byte(content), 0644); err != nil {
		tmpFile.Close()
		return err
	}
	tmpFile.Close()

	exec.Command("docker", "exec", ctrName, "mkdir", "-p", path.Dir(filePath)).Run()

	out, err := exec.Command("docker", "cp", tmpFile.Name(), ctrName+":"+filePath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// assetDirs are nginx webroot subdirectories that are not standalone sites.
var assetDirs = map[string]bool{
	"images": true, "img": true, "css": true, "js": true, "assets": true,
	"static": true, "fonts": true, "media": true, "cours": true,
}

// buildMultiSiteSelector detects several sites deployed in their own
// subdirectories of the same nginx webroot. When no root index.html was
// provided, it returns a generated selector homepage.
func buildMultiSiteSelector(project string, files []interface{}) (string, string, bool) {
	const webRoot = "/usr/share/nginx/html/"
	sites := map[string]bool{}
	nginxCtr := ""
	hasRootIndex := false
	for _, f := range files {
		fm, ok := f.(map[string]interface{})
		if !ok {
			continue
		}
		ctr, _ := fm["container"].(string)
		filePath, _ := fm["path"].(string)
		if !strings.Contains(ctr, "nginx") || !strings.HasPrefix(filePath, webRoot) {
			continue
		}
		nginxCtr = ctr
		rel := strings.TrimPrefix(filePath, webRoot)
		if rel == "index.html" {
			hasRootIndex = true
			continue
		}
		parts := strings.SplitN(rel, "/", 2)
		if len(parts) == 2 && parts[0] != "" && !assetDirs[strings.ToLower(parts[0])] {
			sites[parts[0]] = true
		}
	}
	if hasRootIndex || len(sites) < 2 || nginxCtr == "" {
		return "", "", false
	}
	names := make([]string, 0, len(sites))
	for s := range sites {
		names = append(names, s)
	}
	sort.Strings(names)
	return services.MultiSiteSelectorHTML(project, names), nginxCtr, true
}

// ---------------------------------------------------------------------------
// Compose editing: save (with versioning + validation) and AI-assisted edit
// ---------------------------------------------------------------------------

// composeWriteAllowed mirrors the deploy ACL: write role required when the
// project is registered, otherwise (imported/unregistered project) allowed.
func composeWriteAllowed(project, user string) bool {
	if getUserRoleOnProject(project, user) == "write" {
		return true
	}
	var owner sql.NullString
	err := db.DB.QueryRow("SELECT owner FROM labs WHERE project = ?", project).Scan(&owner)
	return err != nil || !owner.Valid
}

// validateComposeProject runs `docker compose config --quiet` against the
// compose.yaml already written in projDir. It returns the combined command
// output and a non-nil error when the file is not deployable.
func validateComposeProject(projDir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := runComposeCommand(ctx, projDir, "config", "--quiet")
	if err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
		}
		return msg, err
	}
	return "", nil
}

// validateComposeContent checks YAML syntax, then runs `docker compose config`
// when Docker is available. Returns nil when the content is deployable.
func validateComposeContent(projDir, content string) error {
	var parsed map[string]interface{}
	if err := yaml.Unmarshal([]byte(content), &parsed); err != nil {
		return fmt.Errorf("YAML invalide: %v", err)
	}
	if _, ok := parsed["services"]; !ok {
		return fmt.Errorf("section 'services' manquante")
	}
	if services.EnsureDocker() == nil {
		return nil // Docker absent: syntax check only
	}
	tmp := filepath.Join(projDir, ".compose_validate.yaml")
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		return nil
	}
	defer os.Remove(tmp)
	cmd := exec.Command("docker", "compose", "-f", tmp, "config", "--quiet")
	cmd.Dir = projDir
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("compose invalide: %s", truncate(msg, 400))
	}
	return nil
}

func handleComposeSave(c *gin.Context) {
	name := c.Param("name")
	if !isSafeProjectName(name) {
		c.JSON(400, gin.H{"error": "Nom invalide"})
		return
	}
	var req struct {
		Content  string `json:"content"`
		Redeploy bool   `json:"redeploy"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Content) == "" {
		c.JSON(400, gin.H{"error": "Contenu compose requis"})
		return
	}

	user, _ := middleware.GetSessionUser(c)
	if !composeWriteAllowed(name, user) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Acces refuse sur ce projet"})
		return
	}

	projDir := filepath.Join(config.ProjectsDir, name)
	composePath := filepath.Join(projDir, "compose.yaml")
	if _, err := os.Stat(composePath); os.IsNotExist(err) {
		c.JSON(404, gin.H{"error": "compose.yaml introuvable"})
		return
	}

	if err := validateComposeContent(projDir, req.Content); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	// Snapshot current version before overwriting
	if currentData, err := os.ReadFile(composePath); err == nil {
		versionsDir := filepath.Join(projDir, "versions")
		os.MkdirAll(versionsDir, 0755)
		vNum := countVersions(versionsDir) + 1
		ts := time.Now().Format("20060102_150405")
		os.WriteFile(filepath.Join(versionsDir, fmt.Sprintf("compose_v%d_%s.yaml", vNum, ts)), currentData, 0644)
	}

	if err := os.WriteFile(composePath, []byte(req.Content), 0644); err != nil {
		c.JSON(500, gin.H{"error": "Erreur ecriture compose.yaml"})
		return
	}

	logs := ""
	redeployed := false
	if req.Redeploy {
		cmd := exec.Command("docker", "compose", "up", "-d", "--remove-orphans")
		cmd.Dir = projDir
		out, err := cmd.CombinedOutput()
		logs = string(out)
		if err != nil {
			c.JSON(500, gin.H{"error": "Compose sauvegarde mais redeploiement echoue", "logs": truncate(logs, 800)})
			return
		}
		redeployed = true
	}

	db.LogActivity(user, "compose-edit", name, "")
	c.JSON(200, gin.H{"status": "ok", "redeployed": redeployed, "logs": truncate(logs, 800)})
}

func handleComposeAIEdit(c *gin.Context) {
	name := c.Param("name")
	if !isSafeProjectName(name) {
		c.JSON(400, gin.H{"error": "Nom invalide"})
		return
	}
	var req struct {
		Instruction string `json:"instruction"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Instruction) == "" {
		c.JSON(400, gin.H{"error": "Instruction requise"})
		return
	}

	user, _ := middleware.GetSessionUser(c)
	if !composeWriteAllowed(name, user) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Acces refuse sur ce projet"})
		return
	}

	projDir := filepath.Join(config.ProjectsDir, name)
	composePath := filepath.Join(projDir, "compose.yaml")
	currentData, err := os.ReadFile(composePath)
	if err != nil {
		c.JSON(404, gin.H{"error": "compose.yaml introuvable"})
		return
	}

	edited, err := services.EditComposeWithAI(string(currentData), req.Instruction)
	if err != nil {
		c.JSON(500, gin.H{"error": "Erreur IA: " + err.Error()})
		return
	}

	if err := validateComposeContent(projDir, edited); err != nil {
		c.JSON(422, gin.H{"error": "L'IA a produit un compose invalide: " + err.Error(), "yaml": edited})
		return
	}

	db.LogActivity(user, "compose-ai-edit", name, truncate(req.Instruction, 120))
	c.JSON(200, gin.H{
		"status": "ok",
		"yaml":   edited,
		"diff":   simpleDiff(string(currentData), edited),
	})
}

func simpleDiff(old, new string) string {
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(new, "\n")

	var sb strings.Builder
	sb.WriteString("--- compose.yaml (actuel)\n")
	sb.WriteString("+++ compose.yaml (nouveau)\n")

	// Simple line-by-line comparison (not a full unified diff, but sufficient for preview)
	maxLen := len(oldLines)
	if len(newLines) > maxLen {
		maxLen = len(newLines)
	}

	for i := 0; i < maxLen; i++ {
		oldLine := ""
		newLine := ""
		if i < len(oldLines) {
			oldLine = oldLines[i]
		}
		if i < len(newLines) {
			newLine = newLines[i]
		}
		if oldLine != newLine {
			if oldLine != "" {
				sb.WriteString("- " + oldLine + "\n")
			}
			if newLine != "" {
				sb.WriteString("+ " + newLine + "\n")
			}
		} else if oldLine != "" {
			sb.WriteString("  " + oldLine + "\n")
		}
	}

	return sb.String()
}
