package handlers

import (
	"fmt"
	"math"
	"time"

	"autodocker-go/db"
	"autodocker-go/services"

	"github.com/gin-gonic/gin"
)

// RegisterAnomalyRoutes registers anomaly detection endpoints.
func RegisterAnomalyRoutes(api *gin.RouterGroup) {
	api.GET("/anomalies", handleGetAnomalies)
	api.GET("/anomalies/baselines", handleGetBaselines)
}

// UpdateBaselines calculates rolling AVG + STDDEV for CPU/RAM over last 24h per container.
// Called hourly by background worker.
func UpdateBaselines() {
	since := time.Now().Add(-24 * time.Hour).Unix()

	rows, err := db.DB.Query(
		`SELECT source_id, source_name,
		        AVG(cpu), AVG(mem), COUNT(*)
		 FROM metrics_history
		 WHERE timestamp > ? AND source_type = 'container'
		 GROUP BY source_id`, since)
	if err != nil { return }
	defer rows.Close()

	for rows.Next() {
		var sourceID, sourceName string
		var avgCPU, avgMem float64
		var count int
		if rows.Scan(&sourceID, &sourceName, &avgCPU, &avgMem, &count) != nil { continue }
		if count < 10 { continue } // need enough samples

		// Calculate STDDEV manually (SQLite doesn't have it)
		stddevCPU := calcStddev(sourceID, "cpu", avgCPU, since)
		stddevMem := calcStddev(sourceID, "mem", avgMem, since)

		now := time.Now().Format("2006-01-02 15:04:05")

		// Upsert CPU baseline
		db.DB.Exec(`INSERT INTO metric_baselines (container_id, metric, avg_value, stddev, sample_count, last_updated)
			VALUES (?, 'cpu', ?, ?, ?, ?)
			ON CONFLICT(container_id, metric) DO UPDATE SET avg_value=?, stddev=?, sample_count=?, last_updated=?`,
			sourceID, avgCPU, stddevCPU, count, now, avgCPU, stddevCPU, count, now)

		// Upsert MEM baseline
		db.DB.Exec(`INSERT INTO metric_baselines (container_id, metric, avg_value, stddev, sample_count, last_updated)
			VALUES (?, 'mem', ?, ?, ?, ?)
			ON CONFLICT(container_id, metric) DO UPDATE SET avg_value=?, stddev=?, sample_count=?, last_updated=?`,
			sourceID, avgMem, stddevMem, count, now, avgMem, stddevMem, count, now)
	}
}

func calcStddev(sourceID, metric string, avg float64, since int64) float64 {
	rows, err := db.DB.Query(
		fmt.Sprintf("SELECT %s FROM metrics_history WHERE source_id = ? AND timestamp > ? AND source_type = 'container'", metric),
		sourceID, since)
	if err != nil { return 0 }
	defer rows.Close()

	var sumSq float64
	var n int
	for rows.Next() {
		var val float64
		if rows.Scan(&val) == nil {
			diff := val - avg
			sumSq += diff * diff
			n++
		}
	}
	if n < 2 { return 0 }
	return math.Sqrt(sumSq / float64(n))
}

// GET /api/anomalies — returns current anomalies
func handleGetAnomalies(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(503, gin.H{"error": "Docker non connecte"})
		return
	}

	// Get latest metrics per container
	latestMetrics := getLatestMetrics()

	// Get baselines
	baseRows, err := db.DB.Query("SELECT container_id, metric, avg_value, stddev, sample_count FROM metric_baselines")
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer baseRows.Close()

	type baseline struct {
		Avg    float64
		Stddev float64
		Count  int
	}
	baselines := make(map[string]map[string]baseline) // containerID -> metric -> baseline
	for baseRows.Next() {
		var cid, metric string
		var avg, stddev float64
		var count int
		if baseRows.Scan(&cid, &metric, &avg, &stddev, &count) != nil { continue }
		if baselines[cid] == nil { baselines[cid] = make(map[string]baseline) }
		baselines[cid][metric] = baseline{Avg: avg, Stddev: stddev, Count: count}
	}

	anomalies := make([]gin.H, 0)
	for cid, metrics := range latestMetrics {
		cName := metrics["name"]
		cBaselines := baselines[cid]
		if cBaselines == nil { continue }

		for _, metricName := range []string{"cpu", "mem"} {
			bl, ok := cBaselines[metricName]
			if !ok || bl.Stddev == 0 { continue }

			currentVal := 0.0
			if v, ok := metrics[metricName]; ok {
				if f, ok := v.(float64); ok { currentVal = f }
			}

			zScore := (currentVal - bl.Avg) / bl.Stddev
			if zScore <= 2 { continue } // not anomalous

			severity := "warning"
			if zScore > 3 { severity = "critical" }

			label := "CPU"
			if metricName == "mem" { label = "RAM" }

			anomalies = append(anomalies, gin.H{
				"container_id":   cid,
				"container_name": cName,
				"metric":         metricName,
				"metric_label":   label,
				"current_value":  math.Round(currentVal*100) / 100,
				"baseline_avg":   math.Round(bl.Avg*100) / 100,
				"baseline_stddev": math.Round(bl.Stddev*100) / 100,
				"z_score":        math.Round(zScore*100) / 100,
				"severity":       severity,
			})
		}
	}

	c.JSON(200, gin.H{"status": "ok", "anomalies": anomalies, "count": len(anomalies)})
}

func getLatestMetrics() map[string]map[string]interface{} {
	since := time.Now().Add(-5 * time.Minute).Unix()
	rows, err := db.DB.Query(
		`SELECT source_id, source_name, cpu, mem
		 FROM metrics_history
		 WHERE timestamp > ? AND source_type = 'container'
		 GROUP BY source_id
		 HAVING timestamp = MAX(timestamp)`, since)
	if err != nil { return nil }
	defer rows.Close()

	result := make(map[string]map[string]interface{})
	for rows.Next() {
		var cid, name string
		var cpu, mem float64
		if rows.Scan(&cid, &name, &cpu, &mem) != nil { continue }
		result[cid] = map[string]interface{}{
			"name": name,
			"cpu":  cpu,
			"mem":  mem,
		}
	}
	return result
}

// GET /api/anomalies/baselines
func handleGetBaselines(c *gin.Context) {
	rows, err := db.DB.Query("SELECT container_id, metric, avg_value, stddev, sample_count, last_updated FROM metric_baselines ORDER BY container_id, metric")
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	baselines := make([]gin.H, 0)
	for rows.Next() {
		var cid, metric, lastUpdated string
		var avg, stddev float64
		var count int
		if rows.Scan(&cid, &metric, &avg, &stddev, &count, &lastUpdated) != nil { continue }
		baselines = append(baselines, gin.H{
			"container_id": cid, "metric": metric,
			"avg": math.Round(avg*100) / 100, "stddev": math.Round(stddev*100) / 100,
			"sample_count": count, "last_updated": lastUpdated,
		})
	}
	c.JSON(200, gin.H{"status": "ok", "baselines": baselines})
}
