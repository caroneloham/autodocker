package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"autodocker-go/config"
	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/gin-gonic/gin"
)

// RegisterToolsRoutes registers all tool/audit/network-map/image routes.
func RegisterToolsRoutes(api *gin.RouterGroup) {
	tools := api.Group("/tools")
	tools.Use(middleware.AdminRequired())
	{
		// Vuln scan
		tools.POST("/vuln-scan", handleVulnScan)
		tools.GET("/trivy-status", handleTrivyStatus)

		// Security audit
		tools.POST("/security-audit", handleSecurityAudit)

		// Audit history
		tools.GET("/audit-history", handleAuditHistory)
		tools.GET("/audit-history/:filename", handleAuditView)
		tools.DELETE("/audit-history/:filename", handleAuditDelete)

		// Hardening
		tools.POST("/hardening/preview", handleHardeningPreview)
		tools.POST("/hardening/apply", handleHardeningApply)
	}

	// Network map (admin required)
	adminGroup := api.Group("")
	adminGroup.Use(middleware.AdminRequired())
	{
		adminGroup.GET("/network-map", handleNetworkMap)
		adminGroup.POST("/network-map/connect", handleNetworkConnect)
		adminGroup.POST("/network-map/disconnect", handleNetworkDisconnect)

		// Images
		adminGroup.GET("/images", handleImagesList)
		adminGroup.POST("/images/pull", handleImagePull)
		adminGroup.DELETE("/images/:id", handleImageDelete)
	}
}

// ===========================================================================
// VULN SCAN
// ===========================================================================

func trivyPath() string {
	// Check next to executable first
	if exe, err := os.Executable(); err == nil {
		local := filepath.Join(filepath.Dir(exe), "trivy.exe")
		if _, err := os.Stat(local); err == nil {
			return local
		}
		local = filepath.Join(filepath.Dir(exe), "trivy")
		if _, err := os.Stat(local); err == nil {
			return local
		}
	}
	// Check in working directory
	if _, err := os.Stat("trivy.exe"); err == nil {
		return "./trivy.exe"
	}
	// Fallback to PATH
	return "trivy"
}

func trivyAvailable() (bool, string) {
	cmd := exec.Command(trivyPath(), "--version")
	out, err := cmd.Output()
	if err != nil {
		return false, ""
	}
	lines := strings.SplitN(string(out), "\n", 2)
	return true, strings.TrimSpace(lines[0])
}

type trivyVuln struct {
	ID        string `json:"id"`
	Severity  string `json:"severity"`
	Pkg       string `json:"pkg"`
	Installed string `json:"installed"`
	Fixed     string `json:"fixed"`
	Title     string `json:"title"`
}

type trivySummary struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
}

type trivyImageResult struct {
	Image   string        `json:"image"`
	Vulns   []trivyVuln   `json:"vulns"`
	Summary *trivySummary `json:"summary,omitempty"`
	Error   string        `json:"error,omitempty"`
}

func runTrivyScan(images []string) []trivyImageResult {
	var results []trivyImageResult
	for _, img := range images {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		cmd := exec.CommandContext(ctx, trivyPath(), "image", "--format", "json",
			"--severity", "CRITICAL,HIGH,MEDIUM", "--quiet", img)
		out, err := cmd.Output()
		cancel()

		if err != nil {
			errMsg := ""
			if ee, ok := err.(*exec.ExitError); ok {
				errMsg = string(ee.Stderr)
				if len(errMsg) > 200 {
					errMsg = errMsg[:200]
				}
			} else {
				errMsg = err.Error()
			}
			results = append(results, trivyImageResult{Image: img, Error: errMsg})
			continue
		}

		var data struct {
			Results []struct {
				Vulnerabilities []struct {
					VulnerabilityID  string `json:"VulnerabilityID"`
					Severity         string `json:"Severity"`
					PkgName          string `json:"PkgName"`
					InstalledVersion string `json:"InstalledVersion"`
					FixedVersion     string `json:"FixedVersion"`
					Title            string `json:"Title"`
				} `json:"Vulnerabilities"`
			} `json:"Results"`
		}
		if err := json.Unmarshal(out, &data); err != nil {
			results = append(results, trivyImageResult{Image: img, Error: "JSON parse error"})
			continue
		}

		var vulns []trivyVuln
		summary := trivySummary{}
		for _, r := range data.Results {
			for _, v := range r.Vulnerabilities {
				title := v.Title
				if len(title) > 100 {
					title = title[:100]
				}
				vulns = append(vulns, trivyVuln{
					ID:        v.VulnerabilityID,
					Severity:  v.Severity,
					Pkg:       v.PkgName,
					Installed: v.InstalledVersion,
					Fixed:     v.FixedVersion,
					Title:     title,
				})
				switch strings.ToLower(v.Severity) {
				case "critical":
					summary.Critical++
				case "high":
					summary.High++
				case "medium":
					summary.Medium++
				}
			}
		}
		results = append(results, trivyImageResult{Image: img, Vulns: vulns, Summary: &summary})
	}
	return results
}

