package handlers

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// RegisterBackupRoutes registers all backup-related API routes.
func RegisterBackupRoutes(api *gin.RouterGroup) {
	g := api.Group("/backups")
	{
		g.GET("", handleListBackups)
		g.POST("/create", handleCreateBackup)
		g.POST("/restore", handleRestoreBackup)
		g.GET("/download/:id", handleDownloadBackup)
		g.DELETE("/:id", handleDeleteBackup)
		g.GET("/volumes", handleListVolumes)

		g.GET("/schedules", handleListSchedules)
		g.POST("/schedules", middleware.AdminRequired(), handleCreateOrUpdateSchedule)
		g.DELETE("/schedules/:id", middleware.AdminRequired(), handleDeleteSchedule)

		g.POST("/config/create", handleCreateConfigBackup)
		g.GET("/config/list", handleListConfigBackups)
		g.GET("/config/download/:id", handleDownloadConfigBackup)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func formatSize(sizeBytes int64) string {
	switch {
	case sizeBytes < 1024:
		return fmt.Sprintf("%d o", sizeBytes)
	case sizeBytes < 1024*1024:
		return fmt.Sprintf("%.1f Ko", float64(sizeBytes)/1024)
	case sizeBytes < 1024*1024*1024:
		return fmt.Sprintf("%.1f Mo", float64(sizeBytes)/(1024*1024))
	default:
		return fmt.Sprintf("%.2f Go", float64(sizeBytes)/(1024*1024*1024))
	}
}

func getSessionUser(c *gin.Context) string {
	s := sessions.Default(c)
	if u, ok := s.Get("user").(string); ok && u != "" {
		return u
	}
	return "unknown"
}

// ---------------------------------------------------------------------------
// Core: backup a Docker volume using alpine tar
// ---------------------------------------------------------------------------

func backupVolume(volumeName, project, createdBy string) (map[string]interface{}, error) {
	cli := services.EnsureDocker()
	if cli == nil {
		return nil, fmt.Errorf("Docker non connecte")
	}

	backupDir := filepath.Join(config.BackupsDir, project)
	os.MkdirAll(backupDir, 0755)

	filename := fmt.Sprintf("%s_%s.tar.gz", volumeName, time.Now().Format("20060102_150405"))
	nowStr := time.Now().Format("2006-01-02 15:04:05")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Run alpine container to create the tar.gz backup
	resp, err := cli.ContainerCreate(ctx, &container.Config{
		Image: "alpine:latest",
		Cmd:   []string{"tar", "czf", "/backup/" + filename, "-C", "/data", "."},
	}, &container.HostConfig{
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: volumeName, Target: "/data", ReadOnly: true},
			{Type: mount.TypeBind, Source: backupDir, Target: "/backup"},
		},
	}, &network.NetworkingConfig{}, nil, "")
	if err != nil {
		recordBackupHistory(project, volumeName, filename, 0, "error", nowStr, createdBy)
		return nil, fmt.Errorf("echec creation conteneur backup: %w", err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		recordBackupHistory(project, volumeName, filename, 0, "error", nowStr, createdBy)
		return nil, fmt.Errorf("echec demarrage conteneur backup: %w", err)
	}

	// Wait for container to finish
	statusCh, errCh := cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
			recordBackupHistory(project, volumeName, filename, 0, "error", nowStr, createdBy)
			return nil, fmt.Errorf("erreur attente conteneur: %w", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
			recordBackupHistory(project, volumeName, filename, 0, "error", nowStr, createdBy)
			return nil, fmt.Errorf("conteneur backup termine avec code %d", status.StatusCode)
		}
	}

	cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})

	// Get file size
	var sizeBytes int64
	fpath := filepath.Join(backupDir, filename)
	if info, err := os.Stat(fpath); err == nil {
		sizeBytes = info.Size()
	}

	recordBackupHistory(project, volumeName, filename, sizeBytes, "success", nowStr, createdBy)
	applyRetention(project, volumeName)

	return map[string]interface{}{
		"project":        project,
		"volume_name":    volumeName,
		"filename":       filename,
		"size_bytes":     sizeBytes,
		"size_formatted": formatSize(sizeBytes),
		"status":         "success",
		"created_at":     nowStr,
		"created_by":     createdBy,
	}, nil
}

