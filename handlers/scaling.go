package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"autodocker-go/config"
	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// ScalingRule represents an auto-scaling rule.
type ScalingRule struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Project          string  `json:"project"`
	Service          string  `json:"service"`
	Metric           string  `json:"metric"`
	Operator         string  `json:"operator"`
	Threshold        float64 `json:"threshold"`
	DurationSeconds  int     `json:"duration_seconds"`
	Direction        string  `json:"direction"`
	ScaleStep        int     `json:"scale_step"`
	MinReplicas      int     `json:"min_replicas"`
	MaxReplicas      int     `json:"max_replicas"`
	CooldownSeconds  int     `json:"cooldown_seconds"`
	Enabled          bool    `json:"enabled"`
	LastTriggered    string  `json:"last_triggered,omitempty"`
}

var (
	scalingRules   []ScalingRule
	scalingRulesMu sync.RWMutex
)

func scalingRulesFile() string {
	return filepath.Join(config.RootDir, "scaling_rules.json")
}

func loadScalingRules() []ScalingRule {
	scalingRulesMu.RLock()
	defer scalingRulesMu.RUnlock()

	data, err := os.ReadFile(scalingRulesFile())
	if err != nil {
		return nil
	}
	var rules []ScalingRule
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil
	}
	return rules
}

func saveScalingRules(rules []ScalingRule) {
	scalingRulesMu.Lock()
	defer scalingRulesMu.Unlock()

	data, _ := json.MarshalIndent(rules, "", "  ")
	os.WriteFile(scalingRulesFile(), data, 0644)
}

