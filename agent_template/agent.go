package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

var (
	serverURL  = "{{SERVER_URL}}"
	agentToken = "{{AGENT_TOKEN}}"
	agentName  = "{{AGENT_NAME}}"
)

func main() {
	fmt.Println("╔════════════════════════════════════════╗")
	fmt.Println("║       AutoDocker Agent                 ║")
	fmt.Printf("║  Nom    : %-28s ║\n", agentName)
	fmt.Printf("║  Serveur: %-28s ║\n", trunc(serverURL, 28))
	fmt.Println("╚════════════════════════════════════════╝")
	fmt.Println()

	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}

	for {
		metrics := collect()
		payload := map[string]interface{}{"token": agentToken, "metrics": metrics}
		body, _ := json.Marshal(payload)

		resp, err := client.Post(serverURL+"/api/agents/report", "application/json", bytes.NewReader(body))
		if err != nil {
			fmt.Printf("[%s] Erreur: %v\n", ts(), err)
		} else {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				fmt.Printf("[%s] OK - CPU: %.1f%% | RAM: %.1f%% | Disk: %.1f%%\n", ts(), metrics["cpu"], metrics["mem"], metrics["disk"])
			} else {
				fmt.Printf("[%s] Serveur: HTTP %d\n", ts(), resp.StatusCode)
			}
		}
		time.Sleep(30 * time.Second)
	}
}

func ts() string { return time.Now().Format("15:04:05") }
func trunc(s string, n int) string { if len(s) <= n { return s }; return s[:n-3] + "..." }

func collect() map[string]interface{} {
	hostname, _ := os.Hostname()
	return map[string]interface{}{
		"hostname": hostname,
		"os":       fmt.Sprintf("%s %s", runtime.GOOS, runtime.GOARCH),
		"cpu":      getCPU(),
		"mem":      getMem(),
		"mem_mb":   getMemMB(),
		"disk":     getDisk(),
		"ip":       getIP(),
	}
}

func getCPU() float64 {
	if runtime.GOOS == "windows" {
		return psFloat(`(Get-CimInstance Win32_Processor).LoadPercentage`)
	}
	out, err := exec.Command("sh", "-c", `grep 'cpu ' /proc/stat | awk '{u=$2+$4; t=$2+$4+$5; printf "%.1f", u/t*100}'`).Output()
	if err == nil { var v float64; fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &v); return v }
	return 0
}

func getMem() float64 {
	if runtime.GOOS == "windows" {
		return psFloat(`[math]::Round((1 - (Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory / (Get-CimInstance Win32_OperatingSystem).TotalVisibleMemorySize) * 100, 1)`)
	}
	out, err := exec.Command("sh", "-c", `free | awk '/Mem:/{printf "%.1f", $3/$2*100}'`).Output()
	if err == nil { var v float64; fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &v); return v }
	return 0
}

func getMemMB() float64 {
	if runtime.GOOS == "windows" {
		return psFloat(`[math]::Round(((Get-CimInstance Win32_OperatingSystem).TotalVisibleMemorySize - (Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory) / 1024, 0)`)
	}
	out, err := exec.Command("sh", "-c", `free -m | awk '/Mem:/{print $3}'`).Output()
	if err == nil { var v float64; fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &v); return v }
	return 0
}

func getDisk() float64 {
	if runtime.GOOS == "windows" {
		return psFloat(`[math]::Round((1 - (Get-PSDrive C).Free / ((Get-PSDrive C).Used + (Get-PSDrive C).Free)) * 100, 1)`)
	}
	out, err := exec.Command("sh", "-c", `df / | tail -1 | awk '{print $5}' | tr -d '%'`).Output()
	if err == nil { var v float64; fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &v); return v }
	return 0
}

func psFloat(cmd string) float64 {
	out, err := exec.Command("powershell", "-NoProfile", "-Command", cmd).Output()
	if err != nil { return 0 }
	var v float64
	fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &v)
	return v
}

func getIP() string {
	conn, err := net.DialTimeout("udp4", "8.8.8.8:53", 2*time.Second)
	if err != nil { return "127.0.0.1" }
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok { return a.IP.String() }
	return "127.0.0.1"
}