func recordBackupHistory(project, volumeName, filename string, sizeBytes int64, status, createdAt, createdBy string) {
	_, _ = db.DB.Exec(
		"INSERT INTO backup_history (project, volume_name, filename, size_bytes, status, created_at, created_by) VALUES (?,?,?,?,?,?,?)",
		project, volumeName, filename, sizeBytes, status, createdAt, createdBy,
	)
}

func applyRetention(project, volumeName string) {
	var retentionCount int
	row := db.DB.QueryRow(
		"SELECT retention_count FROM backup_schedules WHERE project=? AND volume_name=? AND enabled=1 ORDER BY id DESC LIMIT 1",
		project, volumeName,
	)
	if err := row.Scan(&retentionCount); err != nil || retentionCount <= 0 {
		retentionCount = 5
	}

	rows, err := db.DB.Query(
		"SELECT id, filename FROM backup_history WHERE project=? AND volume_name=? AND status='success' ORDER BY created_at DESC",
		project, volumeName,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type backupEntry struct {
		id       int
		filename string
	}
	var all []backupEntry
	for rows.Next() {
		var e backupEntry
		rows.Scan(&e.id, &e.filename)
		all = append(all, e)
	}

	if len(all) <= retentionCount {
		return
	}

	backupDir := filepath.Join(config.BackupsDir, project)
	for _, e := range all[retentionCount:] {
		os.Remove(filepath.Join(backupDir, e.filename))
		db.DB.Exec("DELETE FROM backup_history WHERE id=?", e.id)
	}
}

// ---------------------------------------------------------------------------
// Restore
// ---------------------------------------------------------------------------

func restoreVolume(backupID int) (map[string]interface{}, error) {
	cli := services.EnsureDocker()
	if cli == nil {
		return nil, fmt.Errorf("Docker non connecte")
	}

	var project, volumeName, filename string
	err := db.DB.QueryRow(
		"SELECT project, volume_name, filename FROM backup_history WHERE id=?", backupID,
	).Scan(&project, &volumeName, &filename)
	if err != nil {
		return nil, fmt.Errorf("sauvegarde introuvable (id=%d)", backupID)
	}

	fpath := filepath.Join(config.BackupsDir, project, filename)
	if _, err := os.Stat(fpath); err != nil {
		return nil, fmt.Errorf("fichier de sauvegarde introuvable: %s", filename)
	}

	backupDir := filepath.Join(config.BackupsDir, project)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	resp, err := cli.ContainerCreate(ctx, &container.Config{
		Image: "alpine:latest",
		Cmd:   []string{"sh", "-c", fmt.Sprintf("rm -rf /data/* && tar xzf /backup/%s -C /data", filename)},
	}, &container.HostConfig{
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: volumeName, Target: "/data"},
			{Type: mount.TypeBind, Source: backupDir, Target: "/backup", ReadOnly: true},
		},
	}, &network.NetworkingConfig{}, nil, "")
	if err != nil {
		return nil, fmt.Errorf("echec creation conteneur restore: %w", err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("echec demarrage conteneur restore: %w", err)
	}

	statusCh, errCh := cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
			return nil, fmt.Errorf("erreur attente conteneur restore: %w", err)
		}
	case <-statusCh:
	}
	cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})

	return map[string]interface{}{
		"status":      "ok",
		"message":     fmt.Sprintf("Volume %s restaure depuis %s", volumeName, filename),
		"backup_id":   backupID,
		"volume_name": volumeName,
		"filename":    filename,
	}, nil
}

// ---------------------------------------------------------------------------
// Config backup (zip: auth.db, secrets/, compose files)
// ---------------------------------------------------------------------------

