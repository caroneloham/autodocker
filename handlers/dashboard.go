package handlers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
)

// RegisterDashboardRoutes registers dashboard API routes.
func RegisterDashboardRoutes(api *gin.RouterGroup) {
	api.GET("/refresh", handleRefresh)
	api.GET("/docker/status", handleDockerStatus)
	api.POST("/docker/open-desktop", handleOpenDockerDesktop)
	api.GET("/project/:name/diagnose", handleProjectDiagnose)
	api.GET("/project/:name/log-summary", handleProjectLogSummary)
	api.GET("/project/:name/activity", handleProjectActivity)
	api.POST("/project/:name/repair", handleProjectRepair)
	api.POST("/delete_project", handleDeleteProject)
	api.POST("/bulk-action", handleBulkAction)
}

// ---------------------------------------------------------------------------
// Refresh — list all projects with their containers
// ---------------------------------------------------------------------------

type projectInfo struct {
	Name            string             `json:"name"`
	Status          string             `json:"status"`
	RuntimeState    string             `json:"runtime_state"`
	DockerAvailable bool               `json:"docker_available"`
	ModifiedAt      string             `json:"modified_at,omitempty"`
	Containers      []containerSummary `json:"containers"`
	Services        []string           `json:"services,omitempty"`
	Role            string             `json:"role"`
}

