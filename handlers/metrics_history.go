package handlers

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"autodocker-go/db"
	"autodocker-go/middleware"

	"github.com/gin-gonic/gin"
)

// rangeMap maps accepted range parameter values to seconds.
var rangeMap = map[string]int64{
	"1h":  3600,
	"6h":  21600,
	"24h": 86400,
	"7d":  604800,
	"30d": 2592000,
}

// RegisterMetricsRoutes registers metrics history API routes.
func RegisterMetricsRoutes(api *gin.RouterGroup) {
	metrics := api.Group("/metrics")
	{
		metrics.GET("/history", handleMetricsHistory)
		metrics.GET("/export", handleMetricsExport)
		metrics.GET("/retention", handleMetricsRetentionGet)
		metrics.POST("/retention", middleware.AdminRequired(), handleMetricsRetentionSet)
	}
}

// StoreMetrics inserts container and agent metrics into the metrics_history table.
// Schema: timestamp(int), source_type, source_id, source_name, project, cpu, mem, mem_mb, net_rx, net_tx
func StoreMetrics(containersData []map[string]interface{}, agentsData []map[string]interface{}) {
	now := time.Now().Unix()

	tx, err := db.DB.Begin()
	if err != nil {
		return
	}

	stmt, err := tx.Prepare(
		"INSERT INTO metrics_history (timestamp, source_type, source_id, source_name, project, cpu, mem, mem_mb, net_rx, net_tx) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
	)
	if err != nil {
		tx.Rollback()
		return
	}
	defer stmt.Close()

	for _, c := range containersData {
		id, _ := c["id"].(string)
		name, _ := c["name"].(string)
		project, _ := c["project"].(string)
		cpu, _ := toFloat64(c["cpu"])
		if cpu == 0 {
			cpu, _ = toFloat64(c["cpu_percent"])
		}
		mem, _ := toFloat64(c["mem"])
		memMB, _ := toFloat64(c["mem_mb"])
		netRx, _ := toFloat64(c["net_rx"])
		netTx, _ := toFloat64(c["net_tx"])
		stmt.Exec(now, "container", id, name, project, cpu, mem, memMB, netRx, netTx)
	}

	// For agents
	for _, a := range agentsData {
		id, _ := a["agent_id"].(string)
		if id == "" {
			continue
		}
		name, _ := a["hostname"].(string)
		cpu, _ := toFloat64(a["cpu"])
		memMB, _ := toFloat64(a["mem_mb"])
		mem, _ := toFloat64(a["mem"])
		netRx, _ := toFloat64(a["net_rx"])
		netTx, _ := toFloat64(a["net_tx"])
		stmt.Exec(now, "agent", id, name, "", cpu, mem, memMB, netRx, netTx)
	}

	tx.Commit()
}

// CleanupOldMetrics deletes metrics older than the given number of days.
func CleanupOldMetrics(retentionDays int) {
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
	_, _ = db.DB.Exec("DELETE FROM metrics_history WHERE timestamp < ?", cutoff)
}

func toFloat64(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case string:
		f, err := strconv.ParseFloat(val, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func toInt64(v interface{}) (int64, bool) {
	switch val := v.(type) {
	case int64:
		return val, true
	case int:
		return int64(val), true
	case float64:
		return int64(val), true
	case float32:
		return int64(val), true
	case string:
		i, err := strconv.ParseInt(val, 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

// metricsPoint represents an aggregated metrics data point.
type metricsPoint struct {
	Timestamp string  `json:"timestamp"`
	CPU       float64 `json:"cpu"`
	Mem       float64 `json:"mem"`
	MemMB     float64 `json:"mem_mb"`
	NetRx     float64 `json:"net_rx"`
	NetTx     float64 `json:"net_tx"`
}

// aggregateRows reduces the number of data points when there are too many.
func aggregateRows(rows []metricsPoint, maxPoints int) []metricsPoint {
	if len(rows) <= maxPoints {
		return rows
	}

	bucketCount := maxPoints
	bucketSize := len(rows) / bucketCount
	if bucketSize < 1 {
		bucketSize = 1
	}

	result := make([]metricsPoint, 0, bucketCount)
	for i := 0; i < len(rows); i += bucketSize {
		end := i + bucketSize
		if end > len(rows) {
			end = len(rows)
		}
		bucket := rows[i:end]
		n := float64(len(bucket))
		var cpuSum, memSum, memMBSum, netRxSum, netTxSum float64
		for _, b := range bucket {
			cpuSum += b.CPU
			memSum += b.Mem
			memMBSum += b.MemMB
			netRxSum += b.NetRx
			netTxSum += b.NetTx
		}
		midIdx := len(bucket) / 2
		result = append(result, metricsPoint{
			Timestamp: bucket[midIdx].Timestamp,
			CPU:       math.Round(cpuSum/n*100) / 100,
			Mem:       math.Round(memSum/n*100) / 100,
			MemMB:     math.Round(memMBSum/n*100) / 100,
			NetRx:     math.Round(netRxSum/n*100) / 100,
			NetTx:     math.Round(netTxSum/n*100) / 100,
		})
	}
	return result
}

// ---- GET /api/metrics/history ----

func handleMetricsHistory(c *gin.Context) {
	sourceID := c.Query("source_id")
	if sourceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Parametre source_id requis"})
		return
	}

	timeRange := c.DefaultQuery("range", "1h")
	rangeSeconds, ok := rangeMap[timeRange]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": "error",
			"error":  "Plage invalide. Valeurs acceptees : 1h, 6h, 24h, 7d, 30d",
		})
		return
	}

	since := time.Now().Add(-time.Duration(rangeSeconds) * time.Second).Unix()

	rows, err := db.DB.Query(
		"SELECT timestamp, cpu, mem, mem_mb, net_rx, net_tx "+
			"FROM metrics_history "+
			"WHERE source_id = ? AND timestamp > ? "+
			"ORDER BY timestamp ASC",
		sourceID, since,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	defer rows.Close()

	data := make([]metricsPoint, 0)
	for rows.Next() {
		var tsInt int64
		var cpu, mem, memMB, netRx, netTx float64
		if err := rows.Scan(&tsInt, &cpu, &mem, &memMB, &netRx, &netTx); err != nil {
			continue
		}
		ts := time.Unix(tsInt, 0).Format("2006-01-02 15:04:05")
		data = append(data, metricsPoint{
			Timestamp: ts,
			CPU:       cpu,
			Mem:       mem,
			MemMB:     memMB,
			NetRx:     netRx,
			NetTx:     netTx,
		})
	}

	data = aggregateRows(data, 500)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "data": data})
}