func backupAutodockerConfig(createdBy string) (map[string]interface{}, error) {
	configBackupDir := filepath.Join(config.BackupsDir, "__autodocker__")
	os.MkdirAll(configBackupDir, 0755)

	timestamp := time.Now().Format("20060102_150405")
	filename := fmt.Sprintf("autodocker_config_%s.zip", timestamp)
	fpath := filepath.Join(configBackupDir, filename)

	zf, err := os.Create(fpath)
	if err != nil {
		return nil, fmt.Errorf("impossible de creer le zip: %w", err)
	}
	zw := zip.NewWriter(zf)

	filesAdded := 0

	// 1. auth.db
	dbPath := filepath.Join(config.RootDir, "auth.db")
	if addFileToZip(zw, dbPath, "auth.db") == nil {
		filesAdded++
	}

	// 2. secrets/
	secretsDir := config.SecretsDir
	if entries, err := os.ReadDir(secretsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				src := filepath.Join(secretsDir, e.Name())
				if addFileToZip(zw, src, "secrets/"+e.Name()) == nil {
					filesAdded++
				}
			}
		}
	}

	// 3. projects/*/compose.*
	if entries, err := os.ReadDir(config.ProjectsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			projDir := filepath.Join(config.ProjectsDir, e.Name())
			matches, _ := filepath.Glob(filepath.Join(projDir, "compose.*"))
			for _, m := range matches {
				arcName := fmt.Sprintf("projects/%s/%s", e.Name(), filepath.Base(m))
				if addFileToZip(zw, m, arcName) == nil {
					filesAdded++
				}
			}
			envFile := filepath.Join(projDir, ".env")
			if _, err := os.Stat(envFile); err == nil {
				arcName := fmt.Sprintf("projects/%s/.env", e.Name())
				if addFileToZip(zw, envFile, arcName) == nil {
					filesAdded++
				}
			}
		}
	}

	// 4. Root config files
	for _, name := range []string{"CLAUDE.md", "requirements.txt", "compose_builder.py", "shared.py"} {
		src := filepath.Join(config.RootDir, name)
		if addFileToZip(zw, src, name) == nil {
			filesAdded++
		}
	}

	zw.Close()
	zf.Close()

	var sizeBytes int64
	if info, err := os.Stat(fpath); err == nil {
		sizeBytes = info.Size()
	}

	nowStr := time.Now().Format("2006-01-02 15:04:05")
	recordBackupHistory("__autodocker__", "config", filename, sizeBytes, "success", nowStr, createdBy)
	applyRetention("__autodocker__", "config")

	return map[string]interface{}{
		"project":        "__autodocker__",
		"volume_name":    "config",
		"filename":       filename,
		"size_bytes":     sizeBytes,
		"size_formatted": formatSize(sizeBytes),
		"files_included": filesAdded,
		"status":         "success",
		"created_at":     nowStr,
		"created_by":     createdBy,
	}, nil
}