func findComposePath(project string) string {
	candidates := []string{
		filepath.Join(config.ProjectsDir, project, "compose.yaml"),
		filepath.Join(config.ProjectsDir, project, "compose.yml"),
		filepath.Join(config.ProjectsDir, project, "docker-compose.yaml"),
		filepath.Join(config.ProjectsDir, project, "docker-compose.yml"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// RegisterScalingRoutes registers auto-scaling API routes.
func RegisterScalingRoutes(api *gin.RouterGroup) {
	g := api.Group("/scaling")
	{
		g.GET("/rules", handleListScalingRules)
		g.POST("/rules", middleware.AdminRequired(), handleCreateScalingRule)
		g.DELETE("/rules/:id", middleware.AdminRequired(), handleDeleteScalingRule)
		g.GET("/status", handleScalingStatus)
		g.POST("/evaluate", middleware.AdminRequired(), handleManualEvaluate)
	}
}

// ---------------------------------------------------------------------------
// Container metrics helpers (used by evaluate)
// ---------------------------------------------------------------------------

type containerMetric struct {
	ID      string
	Name    string
	Project string
	Service string
	Status  string
	CPU     float64
	Mem     float64
	MemMB   float64
}

func collectContainerMetrics() ([]containerMetric, error) {
	cli := services.EnsureDocker()
	if cli == nil {
		return nil, fmt.Errorf("Docker non connecte")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	containers, err := cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, err
	}

	var metrics []containerMetric
	for _, ctr := range containers {
		labels := ctr.Labels
		project := labels["com.docker.compose.project"]
		service := labels["com.docker.compose.service"]

		name := ""
		if len(ctr.Names) > 0 {
			name = strings.TrimPrefix(ctr.Names[0], "/")
		}

		// Get stats
		statsResp, err := cli.ContainerStatsOneShot(ctx, ctr.ID)
		if err != nil {
			continue
		}
		var stats container.StatsResponse
		dec := json.NewDecoder(statsResp.Body)
		dec.Decode(&stats)
		statsResp.Body.Close()

		cpuPercent := 0.0
		cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage - stats.PreCPUStats.CPUUsage.TotalUsage)
		systemDelta := float64(stats.CPUStats.SystemUsage - stats.PreCPUStats.SystemUsage)
		numCPUs := float64(stats.CPUStats.OnlineCPUs)
		if numCPUs == 0 {
			numCPUs = 1
		}
		if systemDelta > 0 && cpuDelta > 0 {
			cpuPercent = (cpuDelta / systemDelta) * numCPUs * 100.0
		}

		memUsage := float64(stats.MemoryStats.Usage)
		memLimit := float64(stats.MemoryStats.Limit)
		memPercent := 0.0
		if memLimit > 0 {
			memPercent = (memUsage / memLimit) * 100.0
		}
		memMB := memUsage / (1024 * 1024)

		metrics = append(metrics, containerMetric{
			ID:      ctr.ID[:12],
			Name:    name,
			Project: project,
			Service: service,
			Status:  ctr.State,
			CPU:     cpuPercent,
			Mem:     memPercent,
			MemMB:   memMB,
		})
	}
	return metrics, nil
}

func getMetricValue(metrics []containerMetric, project, service, metric string) (float64, bool) {
	var values []float64
	for _, m := range metrics {
		if m.Project == project && strings.Contains(m.Name, service) {
			switch metric {
			case "cpu_percent":
				values = append(values, m.CPU)
			case "mem_percent":
				values = append(values, m.Mem)
			case "mem_mb":
				values = append(values, m.MemMB)
			}
		}
	}
	if len(values) == 0 {
		return 0, false
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values)), true
}

func countServiceReplicas(metrics []containerMetric, project, service string) int {
	count := 0
	for _, m := range metrics {
		if m.Project == project && strings.Contains(m.Name, service) && m.Status == "running" {
			count++
		}
	}
	if count == 0 {
		return 1
	}
	return count
}

// EvaluateScalingRules evaluates all active scaling rules and triggers actions.
func EvaluateScalingRules(metrics []containerMetric) []gin.H {
	rules := loadScalingRules()
	var triggered []gin.H
	rulesModified := false
	now := time.Now()

	for i := range rules {
		rule := &rules[i]
		if !rule.Enabled {
			continue
		}

		currentValue, ok := getMetricValue(metrics, rule.Project, rule.Service, rule.Metric)
		if !ok {
			continue
		}

		thresholdExceeded := false
		if rule.Operator == ">" && currentValue > rule.Threshold {
			thresholdExceeded = true
		} else if rule.Operator == "<" && currentValue < rule.Threshold {
			thresholdExceeded = true
		}
		if !thresholdExceeded {
			continue
		}

		// Cooldown check
		if rule.LastTriggered != "" {
			if lastTS, err := time.Parse("2006-01-02 15:04:05", rule.LastTriggered); err == nil {
				if now.Sub(lastTS).Seconds() < float64(rule.CooldownSeconds) {
					continue
				}
			}
		}

		currentReplicas := countServiceReplicas(metrics, rule.Project, rule.Service)
		var newReplicas int
		if rule.Direction == "up" {
			newReplicas = currentReplicas + rule.ScaleStep
		} else {
			newReplicas = currentReplicas - rule.ScaleStep
		}

		// Clamp
		if newReplicas < rule.MinReplicas {
			newReplicas = rule.MinReplicas
		}
		if newReplicas > rule.MaxReplicas {
			newReplicas = rule.MaxReplicas
		}
		if newReplicas == currentReplicas {
			continue
		}

		composePath := findComposePath(rule.Project)
		if composePath == "" {
			continue
		}

		// Execute scaling
		cmd := exec.Command("docker", "compose", "-f", composePath,
			"up", "-d", "--scale", fmt.Sprintf("%s=%d", rule.Service, newReplicas), "--no-recreate")
		output, err := cmd.CombinedOutput()
		if err != nil {
			db.LogActivity("auto-scaler", "scaling_error", rule.Project,
				fmt.Sprintf("Erreur scaling %s: %s", rule.Service, string(output)))
			continue
		}

		nowStr := now.Format("2006-01-02 15:04:05")
		rule.LastTriggered = nowStr
		rulesModified = true

		// Log event
		logScalingEvent(rule.Project, rule.Service, rule.ID, rule.Direction,
			currentReplicas, newReplicas, currentValue, rule.Threshold)

		db.LogActivity("auto-scaler", "scaling_"+rule.Direction, rule.Project,
			fmt.Sprintf("Service %s: %d -> %d replicas (%s=%.1f, seuil=%.1f)",
				rule.Service, currentReplicas, newReplicas, rule.Metric, currentValue, rule.Threshold))

		triggered = append(triggered, gin.H{
			"rule_id":      rule.ID,
			"project":      rule.Project,
			"service":      rule.Service,
			"direction":    rule.Direction,
			"old_replicas": currentReplicas,
			"new_replicas": newReplicas,
			"metric":       rule.Metric,
			"metric_value": currentValue,
			"threshold":    rule.Threshold,
		})
	}

	if rulesModified {
		saveScalingRules(rules)
	}
	return triggered
}

func logScalingEvent(project, service, ruleID, direction string, oldReplicas, newReplicas int, metricValue, threshold float64) {
	nowStr := time.Now().Format("2006-01-02 15:04:05")
	details := fmt.Sprintf("Scaling %s: %d -> %d replicas (metrique=%.1f, seuil=%.1f)",
		direction, oldReplicas, newReplicas, metricValue, threshold)
	db.DB.Exec(
		"INSERT INTO scaling_events (timestamp, project, service, rule_id, direction, old_replicas, new_replicas, metric_value, threshold, details) VALUES (?,?,?,?,?,?,?,?,?,?)",
		nowStr, project, service, ruleID, direction, oldReplicas, newReplicas, metricValue, threshold, details,
	)
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

func handleListScalingRules(c *gin.Context) {
	rules := loadScalingRules()
	if rules == nil {
		rules = []ScalingRule{}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "rules": rules})
}

func handleCreateScalingRule(c *gin.Context) {
	var body ScalingRule
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "JSON invalide"})
		return
	}

	if body.Project == "" || body.Service == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Les champs project et service sont requis"})
		return
	}

	validMetrics := map[string]bool{"cpu_percent": true, "mem_percent": true, "mem_mb": true}
	if !validMetrics[body.Metric] {
		body.Metric = "cpu_percent"
	}
	if body.Operator != ">" && body.Operator != "<" {
		body.Operator = ">"
	}
	if body.Direction != "up" && body.Direction != "down" {
		body.Direction = "up"
	}
	if body.Threshold == 0 {
		body.Threshold = 80
	}
	if body.DurationSeconds == 0 {
		body.DurationSeconds = 300
	}
	if body.ScaleStep == 0 {
		body.ScaleStep = 1
	}
	if body.MinReplicas == 0 {
		body.MinReplicas = 1
	}
	if body.MaxReplicas == 0 {
		body.MaxReplicas = 5
	}
	if body.CooldownSeconds == 0 {
		body.CooldownSeconds = 300
	}

	rules := loadScalingRules()
	s := sessions.Default(c)
	username, _ := s.Get("user").(string)

	updated := false
	if body.ID != "" {
		for i, r := range rules {
			if r.ID == body.ID {
				rules[i] = body
				updated = true
				break
			}
		}
	}
	if !updated {
		if body.ID == "" {
			body.ID = fmt.Sprintf("rule_%x", time.Now().UnixNano())
		}
		if body.Name == "" {
			body.Name = fmt.Sprintf("Scale %s %s", body.Service, body.Direction)
		}
		body.Enabled = true
		rules = append(rules, body)
	}

	saveScalingRules(rules)

	action := "creee"
	if updated {
		action = "modifiee"
	}
	db.LogActivity(username, "scaling_rule", body.Project,
		fmt.Sprintf("Regle %s: %s (%s %s %.0f)", action, body.Name, body.Metric, body.Operator, body.Threshold))

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Regle " + action, "rule": body})
}

