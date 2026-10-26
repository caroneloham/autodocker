package handlers

import (
	"context"
	"fmt"
	"net"
	"github.com/docker/docker/api/types/container"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"autodocker-go/config"
	"autodocker-go/services"

	"github.com/gin-gonic/gin"
)

// RegisterTracingRoutes registers distributed tracing endpoints.
func RegisterTracingRoutes(api *gin.RouterGroup) {
	api.POST("/trace", handleTrace)
}

type traceHop struct {
	Service   string  `json:"service"`
	Target    string  `json:"target"`
	LatencyMs float64 `json:"latency_ms"`
	Status    string  `json:"status"` // "ok", "error", "timeout"
	HopIndex  int     `json:"hop_index"`
	Detail    string  `json:"detail,omitempty"`
}

type traceSvcInfo struct {
	ID      string
	Name    string
	Key     string
	Image   string
	IP      string
	Ports   map[int]int
}

// POST /api/trace — trace HTTP path through service chain
func handleTrace(c *gin.Context) {
	var req struct {
		Project string `json:"project"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Project == "" {
		c.JSON(400, gin.H{"error": "project requis"})
		return
	}

	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(503, gin.H{"error": "Docker non connecte"})
		return
	}

	// List containers in the project
	containers, err := cli.ContainerList(c.Request.Context(), container.ListOptions{All: false})
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	prefix := req.Project + "-"
	projectSvcs := make([]traceSvcInfo, 0)
	for _, ctr := range containers {
		name := ""
		if len(ctr.Names) > 0 { name = strings.TrimPrefix(ctr.Names[0], "/") }
		if !strings.HasPrefix(name, prefix) { continue }

		key := strings.TrimPrefix(name, prefix)
		ip := ""
		for _, net := range ctr.NetworkSettings.Networks {
			if net.IPAddress != "" { ip = net.IPAddress; break }
		}
		ports := make(map[int]int)
		for _, p := range ctr.Ports {
			if p.PublicPort > 0 {
				ports[int(p.PrivatePort)] = int(p.PublicPort)
			}
		}
		projectSvcs = append(projectSvcs, traceSvcInfo{
			ID: ctr.ID, Name: name, Key: key,
			Image: ctr.Image, IP: ip, Ports: ports,
		})
	}

	if len(projectSvcs) == 0 {
		c.JSON(404, gin.H{"error": "Aucun conteneur trouve pour ce projet"})
		return
	}

	// Build trace hops
	hops := make([]traceHop, 0)
	hopIdx := 0

	// Sort: web servers first, then app servers, then DBs
	webSvcs := filterByImage(projectSvcs, "nginx", "traefik")
	appSvcs := filterByImage(projectSvcs, "wordpress", "nextcloud")
	dbSvcs := filterByImage(projectSvcs, "mariadb", "mysql", "postgres")

	// Trace: Host → Web
	for _, ws := range webSvcs {
		hop := traceHostToService(ws.Name, ws.Ports, hopIdx)
		hops = append(hops, hop)
		hopIdx++
	}

	// Trace: Web → App
	for _, ws := range webSvcs {
		for _, as := range appSvcs {
			hop := traceServiceToService(ws, as, hopIdx)
			hops = append(hops, hop)
			hopIdx++
		}
	}

	// Trace: App → DB
	for _, as := range appSvcs {
		for _, ds := range dbSvcs {
			hop := traceServiceToDB(as, ds, hopIdx)
			hops = append(hops, hop)
			hopIdx++
		}
	}

	// If no structured chain found, just ping all services from host
	if len(hops) == 0 {
		for _, svc := range projectSvcs {
			hop := traceHostToService(svc.Name, svc.Ports, hopIdx)
			hops = append(hops, hop)
			hopIdx++
		}
	}

	c.JSON(200, gin.H{"status": "ok", "project": req.Project, "hops": hops, "total_services": len(projectSvcs)})
}

func filterByImage(svcs []traceSvcInfo, keywords ...string) []traceSvcInfo {
	var result []traceSvcInfo
	for _, s := range svcs {
		img := strings.ToLower(s.Image)
		for _, kw := range keywords {
			if strings.Contains(img, kw) || strings.Contains(s.Key, kw) {
				result = append(result, s)
				break
			}
		}
	}
	return result
}

func traceHostToService(containerName string, ports map[int]int, hopIdx int) traceHop {
	hop := traceHop{Service: "Host", Target: containerName, HopIndex: hopIdx}

	// Try to connect to a public port
	for _, hostPort := range ports {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort), 5*time.Second)
		elapsed := time.Since(start)
		if err == nil {
			conn.Close()
			hop.LatencyMs = float64(elapsed.Microseconds()) / 1000.0
			hop.Status = "ok"
			hop.Detail = fmt.Sprintf("port %d", hostPort)
			return hop
		}
	}
	hop.Status = "error"
	hop.Detail = "aucun port accessible"
	return hop
}

func traceServiceToService(src, dst traceSvcInfo, hopIdx int) traceHop {
	hop := traceHop{Service: src.Key, Target: dst.Key, HopIndex: hopIdx}

	// Docker exec curl from src to dst IP
	port := 80
	if _, ok := dst.Ports[80]; ok {
		port = 80
	} else if _, ok := dst.Ports[8080]; ok {
		port = 8080
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/", dst.IP, port)
	cmd := exec.CommandContext(ctx, "docker", "exec", src.Name,
		"sh", "-c", fmt.Sprintf("curl -o /dev/null -s -w '%%{time_total}' --connect-timeout 5 %s 2>/dev/null || echo error", url))

	start := time.Now()
	out, err := cmd.CombinedOutput()
	fallbackLatency := float64(time.Since(start).Microseconds()) / 1000.0

	result := strings.TrimSpace(string(out))
	if err != nil || result == "error" || result == "" {
		hop.Status = "error"
		hop.LatencyMs = fallbackLatency
		hop.Detail = fmt.Sprintf("curl %s echec", url)
	} else {
		if f, err := strconv.ParseFloat(result, 64); err == nil {
			hop.LatencyMs = f * 1000 // curl reports in seconds
		} else {
			hop.LatencyMs = fallbackLatency
		}
		hop.Status = "ok"
	}
	return hop
}

func traceServiceToDB(app, dbSvc traceSvcInfo, hopIdx int) traceHop {
	hop := traceHop{Service: app.Key, Target: dbSvc.Key, HopIndex: hopIdx}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Determine DB type and check command
	dbImage := strings.ToLower(dbSvc.Image)
	var checkCmd string
	if strings.Contains(dbImage, "mariadb") || strings.Contains(dbImage, "mysql") {
		checkCmd = fmt.Sprintf("mysqladmin -h %s ping 2>&1 || echo error", dbSvc.IP)
	} else if strings.Contains(dbImage, "postgres") {
		checkCmd = fmt.Sprintf("pg_isready -h %s 2>&1 || echo error", dbSvc.IP)
	} else {
		// Generic TCP check
		checkCmd = fmt.Sprintf("timeout 5 sh -c 'echo > /dev/tcp/%s/3306' 2>/dev/null && echo ok || echo error", dbSvc.IP)
	}

	cmd := exec.CommandContext(ctx, "docker", "exec", app.Name, "sh", "-c", checkCmd)
	start := time.Now()
	out, _ := cmd.CombinedOutput()
	elapsed := float64(time.Since(start).Microseconds()) / 1000.0

	result := strings.TrimSpace(string(out))
	if strings.Contains(result, "alive") || strings.Contains(result, "ok") || strings.Contains(result, "accepting") {
		hop.Status = "ok"
	} else {
		hop.Status = "error"
	}
	hop.LatencyMs = elapsed
	hop.Detail = result
	_ = config.ProjectsDir // suppress unused import

	return hop
}
