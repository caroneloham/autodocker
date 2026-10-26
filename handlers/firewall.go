package handlers

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/gin-gonic/gin"
)

// Whitelist of allowed values
var (
	validChains    = map[string]bool{"INPUT": true, "OUTPUT": true, "FORWARD": true}
	validActions   = map[string]bool{"ACCEPT": true, "DROP": true, "REJECT": true}
	validProtocols = map[string]bool{"tcp": true, "udp": true, "icmp": true, "all": true}
)

// RegisterFirewallRoutes registers firewall (iptables) management routes.
func RegisterFirewallRoutes(api *gin.RouterGroup) {
	fw := api.Group("/firewall")
	{
		// Read-only: login required
		fw.GET("/capable/:cid", handleFirewallCapable)
		fw.GET("/rules/:cid", handleFirewallListRules)

		// Mutations: admin required
		admin := fw.Group("")
		admin.Use(middleware.AdminRequired())
		{
			admin.POST("/rules/:cid", handleFirewallAddRule)
			admin.DELETE("/rules/:cid/:chain/:num", handleFirewallDeleteRule)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func getContainer(cid string) (*types.ContainerJSON, int, string) {
	cli := services.EnsureDocker()
	if cli == nil {
		return nil, http.StatusInternalServerError, "Docker non connecte"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := cli.ContainerInspect(ctx, cid)
	if err != nil {
		return nil, http.StatusNotFound, "Conteneur introuvable"
	}
	if info.State == nil || !info.State.Running {
		return nil, http.StatusBadRequest, "Conteneur non demarre"
	}
	return &info, 0, ""
}

type iptablesRule struct {
	Chain       string `json:"chain"`
	Num         int    `json:"num"`
	Target      string `json:"target"`
	Protocol    string `json:"protocol"`
	Opt         string `json:"opt"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Extra       string `json:"extra"`
	DPort       string `json:"dport,omitempty"`
}

var chainHeaderRe = regexp.MustCompile(`^Chain\s+(\S+)\s+\(policy\s+(\S+)`)
var dportRe = regexp.MustCompile(`dpt:(\d+)`)

func parseIptablesOutput(raw string) []iptablesRule {
	var rules []iptablesRule
	currentChain := ""

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Detect chain header
		if m := chainHeaderRe.FindStringSubmatch(line); m != nil {
			currentChain = m[1]
			continue
		}

		// Skip column headers
		if strings.HasPrefix(line, "num") || strings.HasPrefix(line, "pkts") {
			continue
		}

		// Parse rule line: num target prot opt source destination [extras]
		parts := strings.Fields(line)
		if len(parts) >= 6 {
			num, err := strconv.Atoi(parts[0])
			if err != nil {
				continue
			}
			rule := iptablesRule{
				Chain:       currentChain,
				Num:         num,
				Target:      parts[1],
				Protocol:    parts[2],
				Opt:         parts[3],
				Source:      parts[4],
				Destination: parts[5],
			}
			if len(parts) > 6 {
				rule.Extra = strings.Join(parts[6:], " ")
			}
			if m := dportRe.FindStringSubmatch(rule.Extra); m != nil {
				rule.DPort = m[1]
			}
			rules = append(rules, rule)
		}
	}
	return rules
}

func dockerExec(containerID string, cmd []string) (int, string, string, error) {
	cli := services.EnsureDocker()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	execCfg := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}
	execResp, err := cli.ContainerExecCreate(ctx, containerID, execCfg)
	if err != nil {
		return -1, "", "", err
	}

	attachResp, err := cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return -1, "", "", err
	}
	defer attachResp.Close()

	var outBuf bytes.Buffer
	outBuf.ReadFrom(attachResp.Reader)
	output := outBuf.String()

	inspectResp, err := cli.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return -1, output, "", err
	}

	return inspectResp.ExitCode, output, "", nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func handleFirewallCapable(c *gin.Context) {
	cid := c.Param("cid")
	info, code, errMsg := getContainer(cid)
	if info == nil {
		c.JSON(code, gin.H{"status": "error", "error": errMsg})
		return
	}

	exitCode, stdout, _, err := dockerExec(cid, []string{"which", "iptables"})
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "capable": false, "error": err.Error()})
		return
	}
	capable := exitCode == 0 && len(strings.TrimSpace(stdout)) > 0
	path := ""
	if capable {
		path = strings.TrimSpace(stdout)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "capable": capable, "path": path})
}

func handleFirewallListRules(c *gin.Context) {
	cid := c.Param("cid")
	info, code, errMsg := getContainer(cid)
	if info == nil {
		c.JSON(code, gin.H{"status": "error", "error": errMsg})
		return
	}

	exitCode, stdout, _, err := dockerExec(cid, []string{"iptables", "-L", "-n", "--line-numbers"})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	if exitCode != 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"status":  "error",
			"error":   fmt.Sprintf("iptables non disponible: %s", truncate(stdout, 200)),
			"capable": false,
		})
		return
	}

	rules := parseIptablesOutput(stdout)
	rawOutput := stdout
	if len(rawOutput) > 2000 {
		rawOutput = rawOutput[:2000]
	}

	c.JSON(http.StatusOK, gin.H{
		"status":         "ok",
		"rules":          rules,
		"container_name": info.Name,
		"raw":            rawOutput,
	})
}

func handleFirewallAddRule(c *gin.Context) {
	cid := c.Param("cid")
	info, code, errMsg := getContainer(cid)
	if info == nil {
		c.JSON(code, gin.H{"status": "error", "error": errMsg})
		return
	}

	var body struct {
		Chain       string `json:"chain"`
		Action      string `json:"action"`
		Protocol    string `json:"protocol"`
		Source      string `json:"source"`
		Destination string `json:"destination"`
		DPort       string `json:"dport"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "JSON invalide"})
		return
	}

	chain := strings.ToUpper(strings.TrimSpace(body.Chain))
	action := strings.ToUpper(strings.TrimSpace(body.Action))
	protocol := strings.ToLower(strings.TrimSpace(body.Protocol))
	source := strings.TrimSpace(body.Source)
	destination := strings.TrimSpace(body.Destination)
	dport := strings.TrimSpace(body.DPort)

	if chain == "" {
		chain = "INPUT"
	}
	if action == "" {
		action = "DROP"
	}
	if protocol == "" {
		protocol = "tcp"
	}

	// Validation
	if !validChains[chain] {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": fmt.Sprintf("Chain invalide: %s", chain)})
		return
	}
	if !validActions[action] {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": fmt.Sprintf("Action invalide: %s", action)})
		return
	}
	if !validProtocols[protocol] {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": fmt.Sprintf("Protocole invalide: %s", protocol)})
		return
	}

	// Validate IPs
	if source != "" {
		if _, _, err := net.ParseCIDR(source); err != nil {
			if ip := net.ParseIP(source); ip == nil {
				c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": fmt.Sprintf("Adresse source invalide: %s", source)})
				return
			}
		}
	}
	if destination != "" {
		if _, _, err := net.ParseCIDR(destination); err != nil {
			if ip := net.ParseIP(destination); ip == nil {
				c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": fmt.Sprintf("Adresse destination invalide: %s", destination)})
				return
			}
		}
	}

	// Validate port
	if dport != "" {
		p, err := strconv.Atoi(dport)
		if err != nil || p < 1 || p > 65535 {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": fmt.Sprintf("Port invalide: %s", dport)})
			return
		}
	}

	// Build iptables command
	args := []string{"iptables", "-A", chain}
	if protocol != "all" {
		args = append(args, "-p", protocol)
	}
	if source != "" {
		args = append(args, "-s", source)
	}
	if destination != "" {
		args = append(args, "-d", destination)
	}
	if dport != "" && (protocol == "tcp" || protocol == "udp") {
		args = append(args, "--dport", dport)
	}
	args = append(args, "-j", action)

	exitCode, stdout, _, err := dockerExec(cid, args)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	if exitCode != 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": fmt.Sprintf("Echec: %s", truncate(stdout, 200))})
		return
	}

	cmdStr := strings.Join(args, " ")
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": fmt.Sprintf("Regle ajoutee: %s", cmdStr), "command": cmdStr})
}

func handleFirewallDeleteRule(c *gin.Context) {
	cid := c.Param("cid")
	info, code, errMsg := getContainer(cid)
	if info == nil {
		c.JSON(code, gin.H{"status": "error", "error": errMsg})
		return
	}

	chain := strings.ToUpper(c.Param("chain"))
	numStr := c.Param("num")
	ruleNum, err := strconv.Atoi(numStr)
	if err != nil || ruleNum < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Numero de regle invalide"})
		return
	}
	if !validChains[chain] {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": fmt.Sprintf("Chain invalide: %s", chain)})
		return
	}

	args := []string{"iptables", "-D", chain, numStr}
	exitCode, stdout, _, execErr := dockerExec(cid, args)
	if execErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": execErr.Error()})
		return
	}
	if exitCode != 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": fmt.Sprintf("Echec: %s", truncate(stdout, 200))})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": fmt.Sprintf("Regle %d supprimee de %s", ruleNum, chain)})
}
