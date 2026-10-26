package handlers

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"autodocker-go/db"
	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"
	"github.com/gin-gonic/gin"
)

// lastKnownStates tracks the last observed state of each container
// Key = container_id, value = last state (running, exited, etc.)
var (
	lastKnownStates   = make(map[string]string)
	lastKnownStatesMu sync.Mutex
)

// RegisterHealthRoutes registers health dashboard API routes.
func RegisterHealthRoutes(api *gin.RouterGroup) {
	health := api.Group("/health")
	{
		health.GET("/dashboard", handleHealthDashboard)
		health.GET("/container/:cid", handleHealthContainer)
	}
}

// RecordStateChanges compares current container states with previous ones
// and inserts transitions into the uptime_events table.
// Called from background workers or supervision hooks.
func RecordStateChanges(containersData []map[string]string) {
	lastKnownStatesMu.Lock()
	defer lastKnownStatesMu.Unlock()

	now := time.Now().Format("2006-01-02 15:04:05")

	type event struct {
		containerID string
		project     string
		status      string
		timestamp   string
	}
	var events []event

	for _, c := range containersData {
		cid := c["id"]
		state := c["state"]
		project := c["project"]

		prev, exists := lastKnownStates[cid]
		if !exists || prev != state {
			events = append(events, event{
				containerID: cid,
				project:     project,
				status:      state,
				timestamp:   now,
			})
			lastKnownStates[cid] = state
		}
	}

	if len(events) == 0 {
		return
	}

	tx, err := db.DB.Begin()
	if err != nil {
		return
	}
	stmt, err := tx.Prepare("INSERT INTO uptime_events (timestamp, container_id, project, status) VALUES (?, ?, ?, ?)")
	if err != nil {
		tx.Rollback()
		return
	}
	defer stmt.Close()

	for _, e := range events {
		stmt.Exec(e.timestamp, e.containerID, e.project, e.status)
	}
	tx.Commit()
}

// computeHealthColor determines the health color for a set of containers.
// GREEN: all running, cpu < 80%, mem < 85%.
// ORANGE: at least one restarting or high metrics.
// RED: at least one exited/dead.
func computeHealthColor(containers []healthContainerInfo) string {
	if len(containers) == 0 {
		return "green"
	}

	hasRed := false
	hasOrange := false

	for _, c := range containers {
		state := strings.ToLower(c.State)
		if state == "exited" || state == "dead" {
			hasRed = true
		} else if state == "restarting" {
			hasOrange = true
		}
		if c.CPU >= 80 || c.Mem >= 85 {
			hasOrange = true
		}
	}

	if hasRed {
		return "red"
	}
	if hasOrange {
		return "orange"
	}
	return "green"
}

// uptimePct calculates the percentage of time a container was running over the given number of days.
func uptimePct(containerID string, days int) float64 {
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Format("2006-01-02 15:04:05")

	rows, err := db.DB.Query(
		"SELECT status, timestamp FROM uptime_events WHERE container_id = ? AND timestamp >= ? ORDER BY timestamp ASC",
		containerID, cutoff,
	)
	if err != nil {
		return 100.0
	}
	defer rows.Close()

	type stateEvent struct {
		state string
		ts    time.Time
	}
	var events []stateEvent

	for rows.Next() {
		var status, tsStr string
		if err := rows.Scan(&status, &tsStr); err != nil {
			continue
		}
		t, err := time.Parse("2006-01-02 15:04:05", tsStr)
		if err != nil {
			continue
		}
		events = append(events, stateEvent{state: status, ts: t})
	}

	if len(events) == 0 {
		return 100.0
	}

	totalSeconds := float64(days * 86400)
	runningSeconds := 0.0

	prevState := events[0].state
	prevTs := events[0].ts

	for _, e := range events[1:] {
		if prevState == "running" {
			runningSeconds += e.ts.Sub(prevTs).Seconds()
		}
		prevState = e.state
		prevTs = e.ts
	}

	// Add time from last event to now
	if prevState == "running" {
		runningSeconds += time.Since(prevTs).Seconds()
	}

	pct := (runningSeconds / totalSeconds) * 100
	if pct > 100 {
		pct = 100
	}
	return math.Round(pct*100) / 100
}

// lastIncident returns the timestamp of the last non-running event for a project.
func lastIncident(project string) string {
	var ts sql.NullString
	err := db.DB.QueryRow(
		"SELECT timestamp FROM uptime_events WHERE project = ? AND status != 'running' ORDER BY timestamp DESC LIMIT 1",
		project,
	).Scan(&ts)
	if err != nil || !ts.Valid {
		return ""
	}
	return ts.String
}

// healthContainerInfo holds container data for health calculations
type healthContainerInfo struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	State   string  `json:"state"`
	Project string  `json:"project"`
	CPU     float64 `json:"cpu"`
	Mem     float64 `json:"mem"`
	MemMB   float64 `json:"mem_mb"`
	NetRx   float64 `json:"net_rx"`
	NetTx   float64 `json:"net_tx"`
}