// ---- GET /api/metrics/export ----

func handleMetricsExport(c *gin.Context) {
	sourceID := c.Query("source_id")
	if sourceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Parametre source_id requis"})
		return
	}

	format := c.DefaultQuery("format", "csv")
	if format != "csv" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Seul le format csv est supporte"})
		return
	}

	timeRange := c.DefaultQuery("range", "24h")
	rangeSeconds, ok := rangeMap[timeRange]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Plage invalide"})
		return
	}

	since := time.Now().Add(-time.Duration(rangeSeconds) * time.Second).Unix()

	rows, err := db.DB.Query(
		"SELECT timestamp, cpu, mem, mem_mb, net_rx, net_tx "+
			"FROM metrics_history "+
			"WHERE source_id = ? AND timestamp > ? "+
			"ORDER BY timestamp ASC",
		sourceID, since,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	defer rows.Close()

	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	writer.Write([]string{"timestamp", "cpu", "mem", "mem_mb", "net_rx", "net_tx"})

	for rows.Next() {
		var tsInt int64
		var cpu, mem, memMB, netRx, netTx float64
		if err := rows.Scan(&tsInt, &cpu, &mem, &memMB, &netRx, &netTx); err != nil {
			continue
		}
		ts := time.Unix(tsInt, 0).Format("2006-01-02 15:04:05")
		writer.Write([]string{
			ts,
			fmt.Sprintf("%.2f", cpu),
			fmt.Sprintf("%.2f", mem),
			fmt.Sprintf("%.2f", memMB),
			fmt.Sprintf("%.2f", netRx),
			fmt.Sprintf("%.2f", netTx),
		})
	}
	writer.Flush()

	filename := fmt.Sprintf("metrics_%s_%s.csv", sourceID, timeRange)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	c.Data(http.StatusOK, "text/csv", buf.Bytes())
}

// ---- GET /api/metrics/retention ----

func handleMetricsRetentionGet(c *gin.Context) {
	days := GetRetentionDays()
	c.JSON(http.StatusOK, gin.H{"status": "ok", "retention_days": days})
}

// ---- POST /api/metrics/retention ----

func handleMetricsRetentionSet(c *gin.Context) {
	var data struct {
		RetentionDays *int `json:"retention_days"`
	}
	if err := c.ShouldBindJSON(&data); err != nil || data.RetentionDays == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Champ retention_days requis"})
		return
	}

	days := *data.RetentionDays
	if days < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "retention_days doit etre un entier >= 1"})
		return
	}

	setRetentionDays(days)

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "metrics_retention", "", fmt.Sprintf("Retention modifiee a %d jours", days))

	c.JSON(http.StatusOK, gin.H{"status": "ok", "retention_days": days})
}

// ---- Retention config (in-memory, default 7 days) ----

var metricsRetentionDays = 7

// GetRetentionDays returns the configured metrics retention in days.
func GetRetentionDays() int {
	return metricsRetentionDays
}

func setRetentionDays(days int) {
	metricsRetentionDays = days
}
