package handlers

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"

	"github.com/gin-gonic/gin"
)

// Human-readable schedule presets (NO cron visible to user)
var schedulePresets = map[string]string{
	"every_hour":         "0 * * * *",
	"every_6h":           "0 */6 * * *",
	"every_day_2am":      "0 2 * * *",
	"every_day_midnight": "0 0 * * *",
	"every_week_monday":  "0 2 * * 1",
	"every_month_1st":    "0 2 1 * *",
}

var presetLabels = map[string]string{
	"every_hour":         "Toutes les heures",
	"every_6h":           "Toutes les 6 heures",
	"every_day_2am":      "Chaque jour a 2h du matin",
	"every_day_midnight": "Chaque jour a minuit",
	"every_week_monday":  "Chaque lundi a 2h",
	"every_month_1st":    "Le 1er de chaque mois",
}

var taskTypeLabels = map[string]string{
	"backup_db":      "Sauvegarde base de donnees",
	"restart":        "Redemarrer le conteneur",
	"cleanup_logs":   "Nettoyer les logs",
	"custom_command": "Commande personnalisee",
}

// RegisterScheduledTasksRoutes registers scheduled tasks endpoints.
func RegisterScheduledTasksRoutes(api *gin.RouterGroup) {
	t := api.Group("/scheduled-tasks")
	{
		t.GET("", handleListScheduledTasks)
		t.GET("/presets", handleGetPresets)
		t.POST("", middleware.AdminRequired(), handleCreateScheduledTask)
		t.PUT("/:id", middleware.AdminRequired(), handleToggleScheduledTask)
		t.DELETE("/:id", middleware.AdminRequired(), handleDeleteScheduledTask)
	}
}

// GET /api/scheduled-tasks?project=X
func handleListScheduledTasks(c *gin.Context) {
	project := c.Query("project")
	query := "SELECT id, project, container_name, task_type, schedule_preset, command, enabled, last_run, next_run, created_by, created_at FROM scheduled_tasks"
	args := []interface{}{}
	if project != "" {
		query += " WHERE project = ?"
		args = append(args, project)
	}
	query += " ORDER BY created_at DESC"

	rows, err := db.DB.Query(query, args...)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	tasks := make([]gin.H, 0)
	for rows.Next() {
		var id, enabled int
		var proj, cname, ttype, preset, cmd, lastRun, nextRun, createdBy, createdAt string
		if rows.Scan(&id, &proj, &cname, &ttype, &preset, &cmd, &enabled, &lastRun, &nextRun, &createdBy, &createdAt) == nil {
			tasks = append(tasks, gin.H{
				"id":             id,
				"project":        proj,
				"container_name": cname,
				"task_type":      ttype,
				"task_label":     taskTypeLabels[ttype],
				"schedule_preset": preset,
				"schedule_label": presetLabels[preset],
				"command":        cmd,
				"enabled":        enabled == 1,
				"last_run":       lastRun,
				"next_run":       nextRun,
				"created_by":     createdBy,
				"created_at":     createdAt,
			})
		}
	}
	c.JSON(200, gin.H{"status": "ok", "tasks": tasks})
}

// GET /api/scheduled-tasks/presets
func handleGetPresets(c *gin.Context) {
	schedules := make([]gin.H, 0)
	for k, v := range presetLabels {
		schedules = append(schedules, gin.H{"key": k, "label": v})
	}
	taskTypes := make([]gin.H, 0)
	for k, v := range taskTypeLabels {
		taskTypes = append(taskTypes, gin.H{"key": k, "label": v})
	}
	c.JSON(200, gin.H{"status": "ok", "schedules": schedules, "task_types": taskTypes})
}

// POST /api/scheduled-tasks
func handleCreateScheduledTask(c *gin.Context) {
	var req struct {
		Project       string `json:"project"`
		ContainerName string `json:"container_name"`
		TaskType      string `json:"task_type"`
		SchedulePreset string `json:"schedule_preset"`
		Command       string `json:"command"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "Donnees invalides"})
		return
	}
	if req.Project == "" || req.TaskType == "" || req.SchedulePreset == "" {
		c.JSON(400, gin.H{"error": "project, task_type et schedule_preset requis"})
		return
	}
	if _, ok := schedulePresets[req.SchedulePreset]; !ok {
		c.JSON(400, gin.H{"error": "Preset invalide"})
		return
	}
	if _, ok := taskTypeLabels[req.TaskType]; !ok {
		c.JSON(400, gin.H{"error": "Type de tache invalide"})
		return
	}

	cron := schedulePresets[req.SchedulePreset]
	user, _ := middleware.GetSessionUser(c)
	now := time.Now().Format("2006-01-02 15:04:05")

	_, err := db.DB.Exec(
		`INSERT INTO scheduled_tasks (project, container_name, task_type, schedule_preset, custom_cron, command, enabled, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		req.Project, req.ContainerName, req.TaskType, req.SchedulePreset, cron, req.Command, user, now,
	)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	db.LogActivity(user, "scheduled_task_create", req.Project, req.TaskType+" "+req.ContainerName)
	c.JSON(200, gin.H{"status": "ok", "message": "Tache planifiee creee"})
}

// PUT /api/scheduled-tasks/:id — toggle enabled
func handleToggleScheduledTask(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		c.JSON(400, gin.H{"error": "enabled requis"})
		return
	}
	val := 0
	if *req.Enabled { val = 1 }
	_, err := db.DB.Exec("UPDATE scheduled_tasks SET enabled = ? WHERE id = ?", val, id)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

// DELETE /api/scheduled-tasks/:id
func handleDeleteScheduledTask(c *gin.Context) {
	id := c.Param("id")
	db.DB.Exec("DELETE FROM scheduled_tasks WHERE id = ?", id)
	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "scheduled_task_delete", id, "")
	c.JSON(200, gin.H{"status": "ok"})
}