func addFileToZip(zw *zip.Writer, srcPath, arcName string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	w, err := zw.Create(arcName)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

// ---------------------------------------------------------------------------
// Cron expression matching
// ---------------------------------------------------------------------------

func cronMatches(cronExpr string, now time.Time) bool {
	parts := strings.Fields(cronExpr)
	if len(parts) != 5 {
		return false
	}

	currentValues := []int{
		now.Minute(),
		now.Hour(),
		now.Day(),
		int(now.Month()),
		int(now.Weekday()), // 0=Sunday (Go convention, same as cron)
	}

	for i, field := range parts {
		if field == "*" {
			continue
		}
		allowed := make(map[int]bool)
		for _, token := range strings.Split(field, ",") {
			if strings.Contains(token, "-") {
				rangeParts := strings.SplitN(token, "-", 2)
				start, err1 := strconv.Atoi(rangeParts[0])
				end, err2 := strconv.Atoi(rangeParts[1])
				if err1 != nil || err2 != nil {
					return false
				}
				for v := start; v <= end; v++ {
					allowed[v] = true
				}
			} else {
				v, err := strconv.Atoi(token)
				if err != nil {
					return false
				}
				allowed[v] = true
			}
		}
		if !allowed[currentValues[i]] {
			return false
		}
	}
	return true
}

// CheckSchedules is called periodically by a background worker.
// It checks all enabled backup schedules and triggers backups when due.
func CheckSchedules() {
	now := time.Now()
	nowStr := now.Format("2006-01-02 15:04:05")
	currentPeriod := now.Format("2006-01-02 15:04")

	rows, err := db.DB.Query("SELECT id, project, volume_name, cron_expr, last_run, created_by FROM backup_schedules WHERE enabled=1")
	if err != nil {
		return
	}
	defer rows.Close()

	type schedule struct {
		id         int
		project    string
		volumeName string
		cronExpr   string
		lastRun    string
		createdBy  string
	}
	var schedules []schedule
	for rows.Next() {
		var s schedule
		rows.Scan(&s.id, &s.project, &s.volumeName, &s.cronExpr, &s.lastRun, &s.createdBy)
		schedules = append(schedules, s)
	}

	for _, sched := range schedules {
		if !cronMatches(sched.cronExpr, now) {
			continue
		}

		// Skip if already run this minute
		if strings.HasPrefix(sched.lastRun, currentPeriod) {
			continue
		}

		createdBy := sched.createdBy
		if createdBy == "" {
			createdBy = "scheduler"
		}

		var backupInfo map[string]interface{}
		var backupErr error

		if sched.project == "__autodocker__" && sched.volumeName == "config" {
			backupInfo, backupErr = backupAutodockerConfig(createdBy)
		} else {
			backupInfo, backupErr = backupVolume(sched.volumeName, sched.project, createdBy)
		}

		if backupErr != nil {
			db.LogActivity("scheduler", "backup_scheduled_error", sched.project, fmt.Sprintf("Erreur planification: %v", backupErr))
			continue
		}

		db.DB.Exec("UPDATE backup_schedules SET last_run=? WHERE id=?", nowStr, sched.id)

		sizeFormatted := ""
		if backupInfo != nil {
			if sf, ok := backupInfo["size_formatted"].(string); ok {
				sizeFormatted = sf
			}
		}
		db.LogActivity("scheduler", "backup_scheduled", sched.project,
			fmt.Sprintf("Volume %s sauvegarde (%s)", sched.volumeName, sizeFormatted))
	}
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

func handleListBackups(c *gin.Context) {
	project := c.Query("project")
	if project == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Parametre project requis"})
		return
	}

	rows, err := db.DB.Query(
		"SELECT id, project, volume_name, filename, size_bytes, status, created_at, created_by FROM backup_history WHERE project=? ORDER BY created_at DESC",
		project,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	defer rows.Close()

	var backups []gin.H
	for rows.Next() {
		var id int
		var proj, volName, filename, status, createdAt, createdBy string
		var sizeBytes int64
		rows.Scan(&id, &proj, &volName, &filename, &sizeBytes, &status, &createdAt, &createdBy)
		backups = append(backups, gin.H{
			"id":             id,
			"project":        proj,
			"volume_name":    volName,
			"filename":       filename,
			"size_bytes":     sizeBytes,
			"size_formatted": formatSize(sizeBytes),
			"status":         status,
			"created_at":     createdAt,
			"created_by":     createdBy,
		})
	}
	if backups == nil {
		backups = []gin.H{}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "backups": backups})
}

func handleCreateBackup(c *gin.Context) {
	var body struct {
		Project    string `json:"project"`
		VolumeName string `json:"volume_name"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Project == "" || body.VolumeName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Les champs project et volume_name sont requis"})
		return
	}

	username := getSessionUser(c)

	info, err := backupVolume(body.VolumeName, body.Project, username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	db.LogActivity(username, "backup_create", body.Project,
		fmt.Sprintf("Volume %s sauvegarde (%s)", body.VolumeName, info["size_formatted"]))
	c.JSON(http.StatusOK, gin.H{"status": "ok", "backup": info})
}

func handleRestoreBackup(c *gin.Context) {
	var body struct {
		BackupID int `json:"backup_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.BackupID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Champ backup_id requis"})
		return
	}

	username := getSessionUser(c)

	result, err := restoreVolume(body.BackupID)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "introuvable") {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"status": "error", "error": err.Error()})
		return
	}

	volName, _ := result["volume_name"].(string)
	fname, _ := result["filename"].(string)
	db.LogActivity(username, "backup_restore", volName, fmt.Sprintf("Restauration depuis %s", fname))
	c.JSON(http.StatusOK, gin.H{"status": "ok", "result": result})
}

