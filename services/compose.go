package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"
)

// ---------------------------------------------------------------------------
// Service catalogue
// ---------------------------------------------------------------------------

// ImagesBase contains the built-in service images (cannot be removed).
var ImagesBase = map[string]string{
	"portainer":     "portainer/portainer-ce:lts",
	"traefik":       "traefik:v3.3",
	"nginx":         "nginx:alpine",
	"wordpress":     "wordpress:latest",
	"moodle":        "bitnami/moodle:latest",
	"ollama":        "ollama/ollama:latest",
	"open-webui":    "ghcr.io/open-webui/open-webui:main",
	"gitea":         "gitea/gitea:latest",
	"n8n":           "n8nio/n8n:latest",
	"uptime-kuma":   "louislam/uptime-kuma:1",
	"minio":         "minio/minio:latest",
	"redis":         "redis:7-alpine",
	"mongo":         "mongo:7",
	"mongo-express": "mongo-express:latest",
	"mariadb":       "mariadb:11",
	"postgres":      "postgres:16-alpine",
	"nextcloud":     "nextcloud:stable",
	"dokuwiki":      "dokuwiki/dokuwiki:stable",
	"samba":         "ghcr.io/servercontainers/samba:latest",
	"grafana":       "grafana/grafana-oss:latest",
	"prometheus":    "prom/prometheus:latest",
	"adminer":       "adminer:latest",
	"pgadmin":       "dpage/pgadmin4:latest",
	"fail2ban":      "crazymax/fail2ban:latest",
	"debian":        "debian:bookworm",
}

// UIPorts maps service keys to their default UI port.
var UIPorts = map[string]int{
	"portainer":     9443,
	"traefik":       8081,
	"nginx":         8080,
	"wordpress":     8082,
	"moodle":        8086,
	"ollama":        11434,
	"open-webui":    3001,
	"gitea":         3002,
	"n8n":           5678,
	"uptime-kuma":   3003,
	"minio":         9001,
	"mongo-express": 8087,
	"nextcloud":     8083,
	"dokuwiki":      8090,
	"grafana":       3000,
	"prometheus":    9090,
	"adminer":       8085,
	"pgadmin":       5050,
}

// CategoryMap maps service keys to their UI category.
var CategoryMap = map[string]string{
	"portainer":     "socle",
	"traefik":       "socle",
	"nginx":         "web",
	"wordpress":     "web",
	"moodle":        "web",
	"ollama":        "ai",
	"open-webui":    "ai",
	"gitea":         "web",
	"n8n":           "web",
	"uptime-kuma":   "observ",
	"minio":         "web",
	"redis":         "db",
	"mongo":         "db",
	"mongo-express": "db",
	"mariadb":       "db",
	"postgres":      "db",
	"adminer":       "db",
	"pgadmin":       "db",
	"nextcloud":     "collab",
	"dokuwiki":      "collab",
	"samba":         "collab",
	"grafana":       "observ",
	"prometheus":    "observ",
	"fail2ban":      "sec",
	"debian":        "network",
}

// ValidCategories lists accepted catalogue categories.
var ValidCategories = map[string]bool{
	"socle": true, "web": true, "db": true, "collab": true,
	"observ": true, "sec": true, "ai": true, "network": true, "custom": true,
}

const DefaultOllamaModel = "mistral:7b-instruct-v0.2-q3_K_M"
const FastOllamaModel = "tinyllama:1.1b-chat-v1-q4_K_M"

// Images is the merged catalogue (base + custom). Protected by mu.
var (
	Images   = make(map[string]string)
	customMu sync.RWMutex
)

func init() {
	for key, image := range professionalAIImages {
		ImagesBase[key] = image
		CategoryMap[key] = "ai"
	}
	// Seed Images with base catalogue
	for k, v := range ImagesBase {
		Images[k] = v
	}
}

// MergeCustomCatalogue adds custom entries into the live catalogue.
func MergeCustomCatalogue(customs map[string]string) {
	customMu.Lock()
	defer customMu.Unlock()
	// Reset to base
	Images = make(map[string]string)
	for k, v := range ImagesBase {
		Images[k] = v
	}
	for k, v := range customs {
		Images[k] = v
	}
}

// GetImage returns the image for a key, or empty string.
func GetImage(key string) string {
	customMu.RLock()
	defer customMu.RUnlock()
	return Images[key]
}