type containerSummary struct {
	ID     string `json:"id"`
	FullID string `json:"full_id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Image  string `json:"image"`
	Ports  string `json:"ports"`
}

type diagnoseCheck struct {
	Level  string `json:"level"`
	Title  string `json:"title"`
	Text   string `json:"text"`
	Action string `json:"action,omitempty"`
}

type diagnoseSummary struct {
	Tone    string `json:"tone"`
	Title   string `json:"title"`
	Message string `json:"message"`
}

type projectLogEntry struct {
	Timestamp     string `json:"timestamp"`
	ContainerName string `json:"container_name"`
	Severity      string `json:"severity"`
	Message       string `json:"message"`
}

type projectLogDigest struct {
	Summary           diagnoseSummary   `json:"summary"`
	Counts            map[string]int    `json:"counts"`
	Highlights        []projectLogEntry `json:"highlights"`
	Total             int               `json:"total"`
	RecommendedAction string            `json:"recommended_action"`
	Advice            projectLogAdvice  `json:"advice"`
}

type projectLogAdvice struct {
	Tone     string `json:"tone"`
	Meaning  string `json:"meaning"`
	NextStep string `json:"next_step"`
	Detail   string `json:"detail"`
}

type projectActivityEntry struct {
	Date     string `json:"date"`
	Username string `json:"username"`
	Action   string `json:"action"`
	Target   string `json:"target"`
	Details  string `json:"details"`
}

type portRemap struct {
	Service string
	OldPort int
	NewPort int
}

func dockerUnavailablePayload() gin.H {
	return gin.H{
		"available": false,
		"code":      "docker_unavailable",
		"title":     "Docker Desktop n'est pas pret",
		"message":   "Lancez Docker Desktop, attendez qu'il indique que le moteur tourne, puis relancez l'action.",
		"detail":    "AutoDocker garde vos projets et vos volumes. Cette erreur signifie seulement que le moteur Docker local ne repond pas.",
		"actions": []gin.H{
			{"label": "Ouvrir Docker Desktop", "hint": "Demarrez Docker Desktop depuis Windows, puis revenez ici."},
			{"label": "Reessayer", "hint": "Une fois Docker pret, cliquez de nouveau sur Demarrer, Reparer ou Diagnostiquer."},
		},
	}
}

func handleDockerStatus(c *gin.Context) {
	cli := services.InitDocker()
	if cli == nil {
		payload := dockerUnavailablePayload()
		payload["status"] = "ok"
		c.JSON(http.StatusOK, payload)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":    "ok",
		"available": true,
		"title":     "Docker est disponible",
		"message":   "AutoDocker peut lire et piloter les projets locaux.",
	})
}

func handleOpenDockerDesktop(c *gin.Context) {
	if runtime.GOOS != "windows" {
		c.JSON(http.StatusNotImplemented, gin.H{
			"status":  "error",
			"message": "Ouverture automatique de Docker Desktop disponible pour Windows uniquement.",
		})
		return
	}

	candidates := dockerDesktopCandidates()
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		cmd := exec.Command(candidate)
		cmd.Dir = filepath.Dir(candidate)
		if err := cmd.Start(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"status":  "error",
				"message": "Docker Desktop a ete trouve, mais AutoDocker n'a pas pu le lancer.",
				"error":   err.Error(),
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"status":  "ok",
			"message": "Docker Desktop est en cours d'ouverture. Attendez que le moteur soit pret, puis cliquez sur Verifier.",
			"path":    candidate,
		})
		return
	}

	c.JSON(http.StatusNotFound, gin.H{
		"status":  "error",
		"message": "Docker Desktop est introuvable sur les emplacements Windows habituels.",
		"checked": candidates,
	})
}

func dockerDesktopCandidates() []string {
	candidates := []string{}
	if override := strings.TrimSpace(os.Getenv("DOCKER_DESKTOP_PATH")); override != "" {
		candidates = append(candidates, override)
	}
	if programFiles := strings.TrimSpace(os.Getenv("ProgramFiles")); programFiles != "" {
		candidates = append(candidates, filepath.Join(programFiles, "Docker", "Docker", "Docker Desktop.exe"))
	}
	if programFilesX86 := strings.TrimSpace(os.Getenv("ProgramFiles(x86)")); programFilesX86 != "" {
		candidates = append(candidates, filepath.Join(programFilesX86, "Docker", "Docker", "Docker Desktop.exe"))
	}
	if localAppData := strings.TrimSpace(os.Getenv("LocalAppData")); localAppData != "" {
		candidates = append(candidates, filepath.Join(localAppData, "Docker", "Docker Desktop.exe"))
	}
	return candidates
}

func handleRefresh(c *gin.Context) {
	// Reuse the shared client and verify it once for the entire dashboard.
	// Keep a genuinely nil interface when Docker cannot be reached.
	var docker dockerContainerLister
	if cli := services.EnsureDocker(); cli != nil {
		docker = cli
	}
	handleRefreshWithDocker(c, docker)
}

func handleRefreshWithDocker(c *gin.Context, cli dockerContainerLister) {
	user, role := middleware.GetSessionUser(c)
	isAdmin := middleware.IsAdminOrAbove(role)

	projects := make(map[string]*projectInfo)

	// Scan projects directory
	if entries, err := os.ReadDir(config.ProjectsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				key := strings.ToLower(e.Name())
				defaultRole := "none"
				if isAdmin {
					defaultRole = "write"
				}
				projects[key] = &projectInfo{
					Name:       e.Name(),
					Status:     "inactive",
					Containers: make([]containerSummary, 0),
					Services:   readProjectComposeServices(filepath.Join(config.ProjectsDir, e.Name())),
					ModifiedAt: projectComposeModifiedAt(filepath.Join(config.ProjectsDir, e.Name())),
					Role:       defaultRole,
				}
			}
		}
	}

	// A successful connection alone is not enough: a failed container query
	// must never turn every configured project into an apparently prepared one.
	dockerAvailable := false
	if cli != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		defer cancel()

		containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
		if err == nil {
			dockerAvailable = true
			for _, ctr := range containers {
				projLabel := ""
				if ctr.Labels != nil {
					projLabel = ctr.Labels["com.docker.compose.project"]
				}
				if projLabel == "" {
					continue
				}
				key := strings.ToLower(projLabel)

				if _, exists := projects[key]; !exists {
					defaultRole := "none"
					if isAdmin {
						defaultRole = "write"
					}
					projects[key] = &projectInfo{
						Name:       projLabel,
						Status:     "inactive",
						Containers: make([]containerSummary, 0),
						Services:   make([]string, 0),
						Role:       defaultRole,
					}
				}

				// Build port display
				portsList := make([]string, 0)
				seenPorts := make(map[string]bool)
				for _, p := range ctr.Ports {
					if p.PublicPort > 0 {
						port := fmt.Sprintf("%d->%d", p.PublicPort, p.PrivatePort)
						if !seenPorts[port] {
							seenPorts[port] = true
							portsList = append(portsList, port)
						}
					}
				}
				portsDisplay := "-"
				if len(portsList) > 0 {
					sort.Strings(portsList)
					portsDisplay = strings.Join(portsList, ", ")
				}

				// Image name
				image := "unknown"
				if len(ctr.Image) > 0 {
					image = ctr.Image
				}

				// Short ID
				shortID := ctr.ID
				if len(shortID) > 12 {
					shortID = shortID[:12]
				}

				// Container name (strip leading /)
				name := ""
				if len(ctr.Names) > 0 {
					name = strings.TrimPrefix(ctr.Names[0], "/")
				}

				projects[key].Containers = append(projects[key].Containers, containerSummary{
					ID:     shortID,
					FullID: ctr.ID,
					Name:   name,
					State:  ctr.State,
					Image:  image,
					Ports:  portsDisplay,
				})

				if ctr.State == "running" {
					projects[key].Status = "active"
				}
			}
		}
	}

	// Apply RBAC for non-admin users
	if !isAdmin {
		rows, _ := db.DB.Query("SELECT LOWER(project), role FROM labs_acl WHERE username = ?", user)
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var pk, pr string
				if rows.Scan(&pk, &pr) == nil {
					if p, ok := projects[pk]; ok && p.Role != "write" {
						p.Role = pr
					}
				}
			}
		}

		rows2, _ := db.DB.Query("SELECT LOWER(project) FROM labs WHERE owner = ?", user)
		if rows2 != nil {
			defer rows2.Close()
			for rows2.Next() {
				var pk string
				if rows2.Scan(&pk) == nil {
					if p, ok := projects[pk]; ok {
						p.Role = "write"
					}
				}
			}
		}

		rows3, _ := db.DB.Query(`
			SELECT LOWER(ga.project), ga.role
			FROM groups_acl ga
			JOIN user_groups ug ON ga.group_name = ug.group_name
			WHERE ug.username = ?`, user)
		if rows3 != nil {
			defer rows3.Close()
			for rows3.Next() {
				var pk, pr string
				if rows3.Scan(&pk, &pr) == nil {
					if p, ok := projects[pk]; ok && p.Role != "write" {
						p.Role = pr
					}
				}
			}
		}
	}

	// Keep configured projects visible even when Docker has no container for them
	// yet. Basic mode should show something useful after a failed or pending start.
	output := make([]*projectInfo, 0)
	for _, p := range projects {
		if p.Role != "none" {
			p.DockerAvailable = dockerAvailable
			p.RuntimeState = projectRuntimeState(p.Containers, dockerAvailable)
			output = append(output, p)
		}
	}

	// Sort by name
	sort.Slice(output, func(i, j int) bool {
		return output[i].Name < output[j].Name
	})

	// The header also carries availability when the legacy array is empty.
	c.Header("X-AutoDocker-Docker-Available", strconv.FormatBool(dockerAvailable))
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, output)
}

// projectRuntimeState describes the observed containers, independently of the
// legacy active/inactive status retained for existing API consumers.
func projectRuntimeState(containers []containerSummary, dockerAvailable bool) string {
	if !dockerAvailable {
		return "docker_unavailable"
	}
	if len(containers) == 0 {
		return "prepared"
	}
	running := 0
	for _, ctr := range containers {
		if ctr.State == "running" {
			running++
		}
	}
	if running == len(containers) {
		return "running"
	}
	if running == 0 {
		return "stopped"
	}
	return "partial"
}

func projectComposeModifiedAt(projectDir string) string {
	for _, filename := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
		info, err := os.Stat(filepath.Join(projectDir, filename))
		if err == nil && info.Mode().IsRegular() {
			return info.ModTime().UTC().Format(time.RFC3339Nano)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Project diagnosis
// ---------------------------------------------------------------------------

func handleProjectDiagnose(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if !isSafeProjectName(name) {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Nom de projet invalide"})
		return
	}

	user, role := middleware.GetSessionUser(c)
	isAdmin := middleware.IsAdminOrAbove(role)
	if !isAdmin && getUserRoleOnProject(name, user) == "none" {
		c.JSON(http.StatusForbidden, gin.H{"status": "error", "error": "Permission refusee"})
		return
	}

	checks := make([]diagnoseCheck, 0, 8)
	actions := make([]gin.H, 0, 4)
	addCheck := func(level, title, text, action string) {
		checks = append(checks, diagnoseCheck{Level: level, Title: title, Text: text, Action: action})
	}

	projDir := filepath.Join(config.ProjectsDir, name)
	composePath := filepath.Join(projDir, "compose.yaml")
	composeContent, composeErr := os.ReadFile(composePath)
	if composeErr == nil {
		addCheck("ok", "Configuration trouvee", "compose.yaml est present. AutoDocker peut relire la definition du projet.", "Voir la configuration")
	} else {
		addCheck("warn", "Configuration absente", "compose.yaml est introuvable. Le projet peut venir de Docker directement ou avoir ete supprime partiellement.", "")
	}

	declaredVolumes := extractComposeSectionKeys(string(composeContent), "volumes")
	if len(declaredVolumes) > 0 {
		addCheck("ok", "Volumes declares", fmt.Sprintf("%d volume(s) persistants sont declares dans compose.yaml.", len(declaredVolumes)), "Sauvegarder")
	}

	cli := services.EnsureDocker()
	if cli == nil {
		addCheck("error", "Docker indisponible", "AutoDocker ne peut pas interroger Docker. Verifiez que Docker Desktop est lance.", "")
		c.JSON(http.StatusOK, gin.H{
			"status":  "ok",
			"project": name,
			"summary": diagnoseSummary{Tone: "error", Title: "Docker indisponible", Message: "Le diagnostic ne peut pas continuer tant que Docker n'est pas accessible."},
			"checks":  checks,
			"actions": actions,
		})
		return
	}
	addCheck("ok", "Docker accessible", "Docker repond correctement.", "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		addCheck("error", "Lecture des conteneurs impossible", err.Error(), "")
		c.JSON(http.StatusOK, gin.H{"status": "ok", "project": name, "summary": diagnoseSummary{Tone: "error", Title: "Diagnostic incomplet", Message: "AutoDocker n'a pas pu lire les conteneurs Docker."}, "checks": checks, "actions": actions})
		return
	}

	projectKey := strings.ToLower(name)
	projectContainerCount := 0
	exposedPorts := 0
	webLike := false
	stopped := make([]string, 0)
	running := 0
	for _, ctr := range containers {
		if ctr.Labels == nil || strings.ToLower(ctr.Labels["com.docker.compose.project"]) != projectKey {
			continue
		}
		projectContainerCount++
		containerName := readableContainerName(ctr.Names, ctr.ID)
		if ctr.State == "running" {
			running++
		} else {
			stopped = append(stopped, containerName)
		}
		for _, p := range ctr.Ports {
			if p.PublicPort > 0 {
				exposedPorts++
			}
		}
		if looksLikeWebService(containerName, ctr.Image) {
			webLike = true
		}
	}

	switch {
	case projectContainerCount == 0 && composeErr == nil:
		addCheck("warn", "Aucun conteneur detecte", "Le projet a une configuration, mais aucun conteneur Docker associe n'est visible. Essayez Demarrer.", "Demarrer")
		actions = append(actions, gin.H{"label": "Demarrer le projet", "action": "start", "detail": "Relance les services associes si les conteneurs existent encore."})
	case projectContainerCount == 0:
		addCheck("error", "Projet introuvable dans Docker", "Aucun conteneur ni compose exploitable n'a ete detecte pour ce projet.", "")
	default:
		addCheck("ok", "Services detectes", fmt.Sprintf("%d service(s) Docker sont associes au projet.", projectContainerCount), "")
	}

	if len(stopped) > 0 {
		addCheck("warn", "Services arretes", fmt.Sprintf("%d service(s) ne tournent pas : %s.", len(stopped), strings.Join(limitStrings(stopped, 4), ", ")), "Demarrer")
		actions = append(actions, gin.H{"label": "Demarrer", "action": "start", "detail": "Demarre les services arretes du projet."})
	} else if projectContainerCount > 0 {
		addCheck("ok", "Services en ligne", "Tous les conteneurs detectes sont demarres.", "")
	}

	if webLike && exposedPorts == 0 {
		addCheck("warn", "Aucun lien web expose", "Le projet ressemble a une application web, mais aucun port public n'est expose. Ouvrir le site risque de ne rien donner.", "Voir la configuration")
	} else if exposedPorts > 0 {
		addCheck("ok", "Lien web detecte", fmt.Sprintf("%d port(s) publics sont exposes.", exposedPorts), "Ouvrir")
	}

	missingVolumes := missingDockerVolumes(ctx, cli, declaredVolumes, name)
	if len(missingVolumes) > 0 {
		addCheck("warn", "Volumes manquants", fmt.Sprintf("%d volume(s) declares ne sont pas visibles dans Docker : %s.", len(missingVolumes), strings.Join(limitStrings(missingVolumes, 4), ", ")), "Demarrer")
	}

	tone := "ok"
	title := "Projet sain"
	message := "Aucun probleme evident detecte."
	for _, check := range checks {
		if check.Level == "error" {
			tone = "error"
			title = "Action requise"
			message = check.Text
			break
		}
		if check.Level == "warn" && tone != "error" {
			tone = "warn"
			title = "A verifier"
			message = check.Text
		}
	}
	if projectContainerCount > 0 && running == projectContainerCount && tone == "ok" {
		message = "Les services sont demarres et le projet semble exploitable."
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"project": name,
		"summary": diagnoseSummary{Tone: tone, Title: title, Message: message},
		"checks":  checks,
		"actions": actions,
	})
}

// ---------------------------------------------------------------------------
// Project log summary
// ---------------------------------------------------------------------------

func handleProjectLogSummary(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if !isSafeProjectName(name) {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Nom de projet invalide"})
		return
	}

	user, role := middleware.GetSessionUser(c)
	isAdmin := middleware.IsAdminOrAbove(role)
	if !isAdmin && getUserRoleOnProject(name, user) == "none" {
		c.JSON(http.StatusForbidden, gin.H{"status": "error", "error": "Permission refusee"})
		return
	}

	limit := 120
	if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
		if parsed, err := strconv.Atoi(rawLimit); err == nil {
			switch {
			case parsed < 20:
				limit = 20
			case parsed > 500:
				limit = 500
			default:
				limit = parsed
			}
		}
	}

	rows, err := db.DB.Query(`
		SELECT timestamp, container_name, severity, message
		FROM container_logs
		WHERE project = ?
		ORDER BY timestamp DESC, id DESC
		LIMIT ?`,
		name,
		limit,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	defer rows.Close()

	entries := make([]projectLogEntry, 0, limit)
	for rows.Next() {
		var entry projectLogEntry
		if err := rows.Scan(&entry.Timestamp, &entry.ContainerName, &entry.Severity, &entry.Message); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
			return
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	digest := buildProjectLogDigest(entries)
	c.JSON(http.StatusOK, gin.H{
		"status":             "ok",
		"project":            name,
		"summary":            digest.Summary,
		"counts":             digest.Counts,
		"highlights":         digest.Highlights,
		"total":              digest.Total,
		"recommended_action": digest.RecommendedAction,
		"advice":             digest.Advice,
	})
}

func buildProjectLogDigest(entries []projectLogEntry) projectLogDigest {
	digest := projectLogDigest{
		Counts: map[string]int{
			"error":   0,
			"warning": 0,
			"info":    0,
			"debug":   0,
		},
		Highlights: make([]projectLogEntry, 0, 5),
		Total:      len(entries),
	}

	errors := make([]projectLogEntry, 0, 5)
	warnings := make([]projectLogEntry, 0, 5)
	neutral := make([]projectLogEntry, 0, 5)
	for _, entry := range entries {
		entry.Severity = normalizeProjectLogSeverity(entry.Severity)
		entry.Message = conciseLogMessage(entry.Message)
		digest.Counts[entry.Severity]++
		if entry.Severity == "error" {
			errors = append(errors, entry)
		} else if entry.Severity == "warning" {
			warnings = append(warnings, entry)
		} else {
			neutral = append(neutral, entry)
		}
	}
	ordered := append(append(errors, warnings...), neutral...)
	for _, entry := range ordered {
		if len(digest.Highlights) >= 5 {
			break
		}
		digest.Highlights = append(digest.Highlights, entry)
	}

	switch {
	case digest.Total == 0:
		digest.Summary = diagnoseSummary{
			Tone:    "muted",
			Title:   "Aucun log centralise",
			Message: "AutoDocker n'a pas encore collecte de logs pour ce projet. Lancez le projet ou ouvrez un service pour commencer a voir son journal.",
		}
		digest.RecommendedAction = "Demarrer ou diagnostiquer le projet"
	case digest.Counts["error"] > 0:
		digest.Summary = diagnoseSummary{
			Tone:    "error",
			Title:   "Erreurs detectees",
			Message: fmt.Sprintf("%d erreur(s) dans les %d dernieres lignes. Ouvrez les logs ou lancez le diagnostic avant de modifier le projet.", digest.Counts["error"], digest.Total),
		}
		digest.RecommendedAction = "Voir les logs puis diagnostiquer"
	case digest.Counts["warning"] > 0:
		digest.Summary = diagnoseSummary{
			Tone:    "warn",
			Title:   "Avertissements recents",
			Message: fmt.Sprintf("%d avertissement(s) dans les %d dernieres lignes. Le projet peut fonctionner, mais merite une verification.", digest.Counts["warning"], digest.Total),
		}
		digest.RecommendedAction = "Verifier le projet"
	default:
		digest.Summary = diagnoseSummary{
			Tone:    "ok",
			Title:   "Logs recents propres",
			Message: fmt.Sprintf("%d ligne(s) lues sans erreur ni avertissement.", digest.Total),
		}
		digest.RecommendedAction = "Ouvrir le projet"
	}
	digest.Advice = inferProjectLogAdvice(digest)

	return digest
}

func inferProjectLogAdvice(digest projectLogDigest) projectLogAdvice {
	if digest.Total == 0 {
		return projectLogAdvice{
			Tone:     "muted",
			Meaning:  "Aucun journal n'est encore disponible pour ce projet.",
			NextStep: "Demarrez le projet ou lancez un diagnostic pour creer les premieres informations utiles.",
			Detail:   "Ce n'est pas forcement une erreur : AutoDocker n'a simplement rien a resumer pour le moment.",
		}
	}

	var problem projectLogEntry
	for _, entry := range digest.Highlights {
		if entry.Severity == "error" || entry.Severity == "warning" {
			problem = entry
			break
		}
	}

	if problem.Message == "" {
		return projectLogAdvice{
			Tone:     "ok",
			Meaning:  "Les logs recents ne montrent pas de probleme evident.",
			NextStep: "Ouvrez le projet ou faites une sauvegarde avant une modification importante.",
			Detail:   fmt.Sprintf("%d ligne(s) lues sans erreur ni avertissement prioritaire.", digest.Total),
		}
	}

	message := strings.ToLower(problem.Message)
	service := strings.TrimSpace(problem.ContainerName)
	if service == "" {
		service = "un service"
	}
	detail := fmt.Sprintf("Signal trouve dans %s : %s", service, problem.Message)

	switch {
	case containsAny(message, "address already in use", "port is already allocated", "bind: only one usage", "bind for 0.0.0.0"):
		return projectLogAdvice{
			Tone:     "warn",
			Meaning:  "Un port semble deja utilise par un autre service.",
			NextStep: "Cliquez sur Reparer ou Diagnostiquer pour remapper le port ou identifier le service qui bloque.",
			Detail:   detail,
		}
	case containsAny(message, "permission denied", "access denied", "operation not permitted"):
		return projectLogAdvice{
			Tone:     "warn",
			Meaning:  "Un service n'a pas les droits necessaires pour lire ou ecrire quelque chose.",
			NextStep: "Verifiez les volumes et lancez le diagnostic avant de modifier les fichiers a la main.",
			Detail:   detail,
		}
	case containsAny(message, "no space left", "disk full", "not enough space"):
		return projectLogAdvice{
			Tone:     "error",
			Meaning:  "L'espace disque ou le volume Docker semble plein.",
			NextStep: "Liberez de l'espace, puis relancez le projet. Faites une sauvegarde si des donnees importantes sont presentes.",
			Detail:   detail,
		}
	case containsAny(message, "connection refused", "connection failed", "could not connect", "can't connect", "cannot connect"):
		return projectLogAdvice{
			Tone:     "warn",
			Meaning:  "Un service essaie de joindre un autre service qui n'est pas pret ou pas demarre.",
			NextStep: "Cliquez sur Verifier et demarrer pour relancer l'ensemble dans le bon ordre.",
			Detail:   detail,
		}
	case containsAny(message, "access denied for user", "authentication failed", "password authentication failed", "invalid password"):
		return projectLogAdvice{
			Tone:     "error",
			Meaning:  "Un identifiant ou mot de passe de service semble incorrect.",
			NextStep: "Verifiez les secrets ou variables d'environnement avant de relancer le projet.",
			Detail:   detail,
		}
	case containsAny(message, "pull access denied", "manifest unknown", "not found", "image not found", "repository does not exist"):
		return projectLogAdvice{
			Tone:     "error",
			Meaning:  "Une image Docker ne semble pas disponible ou son nom est incorrect.",
			NextStep: "Verifiez la configuration du service ou choisissez une recette officielle AutoDocker.",
			Detail:   detail,
		}
	case containsAny(message, "timeout", "timed out", "deadline exceeded"):
		return projectLogAdvice{
			Tone:     "warn",
			Meaning:  "Un service met trop de temps a repondre.",
			NextStep: "Relancez le projet, puis ouvrez les logs complets si le message revient.",
			Detail:   detail,
		}
	}

	if problem.Severity == "error" {
		return projectLogAdvice{
			Tone:     "error",
			Meaning:  "Un service signale une erreur qui peut bloquer le projet.",
			NextStep: "Lancez Diagnostiquer, puis demandez a l'assistant d'expliquer ce message si besoin.",
			Detail:   detail,
		}
	}
	return projectLogAdvice{
		Tone:     "warn",
		Meaning:  "Un service signale un avertissement. Le projet peut fonctionner, mais il faut garder un oeil dessus.",
		NextStep: "Surveillez le projet ou lancez Diagnostiquer si quelque chose ne s'ouvre pas.",
		Detail:   detail,
	}
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func normalizeProjectLogSeverity(severity string) string {
	key := strings.ToLower(strings.TrimSpace(severity))
	switch {
	case strings.Contains(key, "error"), strings.Contains(key, "critical"), strings.Contains(key, "fatal"), strings.Contains(key, "panic"):
		return "error"
	case strings.Contains(key, "warn"):
		return "warning"
	case strings.Contains(key, "debug"):
		return "debug"
	default:
		return "info"
	}
}

func conciseLogMessage(message string) string {
	msg := strings.Join(strings.Fields(strings.TrimSpace(message)), " ")
	if len(msg) > 220 {
		return msg[:217] + "..."
	}
	return msg
}

// ---------------------------------------------------------------------------
// Project activity
// ---------------------------------------------------------------------------

func handleProjectActivity(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if !isSafeProjectName(name) {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Nom de projet invalide"})
		return
	}

	user, role := middleware.GetSessionUser(c)
	isAdmin := middleware.IsAdminOrAbove(role)
	if !isAdmin && getUserRoleOnProject(name, user) == "none" {
		c.JSON(http.StatusForbidden, gin.H{"status": "error", "error": "Permission refusee"})
		return
	}

	limit := 20
	if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
		if parsed, err := strconv.Atoi(rawLimit); err == nil {
			switch {
			case parsed < 5:
				limit = 5
			case parsed > 50:
				limit = 50
			default:
				limit = parsed
			}
		}
	}

	rows, err := db.DB.Query(`
		SELECT timestamp, username, action, target, details
		FROM activity_log
		WHERE target = ?
		   OR target GLOB ?
		   OR target GLOB ?
		ORDER BY timestamp DESC, id DESC
		LIMIT ?`,
		name,
		name+"-*",
		name+"_*",
		limit,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	defer rows.Close()

	entries := make([]projectActivityEntry, 0)
	for rows.Next() {
		var entry projectActivityEntry
		if err := rows.Scan(&entry.Date, &entry.Username, &entry.Action, &entry.Target, &entry.Details); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
			return
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":   "ok",
		"project":  name,
		"activity": entries,
	})
}

// ---------------------------------------------------------------------------
// Project repair (non destructive)
// ---------------------------------------------------------------------------

func handleProjectRepair(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if !isSafeProjectName(name) {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Nom de projet invalide"})
		return
	}

	user, role := middleware.GetSessionUser(c)
	if !middleware.IsAdminOrAbove(role) && getUserRoleOnProject(name, user) != "write" {
		c.JSON(http.StatusForbidden, gin.H{"status": "error", "error": "Permission refusee"})
		return
	}

	cli := services.EnsureDocker()
	if cli == nil {
		payload := dockerUnavailablePayload()
		payload["status"] = "error"
		payload["project"] = name
		payload["error"] = payload["message"]
		payload["steps"] = []gin.H{{
			"status": "error",
			"title":  payload["title"],
			"text":   payload["message"],
		}}
		c.JSON(http.StatusServiceUnavailable, payload)
		return
	}

	steps := make([]gin.H, 0, 4)
	addStep := func(status, title, text string) {
		steps = append(steps, gin.H{"status": status, "title": title, "text": text})
	}

	projDir := filepath.Join(config.ProjectsDir, name)
	composePath := filepath.Join(projDir, "compose.yaml")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if _, err := os.Stat(composePath); err == nil {
		addStep("ok", "Configuration trouvee", "compose.yaml est present.")
		if out, err := validateComposeProject(projDir); err != nil {
			addStep("error", "Configuration invalide", truncate(out, 500))
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"status":  "error",
				"project": name,
				"message": "La configuration Docker Compose doit etre corrigee avant reparation.",
				"steps":   steps,
			})
			return
		}
		addStep("ok", "Configuration valide", "docker compose config ne detecte pas d'erreur.")

		remaps, err := remapBusyComposePorts(ctx, cli, name, projDir, composePath)
		if err != nil {
			addStep("error", "Correction des ports impossible", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{
				"status":  "error",
				"project": name,
				"message": "AutoDocker n'a pas pu corriger les ports occupes.",
				"steps":   steps,
			})
			return
		}
		for _, remap := range remaps {
			addStep("ok", "Port corrige", fmt.Sprintf("%s : port %d remplace par %d.", remap.Service, remap.OldPort, remap.NewPort))
		}
		if len(remaps) > 0 {
			if out, err := validateComposeProject(projDir); err != nil {
				addStep("error", "Configuration invalide apres correction", truncate(out, 500))
				c.JSON(http.StatusUnprocessableEntity, gin.H{
					"status":  "error",
					"project": name,
					"message": "Les ports ont ete ajustes, mais le compose doit encore etre corrige.",
					"steps":   steps,
				})
				return
			}
			addStep("ok", "Configuration sauvegardee", "Les ports occupes ont ete remplaces et l'ancien compose a ete versionne.")
		}

		cmd := exec.CommandContext(ctx, "docker", "compose", "up", "-d", "--remove-orphans")
		cmd.Dir = projDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			addStep("error", "Relance impossible", truncate(strings.TrimSpace(string(out)), 800))
			c.JSON(http.StatusInternalServerError, gin.H{
				"status":  "error",
				"project": name,
				"message": "Docker Compose n'a pas pu relancer le projet.",
				"steps":   steps,
			})
			return
		}
		addStep("ok", "Projet relance", "Docker Compose a relance les services sans supprimer les volumes.")
		db.LogActivity(user, "project-repair", name, "docker compose up -d --remove-orphans")
		c.JSON(http.StatusOK, gin.H{
			"status":  "ok",
			"project": name,
			"message": "Reparation terminee. Les services ont ete relances.",
			"steps":   steps,
		})
		return
	}

	addStep("warn", "Configuration absente", "compose.yaml est introuvable. AutoDocker tente seulement de demarrer les conteneurs existants.")
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		addStep("error", "Lecture Docker impossible", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "project": name, "message": "Impossible de lire les conteneurs Docker.", "steps": steps})
		return
	}

	started := 0
	projectKey := strings.ToLower(name)
	for _, ctr := range containers {
		if ctr.Labels == nil || strings.ToLower(ctr.Labels["com.docker.compose.project"]) != projectKey || ctr.State == "running" {
			continue
		}
		if err := cli.ContainerStart(ctx, ctr.ID, container.StartOptions{}); err == nil {
			started++
		}
	}
	if started == 0 {
		addStep("warn", "Aucune action appliquee", "Aucun conteneur arrete n'a ete trouve pour ce projet.")
		c.JSON(http.StatusOK, gin.H{"status": "ok", "project": name, "message": "Aucune reparation automatique possible sans compose.yaml.", "steps": steps})
		return
	}

	addStep("ok", "Conteneurs demarres", fmt.Sprintf("%d conteneur(s) ont ete demarres.", started))
	db.LogActivity(user, "project-repair", name, fmt.Sprintf("%d conteneurs demarres", started))
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"project": name,
		"message": "Reparation terminee. Les conteneurs arretes ont ete demarres.",
		"steps":   steps,
	})
}

// ---------------------------------------------------------------------------
// Delete project
// ---------------------------------------------------------------------------

func handleDeleteProject(c *gin.Context) {
	var req struct {
		Name          string `json:"name"`
		RemoveVolumes bool   `json:"remove_volumes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No name"})
		return
	}

	if !isSafeProjectName(req.Name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nom de projet invalide"})
		return
	}

	user, role := middleware.GetSessionUser(c)
	if !isOwnerOrAdmin(req.Name, user, role) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Permission refusee : seul le proprietaire ou un admin peut supprimer"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	targetKey := strings.ToLower(req.Name)
	cli := services.EnsureDocker()
	dockerAvailable := cli != nil
	containersRemoved := 0
	volumesRemoved := 0
	dockerWarning := ""

	if cli != nil {
		// Remove containers belonging to this project when Docker is reachable.
		containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
		if err == nil {
			for _, ctr := range containers {
				if ctr.Labels != nil && strings.ToLower(ctr.Labels["com.docker.compose.project"]) == targetKey {
					if removeErr := cli.ContainerRemove(ctx, ctr.ID, container.RemoveOptions{Force: true}); removeErr == nil {
						containersRemoved++
					}
				}
			}
		} else {
			dockerWarning = "Docker n'a pas pu lister les conteneurs. Le dossier projet a quand meme ete supprime."
		}

		if req.RemoveVolumes {
			removed, err := removeProjectDockerVolumes(ctx, cli, req.Name)
			if err == nil {
				volumesRemoved = removed
			} else if dockerWarning == "" {
				dockerWarning = "Le projet a ete supprime, mais certains volumes Docker n'ont pas pu etre supprimes."
			}
		}
	} else {
		if req.RemoveVolumes {
			dockerWarning = "Docker Desktop n'etait pas disponible. AutoDocker a supprime les fichiers locaux et les permissions, mais les volumes Docker n'ont pas ete touches."
		} else {
			dockerWarning = "Docker Desktop n'etait pas disponible. AutoDocker a supprime les fichiers locaux et les permissions du projet."
		}
	}

	// Remove project directory
	projDir := filepath.Join(config.ProjectsDir, req.Name)
	if _, err := os.Stat(projDir); err == nil {
		if err := os.RemoveAll(projDir); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "Impossible de supprimer le dossier projet"})
			return
		}
	}

	if cli != nil {
		// Prune networks only when Docker is reachable.
		_, _ = cli.NetworksPrune(ctx, filters.NewArgs())
	}

	metadataRemoved, metaErr := deleteProjectMetadata(req.Name)
	if metaErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "Projet supprime, mais nettoyage des permissions incomplet"})
		return
	}

	db.LogActivity(user, "delete-project", req.Name, "")
	message := "Projet supprime."
	if dockerWarning != "" {
		message = dockerWarning
	}
	c.JSON(http.StatusOK, gin.H{
		"status":             "ok",
		"message":            message,
		"docker_available":   dockerAvailable,
		"containers_removed": containersRemoved,
		"volumes_removed":    volumesRemoved,
		"volumes_requested":  req.RemoveVolumes,
		"metadata_removed":   metadataRemoved,
	})
}