func handleDeleteScalingRule(c *gin.Context) {
	ruleID := c.Param("id")
	s := sessions.Default(c)
	username, _ := s.Get("user").(string)

	rules := loadScalingRules()
	originalLen := len(rules)
	var filtered []ScalingRule
	for _, r := range rules {
		if r.ID != ruleID {
			filtered = append(filtered, r)
		}
	}

	if len(filtered) == originalLen {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "Regle introuvable"})
		return
	}

	saveScalingRules(filtered)
	db.LogActivity(username, "scaling_rule_delete", ruleID, fmt.Sprintf("Regle de scaling supprimee (id=%s)", ruleID))
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Regle supprimee"})
}

func handleScalingStatus(c *gin.Context) {
	cli := services.EnsureDocker()
	rules := loadScalingRules()

	var statusList []gin.H
	for _, rule := range rules {
		replicaCount := 0
		if cli != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			containers, err := cli.ContainerList(ctx, container.ListOptions{
				Filters: filters.NewArgs(
					filters.Arg("label", fmt.Sprintf("com.docker.compose.project=%s", rule.Project)),
				),
			})
			cancel()
			if err == nil {
				for _, ctr := range containers {
					if ctr.Labels["com.docker.compose.service"] == rule.Service {
						replicaCount++
					}
				}
			}
		}

		// Recent events
		var recentEvents []gin.H
		rows, err := db.DB.Query(
			"SELECT id, timestamp, direction, old_replicas, new_replicas, metric_value, threshold, details FROM scaling_events WHERE project=? AND service=? ORDER BY timestamp DESC LIMIT 10",
			rule.Project, rule.Service,
		)
		if err == nil {
			for rows.Next() {
				var id, oldR, newR int
				var ts, dir, details string
				var metricVal, thresh float64
				rows.Scan(&id, &ts, &dir, &oldR, &newR, &metricVal, &thresh, &details)
				recentEvents = append(recentEvents, gin.H{
					"id": id, "timestamp": ts, "direction": dir,
					"old_replicas": oldR, "new_replicas": newR,
					"metric_value": metricVal, "threshold": thresh, "details": details,
				})
			}
			rows.Close()
		}
		if recentEvents == nil {
			recentEvents = []gin.H{}
		}

		statusList = append(statusList, gin.H{
			"rule_id":          rule.ID,
			"rule_name":        rule.Name,
			"project":          rule.Project,
			"service":          rule.Service,
			"enabled":          rule.Enabled,
			"current_replicas": replicaCount,
			"min_replicas":     rule.MinReplicas,
			"max_replicas":     rule.MaxReplicas,
			"metric":           rule.Metric,
			"threshold":        rule.Threshold,
			"last_triggered":   rule.LastTriggered,
			"recent_events":    recentEvents,
		})
	}
	if statusList == nil {
		statusList = []gin.H{}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "scaling_status": statusList})
}

func handleManualEvaluate(c *gin.Context) {
	s := sessions.Default(c)
	username, _ := s.Get("user").(string)

	metrics, err := collectContainerMetrics()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	triggered := EvaluateScalingRules(metrics)
	if triggered == nil {
		triggered = []gin.H{}
	}

	db.LogActivity(username, "scaling_evaluate_manual", "",
		fmt.Sprintf("Evaluation manuelle: %d action(s) declenchee(s)", len(triggered)))

	c.JSON(http.StatusOK, gin.H{
		"status":               "ok",
		"triggered":            triggered,
		"containers_evaluated": len(metrics),
	})
}