func handleVulnScan(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Collect unique images
	seen := map[string]bool{}
	var uniqueImages []string
	for _, cont := range containers {
		img := cont.Image
		if img != "" && !seen[img] {
			seen[img] = true
			uniqueImages = append(uniqueImages, img)
		}
	}
	if len(uniqueImages) == 0 {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "results": "Aucune image trouvee."})
		return
	}

	// Try Trivy first
	trivyOK, trivyVer := trivyAvailable()
	if trivyOK {
		trivyResults := runTrivyScan(uniqueImages)
		c.JSON(http.StatusOK, gin.H{
			"status":  "ok",
			"mode":    "trivy",
			"version": trivyVer,
			"results": trivyResults,
		})
		return
	}

	// Fallback: placeholder
	c.JSON(http.StatusOK, gin.H{
		"status":       "ok",
		"mode":         "placeholder",
		"results":      "Trivy non disponible. Installez Trivy pour un scan de vulnerabilites reel.",
		"total_images": len(uniqueImages),
	})
}

func handleTrivyStatus(c *gin.Context) {
	available, version := trivyAvailable()
	c.JSON(http.StatusOK, gin.H{"available": available, "version": version})
}

// ===========================================================================
// SECURITY AUDIT
// ===========================================================================

func handleSecurityAudit(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}

	var body struct {
		Project string `json:"project"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Project == "" {
		body.Project = "unknown"
	}

	auditData, err := services.RunAudit(cli, body.Project)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	htmlReport := services.GenerateAuditHTML(auditData, body.Project, "")

	// Save to audits dir
	ts := time.Now().Format("20060102_150405")
	auditFilename := fmt.Sprintf("audit_%s_%s.html", body.Project, ts)
	auditPath := filepath.Join(config.AuditsDir, auditFilename)
	os.WriteFile(auditPath, []byte(htmlReport), 0644)

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"html":   htmlReport,
		"saved":  auditFilename,
		"audit":  auditData,
	})
}

// ===========================================================================
// AUDIT HISTORY
// ===========================================================================

func handleAuditHistory(c *gin.Context) {
	entries, err := os.ReadDir(config.AuditsDir)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "audits": []any{}})
		return
	}

	type auditEntry struct {
		Filename string `json:"filename"`
		Project  string `json:"project"`
		Date     string `json:"date"`
		Size     int64  `json:"size"`
	}

	var audits []auditEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "audit_") || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".html")
		parts := strings.Split(name, "_")
		project := "unknown"
		dateStr := ""
		if len(parts) > 3 {
			project = strings.Join(parts[1:len(parts)-2], "_")
			dateStr = parts[len(parts)-2] + "_" + parts[len(parts)-1]
		} else if len(parts) > 1 {
			project = parts[1]
		}
		t, err := time.Parse("20060102_150405", dateStr)
		if err != nil {
			t = info.ModTime()
		}
		audits = append(audits, auditEntry{
			Filename: e.Name(),
			Project:  project,
			Date:     t.Format("02/01/2006 15:04:05"),
			Size:     info.Size(),
		})
	}

	// Sort by date descending (newest first)
	sort.Slice(audits, func(i, j int) bool {
		return audits[i].Date > audits[j].Date
	})

	c.JSON(http.StatusOK, gin.H{"status": "ok", "audits": audits})
}

func handleAuditView(c *gin.Context) {
	filename := filepath.Base(c.Param("filename"))
	if !strings.HasSuffix(filename, ".html") {
		c.JSON(http.StatusNotFound, gin.H{"error": "Rapport introuvable"})
		return
	}
	auditPath := filepath.Join(config.AuditsDir, filename)
	data, err := os.ReadFile(auditPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Rapport introuvable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "html": string(data), "filename": filename})
}

func handleAuditDelete(c *gin.Context) {
	filename := filepath.Base(c.Param("filename"))
	auditPath := filepath.Join(config.AuditsDir, filename)
	if _, err := os.Stat(auditPath); os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Rapport introuvable"})
		return
	}
	if err := os.Remove(auditPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Rapport supprime"})
}

// ===========================================================================
// HARDENING
// ===========================================================================

func handleHardeningPreview(c *gin.Context) {
	var body struct {
		Project string `json:"project"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Project == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nom de projet requis"})
		return
	}
	composePath := filepath.Join(config.ProjectsDir, body.Project, "compose.yaml")
	if _, err := os.Stat(composePath); os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Fichier compose.yaml introuvable"})
		return
	}

	result := services.PrepareHardening(composePath)
	if result.Error != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": result.Error})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":      "ok",
		"diff":        result.Diff,
		"diff_lines":  result.DiffLines,
		"hardened":    result.Hardened,
		"backup_path": result.BackupPath,
	})
}