func handleDownloadBackup(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	if id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "ID invalide"})
		return
	}

	var project, filename string
	err := db.DB.QueryRow("SELECT project, filename FROM backup_history WHERE id=?", id).Scan(&project, &filename)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "Sauvegarde introuvable"})
		return
	}

	fpath := filepath.Join(config.BackupsDir, project, filename)
	if _, err := os.Stat(fpath); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "Fichier introuvable sur le disque"})
		return
	}

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	c.File(fpath)
}

func handleDeleteBackup(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	if id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "ID invalide"})
		return
	}

	username := getSessionUser(c)

	var project, filename string
	err := db.DB.QueryRow("SELECT project, filename FROM backup_history WHERE id=?", id).Scan(&project, &filename)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "Sauvegarde introuvable"})
		return
	}

	fpath := filepath.Join(config.BackupsDir, project, filename)
	os.Remove(fpath)
	db.DB.Exec("DELETE FROM backup_history WHERE id=?", id)

	db.LogActivity(username, "backup_delete", project, fmt.Sprintf("Sauvegarde %s supprimee", filename))
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Sauvegarde supprimee"})
}

func handleListVolumes(c *gin.Context) {
	project := c.Query("project")
	if project == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Parametre project requis"})
		return
	}

	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "volumes": []string{}})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	volumeList, err := cli.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	var matching []string
	for _, v := range volumeList.Volumes {
		if strings.HasPrefix(v.Name, project+"_") || v.Name == project {
			matching = append(matching, v.Name)
		}
	}
	sort.Strings(matching)
	if matching == nil {
		matching = []string{}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "volumes": matching})
}

func handleListSchedules(c *gin.Context) {
	rows, err := db.DB.Query("SELECT id, project, volume_name, cron_expr, retention_count, enabled, last_run, created_by, created_at FROM backup_schedules ORDER BY project, volume_name")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	defer rows.Close()

	var schedules []gin.H
	for rows.Next() {
		var id, retentionCount, enabled int
		var project, volumeName, cronExpr, lastRun, createdBy, createdAt string
		rows.Scan(&id, &project, &volumeName, &cronExpr, &retentionCount, &enabled, &lastRun, &createdBy, &createdAt)
		schedules = append(schedules, gin.H{
			"id":              id,
			"project":         project,
			"volume_name":     volumeName,
			"cron_expr":       cronExpr,
			"retention_count": retentionCount,
			"enabled":         enabled == 1,
			"last_run":        lastRun,
			"created_by":      createdBy,
			"created_at":      createdAt,
		})
	}
	if schedules == nil {
		schedules = []gin.H{}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "schedules": schedules})
}

