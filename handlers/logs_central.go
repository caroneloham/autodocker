package handlers

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"autodocker-go/db"

	"github.com/gin-gonic/gin"
)

// RegisterLogsRoutes registers centralized log API routes.
func RegisterLogsRoutes(api *gin.RouterGroup) {
	logs := api.Group("/logs")
	{
		logs.GET("/search", handleLogsSearch)
		logs.GET("/timeline", handleLogsTimeline)
		logs.GET("/containers", handleLogsContainers)
		logs.GET("/correlate", handleLogsCorrelate)
	}
}

// ---- GET /api/logs/search ----

func handleLogsSearch(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	containerFilter := strings.TrimSpace(c.Query("container"))
	severity := strings.ToLower(strings.TrimSpace(c.Query("severity")))
	since := strings.TrimSpace(c.Query("since"))
	until := strings.TrimSpace(c.Query("until"))
	project := strings.TrimSpace(c.Query("project"))

	limit := 200
	if l := c.Query("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 2000 {
		limit = 2000
	}

	offset := 0
	if o := c.Query("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil && v >= 0 {
			offset = v
		}
	}

	// Build WHERE clause
	conditions := make([]string, 0)
	params := make([]interface{}, 0)

	if q != "" {
		conditions = append(conditions, "message LIKE ?")
		params = append(params, "%"+q+"%")
	}
	if containerFilter != "" {
		conditions = append(conditions, "(container_id = ? OR container_name = ?)")
		params = append(params, containerFilter, containerFilter)
	}
	if severity != "" {
		conditions = append(conditions, "severity = ?")
		params = append(params, severity)
	}
	if since != "" {
		conditions = append(conditions, "timestamp >= ?")
		params = append(params, since)
	}
	if until != "" {
		conditions = append(conditions, "timestamp <= ?")
		params = append(params, until)
	}
	if project != "" {
		conditions = append(conditions, "project = ?")
		params = append(params, project)
	}

	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	// Count total
	var total int
	countQuery := "SELECT COUNT(*) FROM container_logs" + where
	if err := db.DB.QueryRow(countQuery, params...).Scan(&total); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	// Fetch rows
	selectQuery := "SELECT id, timestamp, container_id, container_name, project, severity, message FROM container_logs" +
		where + " ORDER BY timestamp DESC LIMIT ? OFFSET ?"
	queryParams := append(params, limit, offset)

	rows, err := db.DB.Query(selectQuery, queryParams...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	defer rows.Close()

	logs := make([]gin.H, 0)
	for rows.Next() {
		var id int
		var ts, cid, cname, proj, sev, msg string
		if err := rows.Scan(&id, &ts, &cid, &cname, &proj, &sev, &msg); err != nil {
			continue
		}
		logs = append(logs, gin.H{
			"id":             id,
			"timestamp":      ts,
			"container_id":   cid,
			"container_name": cname,
			"project":        proj,
			"severity":       sev,
			"message":        msg,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"logs":   logs,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// ---- GET /api/logs/timeline ----

type timelineBucket struct {
	Timestamp string `json:"timestamp"`
	Info      int    `json:"info"`
	Warning   int    `json:"warning"`
	Error     int    `json:"error"`
	Debug     int    `json:"debug"`
}

func handleLogsTimeline(c *gin.Context) {
	rangeParam := strings.ToLower(strings.TrimSpace(c.DefaultQuery("range", "1h")))

	type rangeConfig struct {
		window         time.Duration
		bucketSize     time.Duration
		expectedPoints int
	}

	rangeConfigs := map[string]rangeConfig{
		"1h":  {1 * time.Hour, 5 * time.Minute, 12},
		"6h":  {6 * time.Hour, 30 * time.Minute, 12},
		"24h": {24 * time.Hour, 1 * time.Hour, 24},
		"7d":  {7 * 24 * time.Hour, 6 * time.Hour, 28},
	}

	cfg, ok := rangeConfigs[rangeParam]
	if !ok {
		rangeParam = "1h"
		cfg = rangeConfigs["1h"]
	}

	now := time.Now().UTC()
	start := now.Add(-cfg.window)

	// Fetch logs in range
	rows, err := db.DB.Query(
		"SELECT timestamp, severity FROM container_logs WHERE timestamp >= ? ORDER BY timestamp ASC",
		start.Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	defer rows.Close()

	// Create empty buckets
	buckets := make([]timelineBucket, cfg.expectedPoints)
	bucketStart := start
	for i := 0; i < cfg.expectedPoints; i++ {
		buckets[i] = timelineBucket{
			Timestamp: bucketStart.Format("2006-01-02 15:04:05"),
		}
		bucketStart = bucketStart.Add(cfg.bucketSize)
	}

	// Distribute logs into buckets
	for rows.Next() {
		var tsStr, severity string
		if err := rows.Scan(&tsStr, &severity); err != nil {
			continue
		}
		ts, err := time.Parse("2006-01-02 15:04:05", tsStr[:19])
		if err != nil {
			continue
		}
		idx := int(math.Floor(ts.Sub(start).Seconds() / cfg.bucketSize.Seconds()))
		if idx < 0 || idx >= len(buckets) {
			continue
		}
		switch severity {
		case "info":
			buckets[idx].Info++
		case "warning":
			buckets[idx].Warning++
		case "error":
			buckets[idx].Error++
		case "debug":
			buckets[idx].Debug++
		default:
			buckets[idx].Info++
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "buckets": buckets, "range": rangeParam})
}

// ---- GET /api/logs/containers ----

func handleLogsContainers(c *gin.Context) {
	rows, err := db.DB.Query(
		"SELECT container_name, container_id, COUNT(*) as log_count " +
			"FROM container_logs GROUP BY container_id ORDER BY log_count DESC",
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	defer rows.Close()

	containers := make([]gin.H, 0)
	for rows.Next() {
		var cname, cid string
		var count int
		if err := rows.Scan(&cname, &cid, &count); err != nil {
			continue
		}
		containers = append(containers, gin.H{
			"container_name": cname,
			"container_id":   cid,
			"log_count":      count,
		})
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "containers": containers})
}

// formatTimestamp is unused but kept as a potential utility for parsing
// Docker log timestamps.
func formatTimestamp(ts string) string {
	if len(ts) >= 19 {
		return ts[:19]
	}
	return fmt.Sprintf("%-19s", ts)
}

// ---- GET /api/logs/correlate ----
// Given a container + timestamp, find all logs from OTHER containers in the same project within ±window.

func handleLogsCorrelate(c *gin.Context) {
	container := c.Query("container")
	timestamp := c.Query("timestamp")
	windowStr := c.DefaultQuery("window", "5")

	if container == "" || timestamp == "" {
		c.JSON(400, gin.H{"error": "container et timestamp requis"})
		return
	}

	windowSec, _ := strconv.Atoi(windowStr)
	if windowSec <= 0 { windowSec = 5 }
	if windowSec > 60 { windowSec = 60 }

	// Find the project of this container
	var project string
	err := db.DB.QueryRow("SELECT project FROM container_logs WHERE (container_name = ? OR container_id = ?) AND project != '' LIMIT 1",
		container, container).Scan(&project)
	if err != nil || project == "" {
		c.JSON(404, gin.H{"error": "Projet introuvable pour ce conteneur"})
		return
	}

	// Parse timestamp and compute window
	ts, err := time.Parse("2006-01-02 15:04:05", timestamp)
	if err != nil {
		// Try other formats
		ts, err = time.Parse("2006-01-02T15:04:05", timestamp)
		if err != nil {
			c.JSON(400, gin.H{"error": "Format timestamp invalide (attendu: 2006-01-02 15:04:05)"})
			return
		}
	}
	before := ts.Add(-time.Duration(windowSec) * time.Second).Format("2006-01-02 15:04:05")
	after := ts.Add(time.Duration(windowSec) * time.Second).Format("2006-01-02 15:04:05")

	// Get source log
	var srcSeverity, srcMessage string
	db.DB.QueryRow("SELECT severity, message FROM container_logs WHERE (container_name = ? OR container_id = ?) AND timestamp = ? LIMIT 1",
		container, container, timestamp).Scan(&srcSeverity, &srcMessage)

	// Get correlated logs from OTHER containers
	rows, err := db.DB.Query(
		`SELECT container_name, container_id, timestamp, severity, message
		 FROM container_logs
		 WHERE project = ? AND container_name != ? AND container_id != ?
		 AND timestamp BETWEEN ? AND ?
		 ORDER BY timestamp ASC
		 LIMIT 500`,
		project, container, container, before, after,
	)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	// Group by container
	grouped := make(map[string][]gin.H)
	for rows.Next() {
		var cname, cid, ts, sev, msg string
		if rows.Scan(&cname, &cid, &ts, &sev, &msg) != nil { continue }
		grouped[cname] = append(grouped[cname], gin.H{
			"timestamp": ts,
			"severity":  sev,
			"message":   msg,
		})
	}

	correlated := make([]gin.H, 0)
	for cname, logs := range grouped {
		correlated = append(correlated, gin.H{
			"container_name": cname,
			"log_count":      len(logs),
			"logs":           logs,
		})
	}

	c.JSON(200, gin.H{
		"status":  "ok",
		"source":  gin.H{"container": container, "timestamp": timestamp, "severity": srcSeverity, "message": srcMessage},
		"project": project,
		"window":  windowSec,
		"correlated": correlated,
	})
}
