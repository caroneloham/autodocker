package workers

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"autodocker-go/db"
	"autodocker-go/handlers"
	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// lastCollected tracks the last collection time per container ID
var lastCollected = make(map[string]time.Time)

// severityPatterns for detecting log severity
var severityPatterns = []struct {
	re    *regexp.Regexp
	level string
}{
	{regexp.MustCompile(`(?i)\b(ERROR|FATAL|CRITICAL)\b`), "error"},
	{regexp.MustCompile(`(?i)\b(WARN|WARNING)\b`), "warning"},
	{regexp.MustCompile(`(?i)\b(DEBUG|TRACE)\b`), "debug"},
}

// ansiRE strips ANSI escape codes
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// detectSeverity returns the severity level for a log line.
func detectSeverity(line string) string {
	for _, sp := range severityPatterns {
		if sp.re.MatchString(line) {
			return sp.level
		}
	}
	return "info"
}

// StartBackgroundWorkers launches all background goroutines.
func StartBackgroundWorkers(database *sql.DB, dockerClient *client.Client) {
	fmt.Println("[WORKERS] Demarrage des workers d'arriere-plan...")

	// Collect container logs every 30 seconds
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			collectContainerLogs()
		}
	}()

	// Cleanup old metrics every 1 hour
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			cleanupOldMetrics()
		}
	}()

	// Cleanup old logs every 1 hour
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			cleanupOldLogs()
		}
	}()

	// Check backup schedules every 60 seconds
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			checkBackupSchedules()
		}
	}()

	// Check scheduled tasks every 60 seconds (F2: cron jobs)
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			handlers.CheckScheduledTasks()
		}
	}()

	// Update anomaly baselines every hour (F5: anomaly detection)
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			handlers.UpdateBaselines()
		}
	}()

	fmt.Println("[WORKERS] Tous les workers sont actifs")
}

// collectContainerLogs fetches logs from all running containers,
// detects severity, strips ANSI codes, and stores in container_logs table.
func collectContainerLogs() {
	cli := services.EnsureDocker()
	if cli == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	containers, err := cli.ContainerList(ctx, container.ListOptions{All: false})
	if err != nil {
		return
	}

	now := time.Now().UTC()

	type logRow struct {
		timestamp     string
		containerID   string
		containerName string
		project       string
		severity      string
		message       string
	}
	var rows []logRow

	for _, ctr := range containers {
		cid := ctr.ID
		if len(cid) > 12 {
			cid = cid[:12]
		}

		cname := ""
		if len(ctr.Names) > 0 {
			cname = strings.TrimPrefix(ctr.Names[0], "/")
		}

		project := ""
		if ctr.Labels != nil {
			project = ctr.Labels["com.docker.compose.project"]
		}

		sinceDt := now.Add(-60 * time.Second)
		if last, ok := lastCollected[cid]; ok {
			sinceDt = last
		}

		logReader, err := cli.ContainerLogs(ctx, ctr.ID, container.LogsOptions{
			ShowStdout: true,
			ShowStderr: true,
			Since:      sinceDt.Format(time.RFC3339),
			Timestamps: true,
		})
		if err != nil {
			continue
		}

		data, err := io.ReadAll(logReader)
		logReader.Close()
		if err != nil || len(data) == 0 {
			lastCollected[cid] = now
			continue
		}

		// Strip Docker multiplexed stream headers
		raw := stripDockerHeaders(data)
		if strings.TrimSpace(raw) == "" {
			lastCollected[cid] = now
			continue
		}

		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			// Expected format: "2024-01-01T12:00:00.000000000Z message..."
			parts := strings.SplitN(line, " ", 2)
			var tsStr, msg string
			if len(parts) == 2 {
				tsStr = parts[0]
				msg = parts[1]
			} else {
				tsStr = now.Format("2006-01-02 15:04:05")
				msg = line
			}

			// Normalize timestamp
			tsClean := tsStr
			if len(tsClean) > 26 {
				tsClean = tsClean[:26]
			}
			tsClean = strings.TrimRight(tsClean, "Z")
			tsClean = strings.Replace(tsClean, "T", " ", 1)

			// Strip ANSI codes
			msg = ansiRE.ReplaceAllString(msg, "")

			severity := detectSeverity(msg)
			rows = append(rows, logRow{
				timestamp:     tsClean,
				containerID:   cid,
				containerName: cname,
				project:       project,
				severity:      severity,
				message:       msg,
			})
		}

		lastCollected[cid] = now
	}

	if len(rows) == 0 {
		return
	}

	tx, err := db.DB.Begin()
	if err != nil {
		return
	}

	stmt, err := tx.Prepare(
		"INSERT INTO container_logs (timestamp, container_id, container_name, project, severity, message) VALUES (?, ?, ?, ?, ?, ?)",
	)
	if err != nil {
		tx.Rollback()
		return
	}
	defer stmt.Close()

	for _, r := range rows {
		stmt.Exec(r.timestamp, r.containerID, r.containerName, r.project, r.severity, r.message)
	}
	tx.Commit()
}

// stripDockerHeaders removes the 8-byte Docker log stream header from each frame.
func stripDockerHeaders(data []byte) string {
	var result strings.Builder
	for len(data) > 0 {
		if len(data) < 8 {
			result.Write(data)
			break
		}
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

// cleanupOldMetrics deletes metrics older than the configured retention period (default 7 days).
func cleanupOldMetrics() {
	days := handlers.GetRetentionDays()
	handlers.CleanupOldMetrics(days)
	fmt.Printf("[WORKERS] Nettoyage metriques > %d jours effectue\n", days)
}

// cleanupOldLogs deletes logs older than 3 days.
func cleanupOldLogs() {
	cutoff := time.Now().UTC().Add(-3 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	_, _ = db.DB.Exec("DELETE FROM container_logs WHERE timestamp < ?", cutoff)
	fmt.Println("[WORKERS] Nettoyage logs > 3 jours effectue")
}

// checkBackupSchedules checks cron expressions and triggers backups.
// This is a placeholder for Phase 6 implementation.
func checkBackupSchedules() {
	// Phase 6: Implement backup scheduling based on cron expressions
	// in the backup_schedules table. For now, this is a no-op.
}