func handleCreateOrUpdateSchedule(c *gin.Context) {
	var body struct {
		ID             *int   `json:"id"`
		Project        string `json:"project"`
		VolumeName     string `json:"volume_name"`
		CronExpr       string `json:"cron_expr"`
		RetentionCount int    `json:"retention_count"`
		Enabled        *bool  `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Project == "" || body.VolumeName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Les champs project et volume_name sont requis"})
		return
	}

	if body.CronExpr == "" {
		body.CronExpr = "0 2 * * *"
	}
	if body.RetentionCount <= 0 {
		body.RetentionCount = 5
	}
	enabled := 1
	if body.Enabled != nil && !*body.Enabled {
		enabled = 0
	}
	username := getSessionUser(c)
	nowStr := time.Now().Format("2006-01-02 15:04:05")
	action := "creee"

	if body.ID != nil && *body.ID > 0 {
		_, err := db.DB.Exec(
			"UPDATE backup_schedules SET cron_expr=?, retention_count=?, enabled=? WHERE id=?",
			body.CronExpr, body.RetentionCount, enabled, *body.ID,
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
			return
		}
		action = "modifiee"
		db.LogActivity(username, "backup_schedule", body.Project,
			fmt.Sprintf("Planification %s pour %s (cron: %s)", action, body.VolumeName, body.CronExpr))
		c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Planification " + action, "schedule_id": *body.ID})
	} else {
		result, err := db.DB.Exec(
			"INSERT INTO backup_schedules (project, volume_name, cron_expr, retention_count, enabled, created_by, created_at) VALUES (?,?,?,?,?,?,?)",
			body.Project, body.VolumeName, body.CronExpr, body.RetentionCount, enabled, username, nowStr,
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
			return
		}
		schedID, _ := result.LastInsertId()
		db.LogActivity(username, "backup_schedule", body.Project,
			fmt.Sprintf("Planification %s pour %s (cron: %s)", action, body.VolumeName, body.CronExpr))
		c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Planification " + action, "schedule_id": schedID})
	}
}

func handleDeleteSchedule(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	if id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "ID invalide"})
		return
	}

	username := getSessionUser(c)

	var project, volumeName string
	err := db.DB.QueryRow("SELECT project, volume_name FROM backup_schedules WHERE id=?", id).Scan(&project, &volumeName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "Planification introuvable"})
		return
	}

	db.DB.Exec("DELETE FROM backup_schedules WHERE id=?", id)
	db.LogActivity(username, "backup_schedule_delete", project, fmt.Sprintf("Planification supprimee pour %s", volumeName))
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Planification supprimee"})
}

func handleCreateConfigBackup(c *gin.Context) {
	username := getSessionUser(c)
	info, err := backupAutodockerConfig(username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	db.LogActivity(username, "config_backup", "autodocker",
		fmt.Sprintf("Config sauvegardee (%s, %v fichiers)", info["size_formatted"], info["files_included"]))
	c.JSON(http.StatusOK, gin.H{"status": "ok", "backup": info})
}

func handleListConfigBackups(c *gin.Context) {
	rows, err := db.DB.Query(
		"SELECT id, filename, size_bytes, status, created_at, created_by FROM backup_history WHERE project='__autodocker__' ORDER BY created_at DESC",
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	defer rows.Close()

	var backups []gin.H
	for rows.Next() {
		var id int
		var filename, status, createdAt, createdBy string
		var sizeBytes int64
		rows.Scan(&id, &filename, &sizeBytes, &status, &createdAt, &createdBy)
		backups = append(backups, gin.H{
			"id":             id,
			"filename":       filename,
			"size_bytes":     sizeBytes,
			"size_formatted": formatSize(sizeBytes),
			"status":         status,
			"created_at":     createdAt,
			"created_by":     createdBy,
		})
	}
	if backups == nil {
		backups = []gin.H{}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "backups": backups})
}

func handleDownloadConfigBackup(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	if id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "ID invalide"})
		return
	}

	var filename string
	err := db.DB.QueryRow(
		"SELECT filename FROM backup_history WHERE id=? AND project='__autodocker__'", id,
	).Scan(&filename)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "Backup introuvable"})
		return
	}

	fpath := filepath.Join(config.BackupsDir, "__autodocker__", filename)
	if _, err := os.Stat(fpath); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "Fichier introuvable"})
		return
	}

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	c.File(fpath)
}