// CheckScheduledTasks is called by background worker every 60s.
func CheckScheduledTasks() {
	now := time.Now()
	rows, err := db.DB.Query("SELECT id, project, container_name, task_type, custom_cron, command FROM scheduled_tasks WHERE enabled = 1")
	if err != nil { return }
	defer rows.Close()

	for rows.Next() {
		var id int
		var project, containerName, taskType, cronExpr, command string
		if rows.Scan(&id, &project, &containerName, &taskType, &cronExpr, &command) != nil { continue }

		if !CronMatches(cronExpr, now) { continue }

		// Check if already run this minute
		minuteKey := now.Format("2006-01-02 15:04")
		var lastRun string
		db.DB.QueryRow("SELECT last_run FROM scheduled_tasks WHERE id = ?", id).Scan(&lastRun)
		if strings.HasPrefix(lastRun, minuteKey) { continue }

		// Execute task
		go executeScheduledTask(id, project, containerName, taskType, command)
	}
}

func executeScheduledTask(id int, project, containerName, taskType, command string) {
	now := time.Now().Format("2006-01-02 15:04:05")
	db.DB.Exec("UPDATE scheduled_tasks SET last_run = ? WHERE id = ?", now, id)

	cli := services.EnsureDocker()
	if cli == nil { return }

	// Find container by name
	fullName := containerName
	if !strings.Contains(containerName, project) {
		fullName = project + "-" + containerName
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var err error
	switch taskType {
	case "restart":
		err = cli.ContainerRestart(ctx, fullName, container.StopOptions{})

	case "backup_db":
		// Detect DB type from container image
		info, ierr := cli.ContainerInspect(ctx, fullName)
		if ierr != nil { return }
		image := strings.ToLower(info.Config.Image)
		var cmd []string
		if strings.Contains(image, "mariadb") || strings.Contains(image, "mysql") {
			cmd = []string{"sh", "-c", "mysqldump -u root --all-databases > /tmp/backup_$(date +%Y%m%d).sql"}
		} else if strings.Contains(image, "postgres") {
			cmd = []string{"sh", "-c", "pg_dumpall -U postgres > /tmp/backup_$(date +%Y%m%d).sql"}
		} else {
			return
		}
		execCmd(ctx, fullName, cmd)

	case "cleanup_logs":
		execCmd(ctx, fullName, []string{"sh", "-c", "find /var/log -type f -name '*.log' -mtime +7 -delete 2>/dev/null; echo done"})

	case "custom_command":
		if command != "" {
			execCmd(ctx, fullName, []string{"sh", "-c", command})
		}
	}

	if err != nil {
		fmt.Printf("[TASK] Erreur tache %d (%s): %v\n", id, taskType, err)
	}
}

func execCmd(ctx context.Context, container string, cmd []string) {
	out, err := exec.CommandContext(ctx, "docker", append([]string{"exec", container}, cmd...)...).CombinedOutput()
	if err != nil {
		fmt.Printf("[TASK] exec %s: %v\n%s\n", container, err, string(out))
	}
}

// CronMatches checks if a cron expression matches the given time (exported for reuse).
func CronMatches(cronExpr string, t time.Time) bool {
	fields := strings.Fields(cronExpr)
	if len(fields) != 5 { return false }
	return fieldMatches(fields[0], t.Minute()) &&
		fieldMatches(fields[1], t.Hour()) &&
		fieldMatches(fields[2], t.Day()) &&
		fieldMatches(fields[3], int(t.Month())) &&
		fieldMatches(fields[4], int(t.Weekday()))
}

func fieldMatches(field string, value int) bool {
	if field == "*" { return true }
	// Handle */N
	if strings.HasPrefix(field, "*/") {
		n, err := strconv.Atoi(field[2:])
		if err != nil || n <= 0 { return false }
		return value%n == 0
	}
	// Handle comma-separated
	for _, part := range strings.Split(field, ",") {
		// Handle ranges
		if strings.Contains(part, "-") {
			bounds := strings.SplitN(part, "-", 2)
			lo, _ := strconv.Atoi(bounds[0])
			hi, _ := strconv.Atoi(bounds[1])
			if value >= lo && value <= hi { return true }
		} else {
			n, err := strconv.Atoi(part)
			if err == nil && n == value { return true }
		}
	}
	return false
}