func removeProjectDockerVolumes(ctx context.Context, cli interface {
	VolumeList(context.Context, volume.ListOptions) (volume.ListResponse, error)
	VolumeRemove(context.Context, string, bool) error
}, project string) (int, error) {
	volumeList, err := cli.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return 0, err
	}
	removed := 0
	var firstErr error
	for _, v := range volumeList.Volumes {
		if !volumeBelongsToProject(v, project) {
			continue
		}
		if err := cli.VolumeRemove(ctx, v.Name, true); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

func volumeBelongsToProject(v *volume.Volume, project string) bool {
	if v == nil {
		return false
	}
	projectKey := strings.ToLower(project)
	if v.Labels != nil {
		if labelProject := strings.TrimSpace(v.Labels["com.docker.compose.project"]); labelProject != "" {
			return strings.ToLower(labelProject) == projectKey
		}
	}
	return strings.HasPrefix(strings.ToLower(v.Name), projectKey+"_")
}

func deleteProjectMetadata(project string) (int64, error) {
	tables := []string{
		"labs_acl",
		"groups_acl",
		"backup_schedules",
		"secrets",
		"scheduled_tasks",
		"container_logs",
		"uptime_events",
		"scaling_events",
		"metrics_history",
		"labs",
	}
	var total int64
	for _, table := range tables {
		res, err := db.DB.Exec("DELETE FROM "+table+" WHERE project = ?", project)
		if err != nil {
			return total, err
		}
		affected, _ := res.RowsAffected()
		total += affected
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// Bulk action (start/stop all containers in a project)
// ---------------------------------------------------------------------------

func handleBulkAction(c *gin.Context) {
	handleBulkActionUsing(c, func() containerBulkClient {
		if cli := services.EnsureDocker(); cli != nil {
			return cli
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func extractComposeSectionKeys(content, section string) []string {
	keys := make([]string, 0)
	inside := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && strings.HasSuffix(trimmed, ":") {
			inside = strings.TrimSuffix(trimmed, ":") == section
			continue
		}
		if !inside {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			break
		}
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") {
			parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
			key := strings.TrimSpace(parts[0])
			if key != "" && !strings.HasPrefix(key, "-") {
				keys = append(keys, key)
			}
		}
	}
	return keys
}

func readProjectComposeServices(projectDir string) []string {
	for _, filename := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
		data, err := os.ReadFile(filepath.Join(projectDir, filename))
		if err == nil {
			return extractComposeSectionKeys(string(data), "services")
		}
	}
	return []string{}
}

func readableContainerName(names []string, id string) string {
	if len(names) > 0 {
		return strings.TrimPrefix(names[0], "/")
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func looksLikeWebService(name, image string) bool {
	text := strings.ToLower(name + " " + image)
	webMarkers := []string{
		"wordpress", "nginx", "traefik", "open-webui", "grafana", "nextcloud",
		"gitea", "n8n", "moodle", "portainer", "minio", "adminer", "pgadmin",
	}
	for _, marker := range webMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func limitStrings(values []string, max int) []string {
	if len(values) <= max {
		return values
	}
	out := append([]string{}, values[:max]...)
	out = append(out, fmt.Sprintf("+%d autre(s)", len(values)-max))
	return out
}

func missingDockerVolumes(ctx context.Context, cli dockerVolumeLister, declared []string, project string) []string {
	if len(declared) == 0 {
		return nil
	}
	volumeList, err := cli.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil
	}
	existing := make(map[string]bool, len(volumeList.Volumes))
	for _, v := range volumeList.Volumes {
		existing[v.Name] = true
	}
	missing := make([]string, 0)
	for _, declaredName := range declared {
		candidates := []string{declaredName, project + "_" + declaredName}
		found := false
		for _, candidate := range candidates {
			if existing[candidate] {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, declaredName)
		}
	}
	return missing
}

type dockerVolumeLister interface {
	VolumeList(context.Context, volume.ListOptions) (volume.ListResponse, error)
}

func remapBusyComposePorts(ctx context.Context, cli dockerContainerLister, project, projDir, composePath string) ([]portRemap, error) {
	data, err := os.ReadFile(composePath)
	if err != nil {
		return nil, err
	}

	var doc map[string]interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("lecture YAML impossible: %w", err)
	}
	servicesMap := asStringMap(doc["services"])
	if len(servicesMap) == 0 {
		return nil, nil
	}

	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	ownedPorts := projectOwnedPublishedPorts(containers, project)
	reserved := collectDeclaredPublishedPorts(servicesMap)
	remaps := make([]portRemap, 0)

	for serviceName, rawService := range servicesMap {
		serviceMap := asStringMap(rawService)
		rawPorts, ok := serviceMap["ports"]
		if !ok {
			continue
		}
		ports, ok := rawPorts.([]interface{})
		if !ok {
			continue
		}
		changed := false
		for i, rawPort := range ports {
			oldPort, ok := composePublishedPort(rawPort)
			if !ok || oldPort <= 0 || ownedPorts[oldPort] || portAvailable(oldPort) {
				continue
			}
			newPort, found := findReplacementPort(oldPort+1, reserved, ownedPorts)
			if !found {
				return remaps, fmt.Errorf("aucun port libre trouve apres %d", oldPort)
			}
			ports[i] = replaceComposePublishedPort(rawPort, newPort)
			delete(reserved, oldPort)
			reserved[newPort] = true
			changed = true
			remaps = append(remaps, portRemap{Service: serviceName, OldPort: oldPort, NewPort: newPort})
		}
		if changed {
			serviceMap["ports"] = ports
		}
	}

	if len(remaps) == 0 {
		return remaps, nil
	}

	if err := snapshotComposeVersion(projDir, data); err != nil {
		return remaps, err
	}
	newData, err := yaml.Marshal(doc)
	if err != nil {
		return remaps, err
	}
	if err := os.WriteFile(composePath, newData, 0644); err != nil {
		return remaps, err
	}
	return remaps, nil
}

type dockerContainerLister interface {
	ContainerList(context.Context, container.ListOptions) ([]types.Container, error)
}

func projectOwnedPublishedPorts(containers []types.Container, project string) map[int]bool {
	owned := make(map[int]bool)
	projectKey := strings.ToLower(project)
	for _, ctr := range containers {
		if ctr.Labels == nil || strings.ToLower(ctr.Labels["com.docker.compose.project"]) != projectKey {
			continue
		}
		for _, p := range ctr.Ports {
			if p.PublicPort > 0 {
				owned[int(p.PublicPort)] = true
			}
		}
	}
	return owned
}

func collectDeclaredPublishedPorts(servicesMap map[string]interface{}) map[int]bool {
	ports := make(map[int]bool)
	for _, rawService := range servicesMap {
		serviceMap := asStringMap(rawService)
		rawPorts, ok := serviceMap["ports"]
		if !ok {
			continue
		}
		portList, ok := rawPorts.([]interface{})
		if !ok {
			continue
		}
		for _, rawPort := range portList {
			if published, ok := composePublishedPort(rawPort); ok && published > 0 {
				ports[published] = true
			}
		}
	}
	return ports
}

func asStringMap(raw interface{}) map[string]interface{} {
	switch v := raw.(type) {
	case map[string]interface{}:
		return v
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(v))
		for key, value := range v {
			out[fmt.Sprint(key)] = value
		}
		return out
	default:
		return nil
	}
}

func composePublishedPort(raw interface{}) (int, bool) {
	switch v := raw.(type) {
	case string:
		return parseShortPublishedPort(v)
	case map[string]interface{}:
		return numberLikePort(v["published"])
	case map[interface{}]interface{}:
		return numberLikePort(v["published"])
	default:
		return 0, false
	}
}

func parseShortPublishedPort(spec string) (int, bool) {
	if strings.ContainsAny(spec, "$-") {
		return 0, false
	}
	withoutProto := strings.SplitN(spec, "/", 2)[0]
	parts := strings.Split(withoutProto, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	hostPart := strings.TrimSpace(parts[len(parts)-2])
	if hostPart == "" {
		return 0, false
	}
	port, err := strconv.Atoi(hostPart)
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

func replaceComposePublishedPort(raw interface{}, newPort int) interface{} {
	switch v := raw.(type) {
	case string:
		return replaceShortPublishedPort(v, newPort)
	case map[string]interface{}:
		v["published"] = samePortValueType(v["published"], newPort)
		return v
	case map[interface{}]interface{}:
		v["published"] = samePortValueType(v["published"], newPort)
		return v
	default:
		return raw
	}
}

func replaceShortPublishedPort(spec string, newPort int) string {
	proto := ""
	withoutProto := spec
	if parts := strings.SplitN(spec, "/", 2); len(parts) == 2 {
		withoutProto = parts[0]
		proto = "/" + parts[1]
	}
	parts := strings.Split(withoutProto, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return spec
	}
	parts[len(parts)-2] = strconv.Itoa(newPort)
	return strings.Join(parts, ":") + proto
}

func numberLikePort(raw interface{}) (int, bool) {
	switch v := raw.(type) {
	case int:
		return validPort(v)
	case int64:
		return validPort(int(v))
	case uint64:
		return validPort(int(v))
	case float64:
		return validPort(int(v))
	case string:
		if strings.ContainsAny(v, "$-") {
			return 0, false
		}
		p, err := strconv.Atoi(v)
		if err != nil {
			return 0, false
		}
		return validPort(p)
	default:
		return 0, false
	}
}

func validPort(port int) (int, bool) {
	if port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

func samePortValueType(raw interface{}, port int) interface{} {
	if _, ok := raw.(string); ok {
		return strconv.Itoa(port)
	}
	return port
}

func findReplacementPort(start int, reserved, owned map[int]bool) (int, bool) {
	if start < 1024 {
		start = 1024
	}
	for port := start; port <= 65535; port++ {
		if reserved[port] || owned[port] || !portAvailable(port) {
			continue
		}
		return port, true
	}
	return 0, false
}

func portAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func snapshotComposeVersion(projDir string, data []byte) error {
	versionsDir := filepath.Join(projDir, "versions")
	if err := os.MkdirAll(versionsDir, 0755); err != nil {
		return err
	}
	vNum := countVersions(versionsDir) + 1
	ts := time.Now().Format("20060102-150405")
	return os.WriteFile(filepath.Join(versionsDir, fmt.Sprintf("compose_v%d_%s.yaml", vNum, ts)), data, 0644)
}

func isOwnerOrAdmin(project, username, role string) bool {
	if middleware.IsAdminOrAbove(role) {
		return true
	}
	var owner string
	err := db.DB.QueryRow("SELECT owner FROM labs WHERE project = ?", project).Scan(&owner)
	return err == nil && owner == username
}