// getProjectContainers returns containers for a specific project with their stats.
func getProjectContainers(project string) []healthContainerInfo {
	cli := services.EnsureDocker()
	if cli == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	allContainers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil
	}

	result := make([]healthContainerInfo, 0)

	for _, ctr := range allContainers {
		// Filter by project label
		if ctr.Labels == nil || !strings.EqualFold(ctr.Labels["com.docker.compose.project"], project) {
			continue
		}

		info := healthContainerInfo{
			State:   ctr.State,
			Project: project,
		}

		if len(ctr.ID) > 12 {
			info.ID = ctr.ID[:12]
		} else {
			info.ID = ctr.ID
		}
		if len(ctr.Names) > 0 {
			info.Name = strings.TrimPrefix(ctr.Names[0], "/")
		}

		// Collect stats for running containers
		if ctr.State == "running" {
			stats, err := services.CollectContainerStats(ctx, cli, ctr.ID)
			if err == nil {
				info.CPU = math.Round(stats.CPU*100) / 100
				if stats.MemLimMB > 0 {
					info.Mem = math.Round((stats.MemMB/stats.MemLimMB)*10000) / 100
				}
				info.MemMB = math.Round(stats.MemMB*100) / 100
				info.NetRx = math.Round(float64(stats.NetRx) / 1048576 * 100) / 100
				info.NetTx = math.Round(float64(stats.NetTx) / 1048576 * 100) / 100
			}
		}

		result = append(result, info)
	}

	return result
}

// listProjects returns the list of projects from the labs table.
func listProjects() []string {
	rows, err := db.DB.Query("SELECT project FROM labs")
	if err != nil {
		return nil
	}
	defer rows.Close()

	var projects []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil {
			projects = append(projects, p)
		}
	}
	return projects
}


// ---- GET /api/health/dashboard ----

func handleHealthDashboard(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "error": "Client Docker non disponible"})
		return
	}

	projects := listProjects()
	result := make([]gin.H, 0, len(projects))

	for _, project := range projects {
		containers := getProjectContainers(project)
		total := len(containers)
		running := 0
		for _, ct := range containers {
			if ct.State == "running" {
				running++
			}
		}

		color := computeHealthColor(containers)

		// Average uptime across containers
		var uptimeSum float64
		for _, ct := range containers {
			uptimeSum += uptimePct(ct.ID, 30)
		}
		uptimeAvg := 100.0
		if len(containers) > 0 {
			uptimeAvg = math.Round(uptimeSum/float64(len(containers))*100) / 100
		}

		incident := lastIncident(project)

		result = append(result, gin.H{
			"project":             project,
			"status":              color,
			"containers_total":    total,
			"containers_running":  running,
			"uptime_pct":          uptimeAvg,
			"last_incident":       incident,
		})
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "data": result})
}

// ---- GET /api/health/container/:cid ----

func handleHealthContainer(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "error": "Client Docker non disponible"})
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

	shortID := ctr.ID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}

	project := ""
	if ctr.Config != nil && ctr.Config.Labels != nil {
		project = ctr.Config.Labels["com.docker.compose.project"]
	}

	uptime7d := uptimePct(shortID, 7)

	// Get timeline of last 7 days
	cutoff := time.Now().Add(-7 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	rows, err := db.DB.Query(
		"SELECT status, timestamp FROM uptime_events WHERE container_id = ? AND timestamp >= ? ORDER BY timestamp ASC",
		shortID, cutoff,
	)

	timeline := make([]gin.H, 0)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var status, ts string
			if rows.Scan(&status, &ts) == nil {
				timeline = append(timeline, gin.H{"state": status, "timestamp": ts})
			}
		}
	}

	// Get current stats
	cpu := 0.0
	mem := 0.0
	memMB := 0.0
	netRx := 0.0
	netTx := 0.0

	if ctr.State.Running {
		stats, err := services.CollectContainerStats(ctx, cli, cid)
		if err == nil {
			cpu = math.Round(stats.CPU*100) / 100
			memMB = math.Round(stats.MemMB*100) / 100
			if stats.MemLimMB > 0 {
				mem = math.Round((stats.MemMB/stats.MemLimMB)*10000) / 100
			}
			netRx = math.Round(float64(stats.NetRx) / 1048576 * 100) / 100
			netTx = math.Round(float64(stats.NetTx) / 1048576 * 100) / 100
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"project": project,
		"health":  computeHealthColor([]healthContainerInfo{{State: ctr.State.Status, CPU: cpu, Mem: mem}}),
		"container": gin.H{
			"id":             shortID,
			"name":           strings.TrimPrefix(ctr.Name, "/"),
			"state":          ctr.State.Status,
			"cpu":            cpu,
			"mem":            mem,
			"mem_mb":         memMB,
			"net_rx":         netRx,
			"net_tx":         netTx,
			"uptime_pct_7d":  uptime7d,
			"timeline":       timeline,
		},
	})
}