// GetAllImages returns a copy of the full catalogue.
func GetAllImages() map[string]string {
	customMu.RLock()
	defer customMu.RUnlock()
	out := make(map[string]string, len(Images))
	for k, v := range Images {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// Port allocation helpers
// ---------------------------------------------------------------------------

// buildPortTracker tracks ports allocated during a single build_compose call
// to avoid conflicts within the same compose file.
type buildPortTracker struct {
	used     map[int]bool
	portable bool
}

func newPortTracker() *buildPortTracker {
	return &buildPortTracker{used: make(map[int]bool)}
}

// getFreePort finds a free host port starting from target.
func (pt *buildPortTracker) getFreePort(target int) int {
	port := target
	for port < 65535 {
		if !pt.used[port] && (pt.portable || !isPortBusy(port)) {
			pt.used[port] = true
			return port
		}
		port++
	}
	// Fallback: just return target
	pt.used[target] = true
	return target
}

// isPortBusy checks whether a TCP port is already in use on the host.
func isPortBusy(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

// GetDockerUsedPorts queries docker for ports in use by running containers.
func GetDockerUsedPorts() map[int]bool {
	ports := make(map[int]bool)
	out, err := exec.Command("docker", "ps", "--format", "{{.Ports}}").Output()
	if err != nil {
		return ports
	}
	re := regexp.MustCompile(`0\.0\.0\.0:(\d+)`)
	for _, m := range re.FindAllStringSubmatch(string(out), -1) {
		if p, err := strconv.Atoi(m[1]); err == nil {
			ports[p] = true
		}
	}
	return ports
}

// GetUsedSubnets reserves other projects' configured and live Docker subnets.
// The current project's own networks must remain usable during a redeployment.
func GetUsedSubnets(projectsDir string, exclude string) map[string]bool {
	return getUsedSubnetsWithRunner(projectsDir, exclude, func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.WaitDelay = time.Second
		return cmd.Output()
	})
}

func getUsedSubnetsWithRunner(projectsDir, exclude string, run func(context.Context, ...string) ([]byte, error)) map[string]bool {
	used := make(map[string]bool)
	entries, _ := os.ReadDir(projectsDir)
	for _, entry := range entries {
		if !entry.IsDir() || strings.EqualFold(entry.Name(), exclude) {
			continue
		}
		for _, filename := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
			data, err := os.ReadFile(filepath.Join(projectsDir, entry.Name(), filename))
			if err != nil {
				continue
			}
			var project struct {
				Networks map[string]struct {
					IPAM struct {
						Config []struct {
							Subnet string `yaml:"subnet"`
						} `yaml:"config"`
					} `yaml:"ipam"`
				} `yaml:"networks"`
			}
			if yaml.Unmarshal(data, &project) == nil {
				for _, network := range project.Networks {
					for _, item := range network.IPAM.Config {
						if item.Subnet != "" {
							used[item.Subnet] = true
						}
					}
				}
			}
			break
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := run(ctx, "network", "ls", "-q")
	if err != nil {
		return used
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return used
	}
	out, err = run(ctx, append([]string{"network", "inspect"}, ids...)...)
	if err != nil {
		return used
	}
	var networks []struct {
		Labels map[string]string
		IPAM   struct{ Config []struct{ Subnet string } }
	}
	if json.Unmarshal(out, &networks) != nil {
		return used
	}
	for _, network := range networks {
		if exclude != "" && strings.EqualFold(network.Labels["com.docker.compose.project"], exclude) {
			continue
		}
		for _, item := range network.IPAM.Config {
			if item.Subnet != "" {
				used[item.Subnet] = true
			}
		}
	}
	return used
}

// FindFreeSubnet picks a subnet that does not conflict with used ones.
// If the requested CIDR is free, it returns it as-is.
func FindFreeSubnet(requested string, used map[string]bool) string {
	if !used[requested] {
		return requested
	}
	_, ipNet, err := net.ParseCIDR(requested)
	if err != nil {
		return requested
	}
	ones, _ := ipNet.Mask.Size()
	base := ipNet.IP.To4()
	if base == nil {
		return requested
	}
	// Try incrementing the third octet
	for i := 1; i < 255; i++ {
		candidate := fmt.Sprintf("%d.%d.%d.0/%d", base[0], base[1], (int(base[2])+i)%256, ones)
		if !used[candidate] {
			return candidate
		}
	}
	return requested
}

// GatewayFromCIDR calculates the .1 gateway address from a CIDR.
// e.g. "172.20.14.0/24" -> "172.20.14.1"
func GatewayFromCIDR(cidr string) string {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ip := ipNet.IP.To4()
	if ip == nil {
		return ""
	}
	ip[3]++
	return ip.String()
}

// ---------------------------------------------------------------------------
// Password generation
// ---------------------------------------------------------------------------

func generatePassword(length int) string {
	b := make([]byte, length)
	_, _ = rand.Read(b)
	s := base64.URLEncoding.EncodeToString(b)
	if len(s) > length {
		s = s[:length]
	}
	return s
}

// ---------------------------------------------------------------------------
// Compose types — we use map[string]interface{} throughout so YAML marshaling
// produces the exact shape Docker Compose expects.
// ---------------------------------------------------------------------------

// M is a shorthand for map[string]interface{}.
type M = map[string]interface{}

// ---------------------------------------------------------------------------
// BuildCompose — core business logic
// ---------------------------------------------------------------------------

// BuildComposePayload is the JSON payload sent by the frontend.
type BuildComposePayload struct {
	Client      *ClientDeliveryConfig `json:"client,omitempty"`
	Supervision *SupervisionConfig    `json:"supervision,omitempty"`
	AIStack     *AIStackConfig        `json:"ai_stack,omitempty"`
	Ingress     *IngressConfig        `json:"ingress,omitempty"`
	Services    []ServiceEntry        `json:"services"`
	Addressing  AddressingConfig      `json:"addressing"`
	UI          UIFlags               `json:"ui"`
	Project     string                `json:"project"`
}

// ServiceEntry represents one service in the payload.
type ServiceEntry struct {
	Key     string            `json:"key"`
	Network string            `json:"network"`
	IP      string            `json:"ip"`
	LAN     *RouterInterface  `json:"lan,omitempty"`
	WAN     *RouterInterface  `json:"wan,omitempty"`
	Extra   map[string]string `json:"extra,omitempty"`
	// Freeform fields: services outside the catalogue proposed by the AI.
	// Untrusted input — validated field by field in buildCustomService.
	Image     string            `json:"image,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Ports     []string          `json:"ports,omitempty"`
	Volumes   []string          `json:"volumes,omitempty"`
	Command   []string          `json:"command,omitempty"`
	DependsOn []string          `json:"depends_on,omitempty"`
}

// RouterInterface holds network + ip for debian-router LAN/WAN.
type RouterInterface struct {
	Network string `json:"network"`
	IP      string `json:"ip"`
}

// AddressingConfig defines network topology.
type AddressingConfig struct {
	Networks []NetworkDef `json:"networks"`
}

// NetworkDef is one network entry.
type NetworkDef struct {
	Name    string `json:"name"`
	CIDR    string `json:"cidr"`
	Gateway string `json:"gateway"`
}

// UIFlags controls which UI port categories to expose.
type UIFlags struct {
	Socle  *bool `json:"socle"`
	Web    *bool `json:"web"`
	DB     *bool `json:"db"`
	Collab *bool `json:"collab"`
	Observ *bool `json:"observ"`
	AI     *bool `json:"ai"`
}

func boolDefault(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

// BuildCompose generates a complete Docker Compose structure from the payload.
func BuildCompose(payload BuildComposePayload, project string) M {
	return buildCompose(payload, project, false)
}

// BuildComposeForLocalDeployment keeps local port allocation while applying the
// same safe access defaults as a downloadable stack. The raw builder remains
// available for callers that need to inspect the unmodified service template.
func BuildComposeForLocalDeployment(payload BuildComposePayload, project string) M {
	compose := buildCompose(payload, project, false)
	secureGeneratedDefaults(compose, payload)
	return compose
}

// BuildComposeForExport avoids reserving or probing ports on the generator's
// host. Only port collisions within this independently downloaded stack count.
func BuildComposeForExport(payload BuildComposePayload, project string) M {
	compose := buildCompose(payload, project, true)
	secureGeneratedDefaults(compose, payload)
	connectNextcloudRedis(compose, compose["services"].(M))
	provisionGeneratedSQLAccounts(compose)
	return compose
}

func buildCompose(payload BuildComposePayload, project string, portable bool) M {
	pt := newPortTracker()
	pt.portable = portable

	compose := M{
		"services": M{},
		"volumes":  M{},
		"networks": buildIPAM(payload.Addressing.Networks),
	}
	networks := compose["networks"].(M)
	services := compose["services"].(M)
	volumes := compose["volumes"].(M)

	netNames := make(map[string]bool)
	for n := range networks {
		netNames[n] = true
	}

	// Helper: default network name
	defaultNet := func() string {
		for n := range netNames {
			return n
		}
		return "default"
	}

	// Track used IPs per network to avoid conflicts
	usedIPs := make(map[string]map[string]bool) // netName -> set of IPs
	for _, n := range payload.Addressing.Networks {
		usedIPs[n.Name] = map[string]bool{n.Gateway: true}
	}
	// Collect all manually assigned IPs
	for _, s := range payload.Services {
		if s.IP != "" && s.Network != "" {
			if usedIPs[s.Network] == nil {
				usedIPs[s.Network] = map[string]bool{}
			}
			usedIPs[s.Network][s.IP] = true
		}
	}

	// Find a free IP on a network
	nextFreeIP := func(netName string) string {
		for _, n := range payload.Addressing.Networks {
			if n.Name == netName {
				_, ipNet, err := net.ParseCIDR(n.CIDR)
				if err != nil {
					return ""
				}
				base := ipNet.IP.To4()
				if base == nil {
					return ""
				}
				if usedIPs[netName] == nil {
					usedIPs[netName] = map[string]bool{}
				}
				for i := 100; i < 254; i++ { // start at .100 for auto-assigned
					candidate := fmt.Sprintf("%d.%d.%d.%d", base[0], base[1], base[2], i)
					if !usedIPs[netName][candidate] {
						usedIPs[netName][candidate] = true
						return candidate
					}
				}
			}
		}
		return ""
	}

	// Helper: attach a service to a network
	attach := func(svc M, netName, ip string) {
		netName = cleanStr(netName)
		if netName == "" {
			netName = defaultNet()
		}
		if !netNames[netName] {
			return
		}
		nets, ok := svc["networks"].(M)
		if !ok {
			nets = M{}
			svc["networks"] = nets
		}
		if ip == "" {
			ip = nextFreeIP(netName)
		}
		if ip != "" {
			if usedIPs[netName] == nil {
				usedIPs[netName] = map[string]bool{}
			}
			usedIPs[netName][ip] = true
			nets[netName] = M{"ipv4_address": ip}
		} else {
			nets[netName] = M{}
		}
	}

	// Helper: add service
	add := func(name string, svc M) {
		services[name] = svc
	}

	// Helper: register volume
	vol := func(name string) {
		volumes[name] = M{}
	}

	// Helper: maybe allocate a port
	maybePort := func(cond bool, target, inner int) []string {
		if cond {
			hp := pt.getFreePort(target)
			return []string{fmt.Sprintf("%d:%d", hp, inner)}
		}
		return nil
	}

	// Index services by key. Open WebUI requires an Ollama backend on a shared
	// Docker network, so add Ollama automatically when only the web UI was asked.
	serviceEntries := professionalAIEntries(payload)
	hasOpenWebUI := false
	hasOllama := false
	hasGitea := false
	hasPostgres := false
	hasMoodle := false
	hasWordpress := false
	hasMariaDB := false
	hasMongoExpress := false
	hasMongo := false
	for _, s := range serviceEntries {
		if s.Key == "open-webui" {
			hasOpenWebUI = true
		}
		if s.Key == "ollama" {
			hasOllama = true
		}
		if s.Key == "gitea" {
			hasGitea = true
		}
		if s.Key == "postgres" {
			hasPostgres = true
		}
		if s.Key == "wordpress" {
			hasWordpress = true
		}
		if s.Key == "moodle" {
			hasMoodle = true
		}
		if s.Key == "mariadb" {
			hasMariaDB = true
		}
		if s.Key == "mongo-express" {
			hasMongoExpress = true
		}
		if s.Key == "mongo" {
			hasMongo = true
		}
	}
	if config, _ := professionalAIConfig(payload); hasOpenWebUI && !hasOllama && config.Engine != "vllm" {
		ollamaNet := defaultNet()
		if netNames["back"] {
			ollamaNet = "back"
		}
		serviceEntries = append(serviceEntries, ServiceEntry{Key: "ollama", Network: ollamaNet, Extra: map[string]string{"model": DefaultOllamaModel}})
	}
	if hasGitea && !hasPostgres {
		serviceEntries = append(serviceEntries, ServiceEntry{Key: "postgres", Network: "back"})
	}
	if (hasMoodle || hasWordpress) && !hasMariaDB {
		serviceEntries = append(serviceEntries, ServiceEntry{Key: "mariadb", Network: "back"})
	}
	if hasMongoExpress && !hasMongo {
		serviceEntries = append(serviceEntries, ServiceEntry{Key: "mongo", Network: "back"})
	}
	S := make(map[string]*ServiceEntry)
	for i := range serviceEntries {
		s := &serviceEntries[i]
		S[s.Key] = s
	}

	// Generate passwords
	dbRootPw := generatePassword(22)
	dbUserPw := generatePassword(16)
	dbUser := "app"
	postgresPw := generatePassword(22)

	ui := payload.UI

	// --- portainer ---
	if s, ok := S["portainer"]; ok {
		svc := M{
			"image":          GetImage("portainer"),
			"container_name": project + "-portainer",
			"restart":        "unless-stopped",
			"volumes":        []string{"/var/run/docker.sock:/var/run/docker.sock", "portainer_data:/data"},
		}
		if ports := maybePort(boolDefault(ui.Socle, true), 9443, 9443); len(ports) > 0 {
			svc["ports"] = ports
		}
		vol("portainer_data")
		attach(svc, s.Network, s.IP)
		add("portainer", svc)
	}

	// --- traefik ---
	if s, ok := S["traefik"]; ok {
		ports := maybePort(true, 80, 80)
		ports = append(ports, maybePort(boolDefault(ui.Socle, true), 8081, 8080)...)
		svc := M{
			"image":          GetImage("traefik"),
			"container_name": project + "-traefik",
			"restart":        "unless-stopped",
			"command":        []string{"--api.dashboard=true", "--providers.docker=true", "--entrypoints.web.address=:80"},
			"ports":          ports,
			"volumes":        []string{"/var/run/docker.sock:/var/run/docker.sock:ro"},
		}
		attach(svc, s.Network, s.IP)
		add("traefik", svc)
	}

	// --- nginx ---
	if s, ok := S["nginx"]; ok {
		svc := M{
			"image":          GetImage("nginx"),
			"container_name": project + "-nginx",
			"restart":        "unless-stopped",
		}
		if ports := maybePort(boolDefault(ui.Web, true), 8080, 80); len(ports) > 0 {
			svc["ports"] = ports
		}
		attach(svc, s.Network, s.IP)
		add("nginx", svc)
	}

	// --- wordpress ---
	if s, ok := S["wordpress"]; ok {
		svc := M{
			"image":          GetImage("wordpress"),
			"container_name": project + "-wordpress",
			"restart":        "unless-stopped",
			"environment": M{
				"WORDPRESS_DB_HOST":     "mariadb:3306",
				"WORDPRESS_DB_NAME":     "app",
				"WORDPRESS_DB_USER":     dbUser,
				"WORDPRESS_DB_PASSWORD": dbUserPw,
			},
			"depends_on": []string{"mariadb"},
			"volumes":    []string{"wordpress_data:/var/www/html"},
		}
		if ports := maybePort(boolDefault(ui.Web, true), 8082, 80); len(ports) > 0 {
			svc["ports"] = ports
		}
		vol("wordpress_data")
		attach(svc, s.Network, s.IP)
		add("wordpress", svc)
	}

	// --- moodle ---
	if s, ok := S["moodle"]; ok {
		svc := M{
			"image":          GetImage("moodle"),
			"container_name": project + "-moodle",
			"restart":        "unless-stopped",
			"environment": M{
				"MOODLE_DATABASE_HOST":     "mariadb",
				"MOODLE_DATABASE_NAME":     "app",
				"MOODLE_DATABASE_USER":     dbUser,
				"MOODLE_DATABASE_PASSWORD": dbUserPw,
				"MOODLE_USERNAME":          "admin",
				"MOODLE_PASSWORD":          dbUserPw,
				"MOODLE_EMAIL":             "admin@local",
			},
			"depends_on": []string{"mariadb"},
			"volumes":    []string{"moodle_data:/bitnami/moodle", "moodledata_data:/bitnami/moodledata"},
		}
		if ports := maybePort(boolDefault(ui.Web, true), 8086, 8080); len(ports) > 0 {
			svc["ports"] = ports
		}
		vol("moodle_data")
		vol("moodledata_data")
		attach(svc, s.Network, s.IP)
		add("moodle", svc)
	}

	// --- ollama ---
	if s, ok := S["ollama"]; ok {
		svc := M{
			"image":          GetImage("ollama"),
			"container_name": project + "-ollama",
			"restart":        "unless-stopped",
			"environment": M{
				"OLLAMA_HOST": "0.0.0.0:11434",
			},
			"volumes": []string{"ollama_data:/root/.ollama"},
			"healthcheck": M{
				"test":     []string{"CMD-SHELL", "OLLAMA_HOST=http://127.0.0.1:11434 ollama list >/dev/null 2>&1"},
				"interval": "10s",
				"timeout":  "5s",
				"retries":  12,
			},
		}
		if ports := maybePort(boolDefault(ui.AI, true), 11434, 11434); len(ports) > 0 {
			svc["ports"] = ports
		}
		vol("ollama_data")
		attach(svc, s.Network, s.IP)
		add("ollama", svc)
	}

	// --- open-webui ---
	if s, ok := S["open-webui"]; ok {
		model := DefaultOllamaModel
		if ollamaSvc, ok := S["ollama"]; ok && ollamaSvc.Extra != nil && strings.TrimSpace(ollamaSvc.Extra["model"]) != "" {
			model = strings.TrimSpace(ollamaSvc.Extra["model"])
		}
		if s.Extra != nil && strings.TrimSpace(s.Extra["model"]) != "" {
			model = strings.TrimSpace(s.Extra["model"])
		}

		svc := M{
			"image":          GetImage("open-webui"),
			"container_name": project + "-open-webui",
			"restart":        "unless-stopped",
			"depends_on":     []string{"ollama"},
			"environment": M{
				"OLLAMA_BASE_URL":          "http://ollama:11434",
				"WEBUI_AUTH":               "False",
				"ENABLE_LOGIN_FORM":        "False",
				"ENABLE_SIGNUP":            "False",
				"ENABLE_PERSISTENT_CONFIG": "False",
				"DEFAULT_LOCALE":           "fr",
				"DEFAULT_MODELS":           model,
				"DEFAULT_PINNED_MODELS":    model,
				"WEBUI_NAME":               "AutoDocker Local AI",
				"CUSTOM_NAME":              "AutoDocker Local AI",
			},
			"volumes": []string{"open_webui_data:/app/backend/data"},
		}
		if ports := maybePort(boolDefault(ui.AI, true), 3001, 8080); len(ports) > 0 {
			svc["ports"] = ports
		}
		vol("open_webui_data")
		attach(svc, s.Network, s.IP)
		if ollamaSvc, ok := S["ollama"]; ok && ollamaSvc.Network != "" && ollamaSvc.Network != s.Network {
			attach(svc, ollamaSvc.Network, "")
		}
		add("open-webui", svc)

		if _, hasInit := services["ollama-model-init"]; !hasInit {
			initSvc := M{
				"image":          GetImage("ollama"),
				"container_name": project + "-ollama-model-init",
				"restart":        "no",
				"depends_on": M{
					"ollama": M{"condition": "service_healthy"},
				},
				"environment": M{
					"OLLAMA_HOST":  "http://ollama:11434",
					"OLLAMA_MODEL": model,
				},
				"entrypoint": []string{"ollama"},
				"command":    []string{"pull", model},
			}
			if ollamaSvc, ok := S["ollama"]; ok {
				attach(initSvc, ollamaSvc.Network, "")
			} else {
				attach(initSvc, s.Network, "")
			}
			add("ollama-model-init", initSvc)
		}
	}

	// --- mariadb ---
	if s, ok := S["mariadb"]; ok {
		svc := M{
			"image":          GetImage("mariadb"),
			"container_name": project + "-mariadb",
			"restart":        "unless-stopped",
			"environment": M{
				"MARIADB_ROOT_PASSWORD": dbRootPw,
				"MARIADB_DATABASE":      "app",
				"MARIADB_USER":          dbUser,
				"MARIADB_PASSWORD":      dbUserPw,
			},
			"volumes": []string{"mariadb_data:/var/lib/mysql"},
		}
		vol("mariadb_data")
		attach(svc, s.Network, s.IP)
		add("mariadb", svc)

		// Auto-add adminer if DB UI requested
		if boolDefault(ui.DB, false) {
			adm := M{
				"image":          GetImage("adminer"),
				"container_name": project + "-adminer",
				"restart":        "unless-stopped",
			}
			if ports := maybePort(true, 8085, 8080); len(ports) > 0 {
				adm["ports"] = ports
			}
			attach(adm, s.Network, "")
			add("adminer", adm)
		}
	}

	// --- postgres ---
	if s, ok := S["postgres"]; ok {
		svc := M{
			"image":          GetImage("postgres"),
			"container_name": project + "-postgres",
			"restart":        "unless-stopped",
			"environment": M{
				"POSTGRES_PASSWORD": postgresPw,
				"POSTGRES_USER":     "postgres",
				"POSTGRES_DB":       "app",
			},
			"volumes": []string{"pg_data:/var/lib/postgresql/data"},
		}
		vol("pg_data")
		attach(svc, s.Network, s.IP)
		add("postgres", svc)

		if boolDefault(ui.DB, false) {
			pgadminPw := generatePassword(16)
			pga := M{
				"image":          GetImage("pgadmin"),
				"container_name": project + "-pgadmin",
				"restart":        "unless-stopped",
				"environment": M{
					"PGADMIN_DEFAULT_EMAIL":    "admin@local",
					"PGADMIN_DEFAULT_PASSWORD": pgadminPw,
				},
			}
			if ports := maybePort(true, 5050, 80); len(ports) > 0 {
				pga["ports"] = ports
			}
			attach(pga, s.Network, "")
			add("pgadmin", pga)
		}
	}

	// --- gitea ---
	if s, ok := S["gitea"]; ok {
		svc := M{
			"image":          GetImage("gitea"),
			"container_name": project + "-gitea",
			"restart":        "unless-stopped",
			"environment": M{
				"GITEA__database__DB_TYPE": "postgres",
				"GITEA__database__HOST":    "postgres:5432",
				"GITEA__database__NAME":    "app",
				"GITEA__database__USER":    "postgres",
				"GITEA__database__PASSWD":  postgresPw,
			},
			"depends_on": []string{"postgres"},
			"volumes":    []string{"gitea_data:/data"},
		}
		if ports := maybePort(boolDefault(ui.Web, true), 3002, 3000); len(ports) > 0 {
			svc["ports"] = ports
		}
		vol("gitea_data")
		attach(svc, s.Network, s.IP)
		if pgSvc, ok := S["postgres"]; ok && pgSvc.Network != "" && pgSvc.Network != s.Network {
			attach(svc, pgSvc.Network, "")
		}
		add("gitea", svc)
	}

	// --- n8n ---
	if s, ok := S["n8n"]; ok {
		svc := M{
			"image":          GetImage("n8n"),
			"container_name": project + "-n8n",
			"restart":        "unless-stopped",
			"environment": M{
				"N8N_SECURE_COOKIE":                     "false",
				"N8N_DIAGNOSTICS_ENABLED":               "false",
				"N8N_PERSONALIZATION_ENABLED":           "false",
				"N8N_ENFORCE_SETTINGS_FILE_PERMISSIONS": "true",
			},
			"volumes": []string{"n8n_data:/home/node/.n8n"},
		}
		if ports := maybePort(boolDefault(ui.Web, true), 5678, 5678); len(ports) > 0 {
			svc["ports"] = ports
		}
		vol("n8n_data")
		attach(svc, s.Network, s.IP)
		add("n8n", svc)
	}

	// --- uptime-kuma ---
	if s, ok := S["uptime-kuma"]; ok {
		svc := M{
			"image":          GetImage("uptime-kuma"),
			"container_name": project + "-uptime-kuma",
			"restart":        "unless-stopped",
			"volumes":        []string{"uptime_kuma_data:/app/data"},
		}
		if ports := maybePort(boolDefault(ui.Observ, true), 3003, 3001); len(ports) > 0 {
			svc["ports"] = ports
		}
		vol("uptime_kuma_data")
		attach(svc, s.Network, s.IP)
		add("uptime-kuma", svc)
	}

	// --- minio ---
	if s, ok := S["minio"]; ok {
		minioSecret := generatePassword(24)
		svc := M{
			"image":          GetImage("minio"),
			"container_name": project + "-minio",
			"restart":        "unless-stopped",
			"environment": M{
				"MINIO_ROOT_USER":     "autodocker",
				"MINIO_ROOT_PASSWORD": minioSecret,
			},
			"command": []string{"server", "/data", "--console-address", ":9001"},
			"volumes": []string{"minio_data:/data"},
		}
		ports := maybePort(boolDefault(ui.Web, true), 9000, 9000)
		ports = append(ports, maybePort(boolDefault(ui.Web, true), 9001, 9001)...)
		if len(ports) > 0 {
			svc["ports"] = ports
		}
		vol("minio_data")
		attach(svc, s.Network, s.IP)
		add("minio", svc)
	}

	// --- redis ---
	if s, ok := S["redis"]; ok {
		svc := M{
			"image":          GetImage("redis"),
			"container_name": project + "-redis",
			"restart":        "unless-stopped",
			"volumes":        []string{"redis_data:/data"},
		}
		if ports := maybePort(boolDefault(ui.DB, false), 6379, 6379); len(ports) > 0 {
			svc["ports"] = ports
		}
		vol("redis_data")
		attach(svc, s.Network, s.IP)
		add("redis", svc)
	}

	// --- mongo ---
	if s, ok := S["mongo"]; ok {
		mongoPw := generatePassword(18)
		svc := M{
			"image":          GetImage("mongo"),
			"container_name": project + "-mongo",
			"restart":        "unless-stopped",
			"environment": M{
				"MONGO_INITDB_ROOT_USERNAME": "autodocker",
				"MONGO_INITDB_ROOT_PASSWORD": mongoPw,
			},
			"volumes": []string{"mongo_data:/data/db"},
		}
		if ports := maybePort(boolDefault(ui.DB, false), 27017, 27017); len(ports) > 0 {
			svc["ports"] = ports
		}
		vol("mongo_data")
		attach(svc, s.Network, s.IP)
		add("mongo", svc)
	}

	// --- mongo-express ---
	if s, ok := S["mongo-express"]; ok {
		svc := M{
			"image":          GetImage("mongo-express"),
			"container_name": project + "-mongo-express",
			"restart":        "unless-stopped",
			"environment": M{
				"ME_CONFIG_MONGODB_SERVER": "mongo",
				"ME_CONFIG_BASICAUTH":      "false",
			},
			"depends_on": []string{"mongo"},
		}
		if ports := maybePort(boolDefault(ui.DB, true), 8087, 8081); len(ports) > 0 {
			svc["ports"] = ports
		}
		attach(svc, s.Network, s.IP)
		if mongoSvc, ok := S["mongo"]; ok && mongoSvc.Network != "" && mongoSvc.Network != s.Network {
			attach(svc, mongoSvc.Network, "")
		}
		add("mongo-express", svc)
	}

	// --- nextcloud ---
	if s, ok := S["nextcloud"]; ok {
		ncPw := generatePassword(16)
		ncRootPw := generatePassword(22)
		_, hasMariaDB := services["mariadb"]

		env := M{}
		if hasMariaDB {
			env["MYSQL_HOST"] = "mariadb"
			env["MYSQL_DATABASE"] = "app"
			env["MYSQL_USER"] = dbUser
			env["MYSQL_PASSWORD"] = dbUserPw
		} else {
			env["MYSQL_HOST"] = "nextcloud-db"
			env["MYSQL_DATABASE"] = "nextcloud"
			env["MYSQL_USER"] = "nextcloud"
			env["MYSQL_PASSWORD"] = ncPw
		}

		svc := M{
			"image":          GetImage("nextcloud"),
			"container_name": project + "-nextcloud",
			"restart":        "unless-stopped",
			"environment":    env,
			"volumes":        []string{"nextcloud_data:/var/www/html"},
		}
		if ports := maybePort(boolDefault(ui.Collab, true), 8083, 80); len(ports) > 0 {
			svc["ports"] = ports
		}
		if hasMariaDB {
			svc["depends_on"] = []string{"mariadb"}
		}
		vol("nextcloud_data")
		attach(svc, s.Network, s.IP)
		add("nextcloud", svc)

		// Spin up a dedicated mariadb if none exists
		if !hasMariaDB {
			mdb := M{
				"image":          GetImage("mariadb"),
				"container_name": project + "-nextcloud-db",
				"restart":        "unless-stopped",
				"environment": M{
					"MARIADB_ROOT_PASSWORD": ncRootPw,
					"MARIADB_DATABASE":      "nextcloud",
					"MARIADB_USER":          "nextcloud",
					"MARIADB_PASSWORD":      ncPw,
				},
				"volumes": []string{"nextcloud_db_data:/var/lib/mysql"},
			}
			vol("nextcloud_db_data")
			attach(mdb, s.Network, "")
			add("nextcloud-db", mdb)
			svc["depends_on"] = []string{"nextcloud-db"}
		}
	}

	// --- dokuwiki ---
	if s, ok := S["dokuwiki"]; ok {
		svc := M{
			"image":          GetImage("dokuwiki"),
			"container_name": project + "-dokuwiki",
			"restart":        "unless-stopped",
			"volumes":        []string{"dokuwiki_data:/storage"},
		}
		if ports := maybePort(boolDefault(ui.Collab, true), 8090, 8080); len(ports) > 0 {
			svc["ports"] = ports
		}
		vol("dokuwiki_data")
		attach(svc, s.Network, s.IP)
		add("dokuwiki", svc)
	}

	// --- samba ---
	if s, ok := S["samba"]; ok {
		sambaPw := generatePassword(16)
		ports := maybePort(true, 445, 445)
		ports = append(ports, maybePort(true, 139, 139)...)
		svc := M{
			"image":          GetImage("samba"),
			"container_name": project + "-samba",
			"restart":        "unless-stopped",
			"environment": M{
				"USER":  fmt.Sprintf("user;%s", sambaPw),
				"SHARE": "public;/share;yes;no;no;user",
			},
			"ports":   ports,
			"volumes": []string{"samba_share:/share"},
		}
		vol("samba_share")
		attach(svc, s.Network, s.IP)
		add("samba", svc)
	}

	// --- grafana ---
	if s, ok := S["grafana"]; ok {
		svc := M{
			"image":          GetImage("grafana"),
			"container_name": project + "-grafana",
			"restart":        "unless-stopped",
			"volumes":        []string{"grafana_data:/var/lib/grafana"},
		}
		if ports := maybePort(boolDefault(ui.Observ, true), 3000, 3000); len(ports) > 0 {
			svc["ports"] = ports
		}
		vol("grafana_data")
		attach(svc, s.Network, s.IP)
		add("grafana", svc)
	}

	// --- prometheus ---
	if s, ok := S["prometheus"]; ok {
		svc := M{
			"image":          GetImage("prometheus"),
			"container_name": project + "-prometheus",
			"restart":        "unless-stopped",
		}
		if ports := maybePort(boolDefault(ui.Observ, true), 9090, 9090); len(ports) > 0 {
			svc["ports"] = ports
		}
		attach(svc, s.Network, s.IP)
		add("prometheus", svc)
	}

	// --- fail2ban ---
	if _, ok := S["fail2ban"]; ok {
		svc := M{
			"image":          GetImage("fail2ban"),
			"container_name": project + "-fail2ban",
			"restart":        "unless-stopped",
			"cap_add":        []string{"NET_ADMIN", "NET_RAW"},
			"network_mode":   "host",
			"environment": M{
				"TZ": "Europe/Paris",
			},
			"volumes": []string{
				"./auth.log:/var/log/auth.log:rw",
				"/var/run/docker.sock:/var/run/docker.sock:ro",
				"./jail.local:/etc/fail2ban/jail.local:ro",
			},
		}
		add("fail2ban", svc)
	}

	// --- debian-router ---
	if s, ok := S["debian-router"]; ok {
		svc := M{
			"image":          GetImage("debian"),
			"container_name": project + "-router",
			"restart":        "unless-stopped",
			"cap_add":        []string{"NET_ADMIN", "NET_RAW"},
			"cap_drop":       []string{"ALL"},
			"sysctls": M{
				"net.ipv4.ip_forward": "1",
			},
			"command":  []string{"/bin/sh", "-c", "bash /usr/local/bin/bootstrap-router.sh; echo 'Script done'; tail -f /dev/null"},
			"volumes":  []string{"./router/bootstrap-router.sh:/usr/local/bin/bootstrap-router.sh:ro"},
			"networks": M{},
		}
		routerNets := svc["networks"].(M)
		if s.LAN != nil && s.LAN.Network != "" {
			entry := M{}
			if s.LAN.IP != "" {
				entry["ipv4_address"] = s.LAN.IP
			}
			routerNets[s.LAN.Network] = entry
		}
		if s.WAN != nil && s.WAN.Network != "" {
			entry := M{}
			if s.WAN.IP != "" {
				entry["ipv4_address"] = s.WAN.IP
			}
			routerNets[s.WAN.Network] = entry
		}
		add("debian-router", svc)
	}

	// --- Generic/custom/freeform services ---
	handledKeys := map[string]bool{
		"portainer": true, "traefik": true, "nginx": true, "wordpress": true, "moodle": true,
		"ollama": true, "open-webui": true, "gitea": true, "n8n": true, "uptime-kuma": true,
		"minio": true, "redis": true, "mongo": true, "mongo-express": true,
		"mariadb": true, "postgres": true, "nextcloud": true, "dokuwiki": true, "samba": true,
		"grafana": true, "prometheus": true, "fail2ban": true, "debian-router": true,
	}
	// One shared password per deployment so an app and its DB stay in sync.
	autoPw := generatePassword(20)
	// Freeform applications can use the same generated application credential
	// as the sole built-in database, instead of receiving an unrelated password.
	if S["postgres"] != nil && S["mariadb"] == nil {
		autoPw = postgresPw
	}
	if S["mariadb"] != nil && S["postgres"] == nil {
		autoPw = dbUserPw
	}
	knownKeys := make(map[string]bool, len(S))
	for key := range S {
		knownKeys[key] = true
	}
	for key, s := range S {
		if handledKeys[key] || professionalAIImages[key] != "" {
			continue
		}
		if _, already := services[key]; already {
			continue
		}
		svc, volNames, ok := buildCustomService(project, s, pt, knownKeys, autoPw)
		if !ok {
			continue
		}
		for _, v := range volNames {
			vol(v)
		}
		attach(svc, s.Network, s.IP)
		add(key, svc)
	}

	applyBuiltinInfrastructureOverrides(services, S, handledKeys, volumes, pt, autoPw)
	applySelectedRuntimeModel(compose, S)
	applyProfessionalAIStack(compose, payload, S, pt)
	applyInfrastructureIngress(compose, payload, S, pt)
	applyClientDelivery(compose, payload)
	applyApplicationConnections(compose)
	return compose
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func buildIPAM(networks []NetworkDef) M {
	if len(networks) == 0 {
		return M{"default": M{"driver": "bridge"}}
	}
	out := M{}
	for _, n := range networks {
		out[n.Name] = M{
			"driver": "bridge",
			"ipam": M{
				"config": []M{
					{"subnet": n.CIDR, "gateway": n.Gateway},
				},
			},
		}
	}
	return out
}

var cleanRe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func cleanStr(s string) string {
	return cleanRe.ReplaceAllString(strings.TrimSpace(s), "")
}

// ---------------------------------------------------------------------------
// Canvas Import: reverse-engineer compose.yaml into canvas nodes + links
// ---------------------------------------------------------------------------

// CanvasNode represents a node on the visual canvas.
type CanvasNode struct {
	ID       string            `json:"id"`
	Key      string            `json:"key"`
	Label    string            `json:"label"`
	Category string            `json:"category"`
	Type     string            `json:"type"`
	X        int               `json:"x"`
	Y        int               `json:"y"`
	IP       string            `json:"ip,omitempty"`
	CIDR     string            `json:"cidr,omitempty"`
	Gateway  string            `json:"gateway,omitempty"`
	Ports    []string          `json:"ports"`
	PortMap  map[string]string `json:"portMap"`
	Envs     map[string]string `json:"envs"`
}

// CanvasLink represents a cable/connection between two canvas nodes.
type CanvasLink struct {
	ID          string `json:"id"`
	Source      string `json:"source"`
	Target      string `json:"target"`
	SourceIface string `json:"sourceIface"`
	TargetIface string `json:"targetIface"`
	CableType   string `json:"cableType"`
}

// ReverseComposeToCanvas takes a pre-parsed compose map and generates canvas nodes + links.
func ReverseComposeToCanvas(compose map[string]interface{}, project string) ([]CanvasNode, []CanvasLink, error) {
	nodes := []CanvasNode{}
	links := []CanvasLink{}
	idCounter := 0
	nextID := func() string { idCounter++; return fmt.Sprintf("imp_%d", idCounter) }

	// 1. Parse networks → network nodes
	netNodeIDs := map[string]string{} // networkName → nodeID
	if netSection, ok := compose["networks"]; ok {
		if netMap, ok := netSection.(map[string]interface{}); ok {
			col := 0
			for netName, netCfg := range netMap {
				nid := nextID()
				node := CanvasNode{
					ID: nid, Key: netName, Label: "Switch " + netName,
					Category: "network", Type: "network",
					X: 200 + col*250, Y: 100,
					Ports: []string{}, PortMap: map[string]string{}, Envs: map[string]string{},
				}
				// Extract CIDR/gateway from IPAM
				if cfg, ok := netCfg.(map[string]interface{}); ok {
					if ipam, ok := cfg["ipam"].(map[string]interface{}); ok {
						if configs, ok := ipam["config"].([]interface{}); ok && len(configs) > 0 {
							if c0, ok := configs[0].(map[string]interface{}); ok {
								node.CIDR, _ = c0["subnet"].(string)
								node.Gateway, _ = c0["gateway"].(string)
							}
						}
					}
				}
				nodes = append(nodes, node)
				netNodeIDs[netName] = nid
				col++
			}
		}
	}

	// 2. Parse services → service nodes + links
	if svcSection, ok := compose["services"]; ok {
		if svcMap, ok := svcSection.(map[string]interface{}); ok {
			row := 0
			for svcName, svcCfg := range svcMap {
				cfg, ok := svcCfg.(map[string]interface{})
				if !ok {
					continue
				}

				nid := nextID()
				image, _ := cfg["image"].(string)
				key := fuzzyMatchImage(image, svcName)
				category := CategoryMap[key]
				if category == "" {
					category = "custom"
				}

				node := CanvasNode{
					ID: nid, Key: key, Label: svcName,
					Category: category, Type: "service",
					X: 150 + (row%4)*200, Y: 250 + (row/4)*140,
					Ports: []string{}, PortMap: map[string]string{}, Envs: map[string]string{},
				}

				// Extract ports
				if ports, ok := cfg["ports"].([]interface{}); ok {
					for _, p := range ports {
						ps := fmt.Sprintf("%v", p)
						parts := strings.SplitN(ps, ":", 2)
						if len(parts) == 2 {
							node.PortMap[parts[0]] = parts[1]
							node.Ports = append(node.Ports, parts[1])
						}
					}
				}

				// Extract network connections + IP
				if nets, ok := cfg["networks"].(map[string]interface{}); ok {
					ifaceIdx := 0
					for netName, netCfg := range nets {
						if netNodeID, exists := netNodeIDs[netName]; exists {
							lid := nextID()
							links = append(links, CanvasLink{
								ID: lid, Source: nid, Target: netNodeID,
								SourceIface: fmt.Sprintf("eth%d", ifaceIdx),
								TargetIface: fmt.Sprintf("fa0/%d", len(links)%8),
								CableType:   "auto",
							})
							// Extract ipv4_address
							if nc, ok := netCfg.(map[string]interface{}); ok {
								if ip, ok := nc["ipv4_address"].(string); ok {
									node.IP = ip
								}
							}
						}
						ifaceIdx++
					}
				}

				nodes = append(nodes, node)
				row++
			}
		}
	}

	return nodes, links, nil
}

// fuzzyMatchImage maps a Docker image string to a catalogue key.
func fuzzyMatchImage(image, svcName string) string {
	image = strings.ToLower(image)
	for key := range ImagesBase {
		if strings.Contains(image, key) {
			return key
		}
	}
	customMu.RLock()
	defer customMu.RUnlock()
	for key := range Images {
		if strings.Contains(image, key) {
			return key
		}
	}
	svcName = strings.ToLower(svcName)
	for key := range ImagesBase {
		if strings.Contains(svcName, key) {
			return key
		}
	}
	return svcName
}
