package handlers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/gin-gonic/gin"
)

// ---- Alerting rules (in-memory + JSON file) ----

type RuleCondition struct {
	Metric   string  `json:"metric"`
	Operator string  `json:"operator"`
	Value    float64 `json:"value"`
}

type AlertRule struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Enabled         bool            `json:"enabled"`
	Target          string          `json:"target"`
	TargetType      string          `json:"target_type"`
	Conditions      []RuleCondition `json:"conditions"`
	Actions         []string        `json:"actions"`
	Severity        string          `json:"severity"`
	CooldownSeconds int             `json:"cooldown_seconds"`
}

var (
	alertRules   []AlertRule
	alertRulesMu sync.RWMutex
)

// ---- SIEM config (in-memory) ----

type SIEMTarget struct {
	Type      string            `json:"type"`
	Host      string            `json:"host,omitempty"`
	Port      int               `json:"port,omitempty"`
	URL       string            `json:"url,omitempty"`
	Path      string            `json:"path,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	VerifySSL bool              `json:"verify_ssl,omitempty"`
}

type SIEMConfig struct {
	Enabled bool         `json:"enabled"`
	Targets []SIEMTarget `json:"targets"`
}

var (
	siemConfig   = SIEMConfig{Enabled: false, Targets: []SIEMTarget{}}
	siemConfigMu sync.RWMutex
)

// RegisterSupervisionRoutes registers all supervision and agent API routes.
func RegisterSupervisionRoutes(api *gin.RouterGroup) {
	// Supervision
	api.GET("/supervision", handleSupervision)
	api.GET("/supervision/rules", handleSupervisionRulesList)
	api.POST("/supervision/rules", middleware.AdminRequired(), handleSupervisionRulesCreate)
	api.DELETE("/supervision/rules/:id", middleware.AdminRequired(), handleSupervisionRulesDelete)
	api.POST("/supervision/evaluate", handleSupervisionEvaluate)
	api.GET("/supervision/siem", middleware.AdminRequired(), handleSIEMConfigGet)
	api.POST("/supervision/siem", middleware.AdminRequired(), handleSIEMConfigSave)
	api.GET("/supervision/container/:cid/deep", handleSupervisionDeep)

	// Agents
	api.POST("/agents/generate", middleware.AdminRequired(), handleAgentsGenerate)
	api.POST("/agents/report", handleAgentsReport)
	api.GET("/agents", handleAgentsList)
	api.DELETE("/agents/:id", middleware.AdminRequired(), handleAgentsDelete)
}

// generateHex returns a random hex string of n bytes.
func generateHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- Container stats result for supervision ----

type supervisionContainerResult struct {
	ID          string  `json:"id"`
	FullID      string  `json:"full_id"`
	Name        string  `json:"name"`
	Image       string  `json:"image"`
	State       string  `json:"state"`
	Project     string  `json:"project"`
	CPUPercent  float64 `json:"cpu_percent"`
	MemMB       float64 `json:"mem_mb"`
	MemLimitMB  float64 `json:"mem_limit_mb"`
	MemPercent  float64 `json:"mem_percent"`
	NetRx       int64   `json:"net_rx"`
	NetTx       int64   `json:"net_tx"`
	NetRxMB     float64 `json:"net_rx_mb"`
	NetTxMB     float64 `json:"net_tx_mb"`
	Alert       bool    `json:"alert"`
	// Internal fields for summary calculation (not exported in JSON)
	memUsageRaw int64
	memLimitRaw int64
}

func collectOneContainerSupervision(ctx context.Context, ctr types.Container) supervisionContainerResult {
	cli := services.EnsureDocker()
	r := supervisionContainerResult{
		State:   ctr.State,
		Project: "",
	}

	// Short and full ID
	r.FullID = ctr.ID
	if len(ctr.ID) > 12 {
		r.ID = ctr.ID[:12]
	} else {
		r.ID = ctr.ID
	}

	// Name
	if len(ctr.Names) > 0 {
		r.Name = strings.TrimPrefix(ctr.Names[0], "/")
	}

	// Image
	r.Image = ctr.Image

	// Project label
	if ctr.Labels != nil {
		r.Project = ctr.Labels["com.docker.compose.project"]
	}

	// Collect stats only for running containers
	if ctr.State == "running" && cli != nil {
		stats, err := services.CollectContainerStats(ctx, cli, ctr.ID)
		if err == nil {
			r.CPUPercent = math.Round(stats.CPU*100) / 100
			r.MemMB = math.Round(stats.MemMB*100) / 100
			r.MemLimitMB = math.Round(stats.MemLimMB*100) / 100
			r.NetRx = stats.NetRx
			r.NetTx = stats.NetTx
			r.NetRxMB = math.Round(float64(stats.NetRx) / 1048576 * 100) / 100
			r.NetTxMB = math.Round(float64(stats.NetTx) / 1048576 * 100) / 100
			r.memUsageRaw = int64(stats.MemMB * 1048576)
			r.memLimitRaw = int64(stats.MemLimMB)
			if r.MemLimitMB > 0 {
				r.MemPercent = math.Round((r.MemMB/r.MemLimitMB)*10000) / 100
			}
			if r.CPUPercent > 80 || r.MemPercent > 85 {
				r.Alert = true
			}
		}
	}

	return r
}

// ---- GET /api/supervision ----

func handleSupervision(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	containers, err := cli.ContainerList(ctx, container.ListOptions{All: false})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Collect stats with goroutine parallelism (max 20 concurrent)
	results := make([]supervisionContainerResult, len(containers))
	sem := make(chan struct{}, 20)
	var wg sync.WaitGroup

	for i, ctr := range containers {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, ct types.Container) {
			defer wg.Done()
			defer func() { <-sem }()
			results[idx] = collectOneContainerSupervision(ctx, ct)
		}(i, ctr)
	}
	wg.Wait()

	// Compute global summary
	var globalCPU, globalMemMB, globalMemLimitMB float64
	alertsCount := 0
	for _, r := range results {
		globalCPU += r.CPUPercent
		globalMemMB += r.MemMB
		globalMemLimitMB += r.MemLimitMB
		if r.Alert {
			alertsCount++
		}
	}
	globalMemPct := 0.0
	if globalMemLimitMB > 0 {
		globalMemPct = (globalMemMB / globalMemLimitMB) * 100
	}

	// Sort by CPU descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].CPUPercent > results[j].CPUPercent
	})

	// Fetch agents
	agentsData := fetchAgentsData()
	agentsOnline := 0
	for _, a := range agentsData {
		if a["status"] == "online" {
			agentsOnline++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status":    "ok",
		"timestamp": time.Now().Unix(),
		"summary": gin.H{
			"total_running":    len(results),
			"global_cpu":       math.Round(globalCPU*10) / 10,
			"global_mem_mb":    math.Round(globalMemMB*10) / 10,
			"global_mem_limit_mb": math.Round(globalMemLimitMB),
			"global_mem_percent":  math.Round(globalMemPct*10) / 10,
			"alerts":           alertsCount,
			"agents_online":    agentsOnline,
			"agents_total":     len(agentsData),
		},
		"containers": results,
		"agents":     agentsData,
	})
}

func fetchAgentsData() []gin.H {
	agents := make([]gin.H, 0)
	rows, err := db.DB.Query("SELECT agent_id, name, hostname, os_info, ip_address, last_seen, last_data FROM agents")
	if err != nil {
		return agents
	}
	defer rows.Close()

	now := time.Now()
	for rows.Next() {
		var agentID, name, hostname, osInfo, ipAddress, lastSeen, lastData sql.NullString
		if err := rows.Scan(&agentID, &name, &hostname, &osInfo, &ipAddress, &lastSeen, &lastData); err != nil {
			continue
		}

		status := "offline"
		lastSeenStr := ""
		if lastSeen.Valid && lastSeen.String != "" {
			lastSeenStr = lastSeen.String
			t, err := time.Parse("2006-01-02 15:04:05", lastSeen.String)
			if err == nil && now.Sub(t).Seconds() < 60 {
				status = "online"
			}
		}

		metrics := gin.H{}
		if lastData.Valid && lastData.String != "" {
			_ = json.Unmarshal([]byte(lastData.String), &metrics)
		}

		h := hostname.String
		if h == "" {
			h = name.String
		}

		agents = append(agents, gin.H{
			"agent_id":   agentID.String,
			"name":       name.String,
			"hostname":   h,
			"os_info":    osInfo.String,
			"ip_address": ipAddress.String,
			"status":     status,
			"last_seen":  lastSeenStr,
			"metrics":    metrics,
		})
	}
	return agents
}

// ---- GET /api/supervision/rules ----

func handleSupervisionRulesList(c *gin.Context) {
	alertRulesMu.RLock()
	defer alertRulesMu.RUnlock()
	c.JSON(http.StatusOK, gin.H{"status": "ok", "rules": alertRules})
}

// ---- POST /api/supervision/rules ----

func handleSupervisionRulesCreate(c *gin.Context) {
	var data struct {
		ID              string          `json:"id"`
		Name            string          `json:"name"`
		Enabled         *bool           `json:"enabled"`
		Target          string          `json:"target"`
		TargetType      string          `json:"target_type"`
		Conditions      []RuleCondition `json:"conditions"`
		Actions         []string        `json:"actions"`
		Severity        string          `json:"severity"`
		CooldownSeconds int             `json:"cooldown_seconds"`
	}
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "JSON invalide"})
		return
	}

	rule := AlertRule{
		ID:              data.ID,
		Name:            data.Name,
		Enabled:         true,
		Target:          data.Target,
		TargetType:      data.TargetType,
		Conditions:      data.Conditions,
		Actions:         data.Actions,
		Severity:        data.Severity,
		CooldownSeconds: data.CooldownSeconds,
	}

	if rule.ID == "" {
		rule.ID = generateHex(6)
	}
	if rule.Name == "" {
		rule.Name = "Regle sans nom"
	}
	if data.Enabled != nil {
		rule.Enabled = *data.Enabled
	}
	if rule.Target == "" {
		rule.Target = "*"
	}
	if rule.TargetType == "" {
		rule.TargetType = "container"
	}
	if rule.Conditions == nil {
		rule.Conditions = []RuleCondition{}
	}
	if rule.Actions == nil {
		rule.Actions = []string{"dashboard"}
	}
	if rule.Severity == "" {
		rule.Severity = "warning"
	}
	if rule.CooldownSeconds == 0 {
		rule.CooldownSeconds = 300
	}

	alertRulesMu.Lock()
	// Update existing rule with same ID, or append
	found := false
	for i, r := range alertRules {
		if r.ID == rule.ID {
			alertRules[i] = rule
			found = true
			break
		}
	}
	if !found {
		alertRules = append(alertRules, rule)
	}
	alertRulesMu.Unlock()

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "supervision_rule_save", rule.Name, "")

	c.JSON(http.StatusOK, gin.H{"status": "ok", "rule": rule})
}

// ---- DELETE /api/supervision/rules/:id ----

func handleSupervisionRulesDelete(c *gin.Context) {
	ruleID := c.Param("id")

	alertRulesMu.Lock()
	newRules := make([]AlertRule, 0, len(alertRules))
	for _, r := range alertRules {
		if r.ID != ruleID {
			newRules = append(newRules, r)
		}
	}
	alertRules = newRules
	alertRulesMu.Unlock()

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "supervision_rule_delete", ruleID, "")

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ---- POST /api/supervision/evaluate ----

func handleSupervisionEvaluate(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}

	alertRulesMu.RLock()
	rules := make([]AlertRule, len(alertRules))
	copy(rules, alertRules)
	alertRulesMu.RUnlock()

	if len(rules) == 0 {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "triggered": []gin.H{}})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	containers, err := cli.ContainerList(ctx, container.ListOptions{All: false})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Collect stats (max 10 concurrent)
	results := make([]supervisionContainerResult, len(containers))
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup

	for i, ctr := range containers {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, ct types.Container) {
			defer wg.Done()
			defer func() { <-sem }()
			results[idx] = collectOneContainerSupervision(ctx, ct)
		}(i, ctr)
	}
	wg.Wait()

	triggered := make([]gin.H, 0)
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		for _, cr := range results {
			target := rule.Target
			tt := rule.TargetType

			if target != "*" {
				if tt == "container" && !strings.Contains(strings.ToLower(cr.Name), strings.ToLower(target)) {
					continue
				}
				if tt == "project" && !strings.Contains(strings.ToLower(cr.Project), strings.ToLower(target)) {
					continue
				}
			}

			allMatch := true
			details := make([]string, 0)
			for _, cond := range rule.Conditions {
				actual := getSupervisionMetricValue(cr, cond.Metric)
				matched := evaluateCondition(actual, cond.Operator, cond.Value)
				if !matched {
					allMatch = false
					break
				}
				details = append(details, fmt.Sprintf("%s %s %.1f (actuel: %.2f)", cond.Metric, cond.Operator, cond.Value, actual))
			}

			if allMatch && len(rule.Conditions) > 0 {
				triggered = append(triggered, gin.H{
					"rule_id":   rule.ID,
					"rule_name": rule.Name,
					"severity":  rule.Severity,
					"container": cr.Name,
					"project":   cr.Project,
					"details":   details,
					"actions":   rule.Actions,
					"timestamp": time.Now().Unix(),
				})
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "triggered": triggered})
}

func getSupervisionMetricValue(cr supervisionContainerResult, metric string) float64 {
	switch metric {
	case "cpu_percent":
		return cr.CPUPercent
	case "mem_percent", "memory_percent":
		return cr.MemPercent
	case "mem_mb", "memory_mb":
		return cr.MemMB
	case "net_rx":
		return float64(cr.NetRx)
	case "net_tx":
		return float64(cr.NetTx)
	default:
		return 0
	}
}

func evaluateCondition(actual float64, operator string, threshold float64) bool {
	switch operator {
	case ">":
		return actual > threshold
	case ">=":
		return actual >= threshold
	case "<":
		return actual < threshold
	case "<=":
		return actual <= threshold
	case "==":
		return actual == threshold
	default:
		return false
	}
}

// ---- GET /api/supervision/siem ----

func handleSIEMConfigGet(c *gin.Context) {
	siemConfigMu.RLock()
	defer siemConfigMu.RUnlock()
	c.JSON(http.StatusOK, gin.H{"status": "ok", "config": siemConfig})
}

// ---- POST /api/supervision/siem ----

func handleSIEMConfigSave(c *gin.Context) {
	var data SIEMConfig
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "JSON invalide"})
		return
	}

	siemConfigMu.Lock()
	siemConfig = data
	siemConfigMu.Unlock()

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "siem_config_update", "", "")

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ---- GET /api/supervision/container/:cid/deep ----

func handleSupervisionDeep(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Docker non connecte"})
		return
	}

	cid := c.Param("cid")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ctr, err := cli.ContainerInspect(ctx, cid)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Conteneur introuvable"})
		return
	}

	state := ctr.State
	hc := state.Health
	hostConfig := ctr.HostConfig

	// Processes
	processes := make([]gin.H, 0)
	if state.Running {
		top, err := cli.ContainerTop(ctx, cid, []string{})
		if err == nil {
			for _, proc := range top.Processes {
				entry := gin.H{}
				for j, title := range top.Titles {
					if j < len(proc) {
						entry[title] = proc[j]
					}
				}
				processes = append(processes, entry)
			}
		}
	}

	// Networks
	networks := gin.H{}
	if ctr.NetworkSettings != nil {
		for netName, netData := range ctr.NetworkSettings.Networks {
			networks[netName] = gin.H{
				"ip":      netData.IPAddress,
				"gateway": netData.Gateway,
				"mac":     netData.MacAddress,
			}
		}
	}

	// Mounts
	mounts := make([]gin.H, 0)
	for _, m := range ctr.Mounts {
		mounts = append(mounts, gin.H{
			"type":        string(m.Type),
			"source":      m.Source,
			"destination": m.Destination,
			"mode":        m.Mode,
			"rw":          m.RW,
		})
	}

	// Image
	image := "unknown"
	if ctr.Config != nil && len(ctr.Config.Image) > 0 {
		image = ctr.Config.Image
	}

	// Health
	healthStatus := "none"
	healthLog := make([]gin.H, 0)
	if hc != nil {
		healthStatus = hc.Status
		logs := hc.Log
		if len(logs) > 5 {
			logs = logs[len(logs)-5:]
		}
		for _, l := range logs {
			healthLog = append(healthLog, gin.H{
				"output":    l.Output,
				"exit_code": l.ExitCode,
				"start":     l.Start.Format(time.RFC3339),
			})
		}
	}

	// Labels (filter out com.docker.* prefix)
	labels := gin.H{}
	if ctr.Config != nil && ctr.Config.Labels != nil {
		for k, v := range ctr.Config.Labels {
			if !strings.HasPrefix(k, "com.docker") {
				labels[k] = v
			}
		}
	}

	// User
	user := "root"
	if ctr.Config != nil && ctr.Config.User != "" {
		user = ctr.Config.User
	}

	// Working dir
	workDir := "/"
	if ctr.Config != nil && ctr.Config.WorkingDir != "" {
		workDir = ctr.Config.WorkingDir
	}

	// Capabilities
	capAdd := []string{}
	capDrop := []string{}
	if hostConfig != nil {
		if hostConfig.CapAdd != nil {
			capAdd = hostConfig.CapAdd
		}
		if hostConfig.CapDrop != nil {
			capDrop = hostConfig.CapDrop
		}
	}

	// Resource limits
	memLimit := int64(0)
	cpuShares := int64(0)
	cpuQuota := int64(0)
	pidsLimit := int64(0)
	privileged := false
	readonlyRootfs := false
	if hostConfig != nil {
		memLimit = hostConfig.Memory
		cpuShares = hostConfig.CPUShares
		cpuQuota = hostConfig.CPUQuota
		if hostConfig.PidsLimit != nil {
			pidsLimit = *hostConfig.PidsLimit
		}
		privileged = hostConfig.Privileged
		readonlyRootfs = hostConfig.ReadonlyRootfs
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"container": gin.H{
			"id":               ctr.ID[:12],
			"name":             strings.TrimPrefix(ctr.Name, "/"),
			"image":            image,
			"state":            state.Status,
			"created":          ctr.Created,
			"started_at":       state.StartedAt,
			"restart_count":    ctr.RestartCount,
			"platform":         ctr.Platform,
			"cmd":              ctr.Config.Cmd,
			"entrypoint":       ctr.Config.Entrypoint,
			"user":             user,
			"working_dir":      workDir,
			"health_status":    healthStatus,
			"health_log":       healthLog,
			"memory_limit":     memLimit,
			"cpu_shares":       cpuShares,
			"cpu_quota":        cpuQuota,
			"pids_limit":       pidsLimit,
			"privileged":       privileged,
			"readonly_rootfs":  readonlyRootfs,
			"cap_add":          capAdd,
			"cap_drop":         capDrop,
			"processes":        processes,
			"networks":         networks,
			"mounts":           mounts,
			"labels":           labels,
		},
	})
}

// ---- POST /api/agents/generate ----

func handleAgentsGenerate(c *gin.Context) {
	var data struct {
		Name      string `json:"name"`
		Format    string `json:"format"`
		ServerURL string `json:"server_url"`
	}
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "JSON invalide"})
		return
	}

	agentName := data.Name
	if agentName == "" {
		agentName = "agent-" + generateHex(4)
	}

	agentID := generateHex(8)
	agentToken := generateHex(16)

	serverURL := strings.TrimSpace(data.ServerURL)
	if serverURL == "" {
		proto := "http"
		if c.Request.TLS != nil {
			proto = "https"
		}
		serverURL = fmt.Sprintf("%s://%s", proto, c.Request.Host)
	}

	user, _ := middleware.GetSessionUser(c)
	now := time.Now().Format("2006-01-02 15:04:05")

	_, err := db.DB.Exec(
		"INSERT INTO agents (agent_id, name, token, created_by, created_at) VALUES (?, ?, ?, ?, ?)",
		agentID, agentName, agentToken, user, now,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	db.LogActivity(user, "agent_generate", agentName, "")

	format := strings.ToLower(strings.TrimSpace(data.Format))
	if format == "exe" || format == ".exe" || format == "bin" || format == "binary" {
		// Compile Go agent for current platform
		binPath, err := compileGoAgent(agentName, serverURL, agentToken)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Compilation echouee: " + err.Error()})
			return
		}
		defer os.Remove(binPath)
		filename := agentName
		if runtime.GOOS == "windows" {
			filename += ".exe"
		}
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
		c.File(binPath)
		return
	}

	// Default: Python script
	script := fmt.Sprintf(`#!/usr/bin/env python3
"""AutoDocker Agent — %s"""
import time, json, platform, socket
try:
    import psutil
    import requests
except ImportError:
    print("pip install psutil requests")
    exit(1)

SERVER_URL = "%s"
AGENT_TOKEN = "%s"
AGENT_NAME = "%s"

def collect():
    return {
        "hostname": socket.gethostname(),
        "os": f"{platform.system()} {platform.release()}",
        "cpu": psutil.cpu_percent(interval=1),
        "mem": psutil.virtual_memory().percent,
        "mem_mb": round(psutil.virtual_memory().used / 1048576, 2),
        "disk": psutil.disk_usage("/").percent,
    }

while True:
    try:
        m = collect()
        requests.post(f"{SERVER_URL}/api/agents/report",
                       json={"token": AGENT_TOKEN, "metrics": m},
                       timeout=10, verify=False)
    except Exception as e:
        print(f"Error: {e}")
    time.sleep(30)
`, agentName, serverURL, agentToken, agentName)

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s.py", agentName))
	c.Data(http.StatusOK, "text/x-python", []byte(script))
}

// compileGoAgent compiles the agent Go template with injected credentials for the current OS
func compileGoAgent(name, serverURL, token string) (string, error) {
	// Find agent template
	exeDir := "."
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	templatePath := filepath.Join(exeDir, "agent_template", "agent.go")
	if _, err := os.Stat(templatePath); err != nil {
		// Fallback: check relative to working directory
		templatePath = filepath.Join("agent_template", "agent.go")
		if _, err := os.Stat(templatePath); err != nil {
			return "", fmt.Errorf("template agent.go introuvable")
		}
	}

	// Read template and inject values
	tmplBytes, err := os.ReadFile(templatePath)
	if err != nil {
		return "", err
	}
	source := string(tmplBytes)
	source = strings.ReplaceAll(source, "{{SERVER_URL}}", serverURL)
	source = strings.ReplaceAll(source, "{{AGENT_TOKEN}}", token)
	source = strings.ReplaceAll(source, "{{AGENT_NAME}}", name)

	// Write to temp directory
	tmpDir, err := os.MkdirTemp("", "autodocker-agent-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	srcFile := filepath.Join(tmpDir, "agent.go")
	if err := os.WriteFile(srcFile, []byte(source), 0644); err != nil {
		return "", err
	}

	// Compile for current OS
	ext := ""
	targetOS := runtime.GOOS
	if targetOS == "windows" {
		ext = ".exe"
	}
	outFile := filepath.Join(os.TempDir(), name+ext)
	cmd := exec.Command("go", "build", "-o", outFile, "-ldflags", "-s -w", srcFile)
	cmd.Env = append(os.Environ(), "GOOS="+targetOS, "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%s: %s", err.Error(), string(output))
	}

	return outFile, nil
}

// ---- POST /api/agents/report ----

func handleAgentsReport(c *gin.Context) {
	var data struct {
		Token   string         `json:"token"`
		Metrics map[string]any `json:"metrics"`
	}
	if err := c.ShouldBindJSON(&data); err != nil || data.Token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Token manquant"})
		return
	}

	var agentID string
	err := db.DB.QueryRow("SELECT agent_id FROM agents WHERE token = ?", data.Token).Scan(&agentID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Token invalide"})
		return
	}

	now := time.Now().Format("2006-01-02 15:04:05")
	metricsJSON, _ := json.Marshal(data.Metrics)
	hostname, _ := data.Metrics["hostname"].(string)
	osInfo, _ := data.Metrics["os"].(string)

	_, _ = db.DB.Exec(
		`UPDATE agents SET last_seen = ?, status = 'online', hostname = ?, os_info = ?, ip_address = ?, last_data = ? WHERE agent_id = ?`,
		now, hostname, osInfo, c.ClientIP(), string(metricsJSON), agentID,
	)

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ---- GET /api/agents ----

func handleAgentsList(c *gin.Context) {
	rows, err := db.DB.Query("SELECT agent_id, name, created_by, created_at, last_seen, hostname, os_info, ip_address, last_data FROM agents ORDER BY last_seen DESC")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	agents := make([]gin.H, 0)
	now := time.Now()

	for rows.Next() {
		var agentID, name, createdBy, createdAt, lastSeen, hostname, osInfo, ipAddress, lastData sql.NullString
		if err := rows.Scan(&agentID, &name, &createdBy, &createdAt, &lastSeen, &hostname, &osInfo, &ipAddress, &lastData); err != nil {
			continue
		}

		status := "offline"
		if lastSeen.Valid && lastSeen.String != "" {
			t, err := time.Parse("2006-01-02 15:04:05", lastSeen.String)
			if err == nil && now.Sub(t).Seconds() < 60 {
				status = "online"
			}
		}

		metrics := gin.H{}
		if lastData.Valid && lastData.String != "" {
			_ = json.Unmarshal([]byte(lastData.String), &metrics)
		}

		agents = append(agents, gin.H{
			"agent_id":   agentID.String,
			"name":       name.String,
			"created_by": createdBy.String,
			"created_at": createdAt.String,
			"last_seen":  lastSeen.String,
			"status":     status,
			"hostname":   hostname.String,
			"os_info":    osInfo.String,
			"ip_address": ipAddress.String,
			"metrics":    metrics,
		})
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "agents": agents})
}

// ---- DELETE /api/agents/:id ----

func handleAgentsDelete(c *gin.Context) {
	agentID := c.Param("id")

	_, err := db.DB.Exec("DELETE FROM agents WHERE agent_id = ?", agentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	user, _ := middleware.GetSessionUser(c)
	db.LogActivity(user, "agent_delete", agentID, "")

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