func handleHardeningApply(c *gin.Context) {
	var body struct {
		Project  string `json:"project"`
		Hardened string `json:"hardened"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Project == "" || body.Hardened == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Projet et contenu requis"})
		return
	}
	composePath := filepath.Join(config.ProjectsDir, body.Project, "compose.yaml")
	if _, err := os.Stat(composePath); os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Fichier compose.yaml introuvable"})
		return
	}

	// Write hardened content
	if err := os.WriteFile(composePath, []byte(body.Hardened), 0644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Restart containers
	projDir := filepath.Join(config.ProjectsDir, body.Project)
	restartOK := false
	restartMsg := ""
	cmd := exec.Command("docker", "compose", "up", "-d", "--force-recreate", "--remove-orphans")
	cmd.Dir = projDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		restartMsg = fmt.Sprintf("Compose ecrit mais erreur au redemarrage: %s", truncate(string(out), 300))
	} else {
		restartOK = true
		restartMsg = "Conteneurs redemarres avec le compose durci."
	}

	c.JSON(http.StatusOK, gin.H{
		"status":      "ok",
		"message":     "Hardening applique.",
		"restarted":   restartOK,
		"restart_msg": restartMsg,
	})
}

// ===========================================================================
// NETWORK MAP
// ===========================================================================

func handleNetworkMap(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	networks, err := cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	type netData struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Driver string `json:"driver"`
		Subnet string `json:"subnet"`
	}
	type contData struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Image  string `json:"image"`
		Status string `json:"status"`
		Ports  string `json:"ports"`
	}
	type linkData struct {
		ContainerID string `json:"container_id"`
		NetworkID   string `json:"network_id"`
		NetworkName string `json:"network_name"`
		IP          string `json:"ip"`
	}

	var networksData []netData
	var containersData []contData
	var links []linkData

	for _, net := range networks {
		if net.Name == "none" || net.Name == "host" {
			continue
		}
		subnet := ""
		if len(net.IPAM.Config) > 0 {
			subnet = net.IPAM.Config[0].Subnet
		}
		networksData = append(networksData, netData{
			ID: net.ID, Name: net.Name, Driver: net.Driver, Subnet: subnet,
		})
	}

	for _, cont := range containers {
		name := strings.TrimPrefix(cont.Names[0], "/")
		img := cont.Image

		var portsList []string
		seenPorts := make(map[string]bool)
		for _, p := range cont.Ports {
			if p.PublicPort > 0 {
				port := fmt.Sprintf("%d->%d", p.PublicPort, p.PrivatePort)
				if !seenPorts[port] {
					seenPorts[port] = true
					portsList = append(portsList, port)
				}
			}
		}
		portsStr := "-"
		if len(portsList) > 0 {
			sort.Strings(portsList)
			portsStr = strings.Join(portsList, ", ")
		}

		containersData = append(containersData, contData{
			ID: cont.ID, Name: name, Image: img, Status: cont.State, Ports: portsStr,
		})

		// Build network links from container network settings
		if cont.NetworkSettings != nil {
			for netName, netCfg := range cont.NetworkSettings.Networks {
				links = append(links, linkData{
					ContainerID: cont.ID,
					NetworkID:   netCfg.NetworkID,
					NetworkName: netName,
					IP:          netCfg.IPAddress,
				})
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"networks":   networksData,
		"containers": containersData,
		"links":      links,
	})
}

func handleNetworkConnect(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}
	var body struct {
		NetworkID   string `json:"network_id"`
		ContainerID string `json:"container_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Parametres invalides"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	err := cli.NetworkConnect(ctx, body.NetworkID, body.ContainerID, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Conteneur connecte au reseau"})
}

func handleNetworkDisconnect(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}
	var body struct {
		NetworkID   string `json:"network_id"`
		ContainerID string `json:"container_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Parametres invalides"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	err := cli.NetworkDisconnect(ctx, body.NetworkID, body.ContainerID, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Conteneur deconnecte du reseau"})
}

// ===========================================================================
// IMAGES
// ===========================================================================

func handleImagesList(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	images, err := cli.ImageList(ctx, image.ListOptions{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	type imgEntry struct {
		ID      string `json:"id"`
		ShortID string `json:"short_id"`
		Repo    string `json:"repo"`
		Tag     string `json:"tag"`
		Size    string `json:"size"`
		Created string `json:"created"`
	}

	var result []imgEntry
	for _, img := range images {
		tags := img.RepoTags
		if len(tags) == 0 {
			tags = []string{"<none>:<none>"}
		}
		for _, tag := range tags {
			parts := strings.SplitN(tag, ":", 2)
			repo := parts[0]
			tagName := "latest"
			if len(parts) > 1 {
				tagName = parts[1]
			}
			sizeMB := float64(img.Size) / 1024 / 1024
			created := time.Unix(img.Created, 0).Format("2006-01-02")
			shortID := img.ID
			if strings.HasPrefix(shortID, "sha256:") {
				shortID = shortID[7:]
			}
			if len(shortID) > 12 {
				shortID = shortID[:12]
			}
			result = append(result, imgEntry{
				ID:      img.ID,
				ShortID: shortID,
				Repo:    repo,
				Tag:     tagName,
				Size:    fmt.Sprintf("%.1f MB", sizeMB),
				Created: created,
			})
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Repo < result[j].Repo
	})

	c.JSON(http.StatusOK, gin.H{"status": "ok", "images": result})
}

func handleImagePull(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}
	var body struct {
		Image string `json:"image"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Image) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nom d'image requis"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 300*time.Second)
	defer cancel()

	reader, err := cli.ImagePull(ctx, strings.TrimSpace(body.Image), image.PullOptions{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer reader.Close()
	// Drain the reader to complete the pull
	buf := make([]byte, 4096)
	for {
		_, err := reader.Read(buf)
		if err != nil {
			break
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": fmt.Sprintf("Image %s telechargee", body.Image)})
}

func handleImageDelete(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}
	imageID := c.Param("id")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	_, err := cli.ImageRemove(ctx, imageID, image.RemoveOptions{Force: true})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Image supprimee"})
}

// truncate shortens a string to maxLen characters.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
