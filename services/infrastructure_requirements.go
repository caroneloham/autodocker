package services

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

func applyExplicitInfrastructureSetting(svc *ServiceEntry, setting infrastructureSetting, catalogue map[string]string) []string {
	warnings := []string{}
	if setting.Version != "" {
		image := svc.Image
		if image == "" {
			image = catalogue[svc.Key]
		}
		tag := ""
		if at := strings.LastIndex(image, ":"); at >= 0 {
			tag = image[at+1:]
		}
		if tag != setting.Version && !strings.HasPrefix(tag, setting.Version+".") && !strings.HasPrefix(tag, setting.Version+"-") {
			base := catalogue[svc.Key]
			if base == "" {
				base = image
			}
			if at := strings.LastIndex(base, ":"); at > strings.LastIndex(base, "/") {
				base = base[:at]
			}
			if base != "" {
				svc.Image = base + ":" + setting.Version
				warnings = append(warnings, fmt.Sprintf("%s : version %s appliquée à votre demande.", svc.Key, setting.Version))
			}
		}
	}
	if setting.HostPort > 0 {
		container := map[string]int{"caddy": 80, "traefik": 80, "haproxy": 80, "wordpress": 80, "nginx": 80, "nextcloud": 80, "grafana": 3000, "postgres": 5432, "mariadb": 3306, "redis": 6379, "n8n": 5678, "prometheus": 9090, "loki": 3100, "portainer": 9443, "gitea": 3000, "minio": 9001, "ollama": 11434, "open-webui": 8080, "uptime-kuma": 3001}[svc.Key]
		if container == 0 && len(svc.Ports) == 1 {
			_, container, _ = sanitizePortSpec(svc.Ports[0])
		}
		if container > 0 {
			updated := []string{}
			for _, port := range svc.Ports {
				_, target, ok := sanitizePortSpec(port)
				if ok && target != container {
					updated = append(updated, port)
				}
			}
			requested := fmt.Sprintf("%d:%d", setting.HostPort, container)
			updated = append(updated, requested)
			if !containsInfrastructureString(svc.Ports, requested) {
				warnings = append(warnings, fmt.Sprintf("%s : port hôte %d appliqué à votre demande (réattribué s'il est occupé).", svc.Key, setting.HostPort))
			}
			svc.Ports = updated
		}
	}
	return warnings
}

// Small, explicit constraints supplement the model; they do not select an
// architecture by guessing a single intent for the complete request.
type infrastructureSetting struct {
	Version  string
	HostPort int
}

func infrastructureExplicitSettings(prompt string, catalogue map[string]string) map[string]infrastructureSetting {
	prompt = strings.ToLower(prompt)
	aliases := map[string]string{"postgresql": "postgres", "graphana": "grafana", "mongodb": "mongo"}
	for key := range catalogue {
		aliases[key] = key
	}
	result := map[string]infrastructureSetting{}
	for alias, key := range aliases {
		r := regexp.MustCompile(`\b` + regexp.QuoteMeta(alias) + `\s+(?:version\s+|v)?([0-9]+(?:\.[0-9]+){0,3}(?:-[a-z0-9._-]+)?)\b`)
		for _, match := range r.FindAllStringSubmatch(prompt, -1) {
			setting := result[key]
			setting.Version = match[1]
			result[key] = setting
		}
	}
	ports := regexp.MustCompile(`\bport\s+([0-9]{2,5})\b`)
	for _, match := range ports.FindAllStringSubmatchIndex(prompt, -1) {
		closest, key := -1, ""
		clauseStart := 0
		for _, boundary := range regexp.MustCompile(`[.!?;](?:\s+|$)|[\r\n]+`).FindAllStringIndex(prompt[:match[0]], -1) {
			clauseStart = boundary[1]
		}
		for alias, canonical := range aliases {
			r := regexp.MustCompile(`\b` + regexp.QuoteMeta(alias) + `\b`)
			for _, name := range r.FindAllStringIndex(prompt[:match[0]], -1) {
				if name[0] >= clauseStart && name[1] > closest && match[0]-name[1] < 100 {
					closest, key = name[1], canonical
				}
			}
		}
		if key != "" {
			port, _ := strconv.Atoi(prompt[match[2]:match[3]])
			if port > 0 && port <= 65535 {
				setting := result[key]
				setting.HostPort = port
				result[key] = setting
			}
		}
	}
	return result
}

func infrastructurePreflightQuestion(prompt string) string {
	words := infrastructureWords(prompt)
	if minecraftAIConnectionRequested(prompt) && !minecraftAIChatRequested(prompt) {
		return "Souhaitez-vous un assistant qui répond aux joueurs dans le chat Minecraft Java, ou un bot qui se déplace et agit dans le monde ? Le profil intégré prépare l’assistant de chat ; un bot joueur demande une intégration différente."
	}
	if mentionedInfrastructureServices(prompt, map[string]string{"moodle": "bitnami/moodle:latest"})["moodle"] {
		return "Le profil Moodle est temporairement indisponible car son image de référence n’existe plus. Voulez-vous préparer une autre infrastructure en attendant un profil maintenu ?"
	}
	if !strings.Contains(words, "jeu video") && !strings.Contains(words, "jeux video") && !strings.Contains(words, "serveur de jeu") {
		return ""
	}
	for _, specific := range []string{"minecraft", "valheim", "terraria", "factorio", "palworld", "rust", "counter strike", "navigateur", "browser", "unreal", "unity", "html", "web", "precisions", "precision"} {
		if strings.Contains(words, specific) {
			return ""
		}
	}
	return "Souhaitez-vous un jeu dans le navigateur ou un serveur pour un jeu existant ? Dans ce dernier cas, lequel ?"
}

// InfrastructurePreflightQuestion returns a clarification that requires no AI
// provider call. Public entry points can answer it before spending an AI quota.
func InfrastructurePreflightQuestion(prompt string) string {
	return infrastructurePreflightQuestion(prompt)
}

// Existing UI catalogue specifications, plus a known log backend. These are
// grounding data, not extra services to insert into every proposal.
var infrastructureReferenceServices = map[string]ServiceEntry{
	"minecraft":   {Key: "minecraft", Image: "itzg/minecraft-server:latest", Ports: []string{"25565:25565"}, Volumes: []string{"minecraft_data:/data"}, Env: map[string]string{"EULA": "FALSE"}},
	"loki":        {Key: "loki", Image: "grafana/loki:3.7.0", Ports: []string{"3100:3100"}},
	"caddy":       {Key: "caddy", Image: "caddy:2-alpine", Ports: []string{"8088:80"}, Volumes: []string{"caddy_data:/data", "caddy_config:/config"}},
	"rabbitmq":    {Key: "rabbitmq", Image: "rabbitmq:4-management", Ports: []string{"5672:5672", "15672:15672"}, Env: map[string]string{"RABBITMQ_DEFAULT_USER": "autodocker", "RABBITMQ_DEFAULT_PASS": AutoPasswordPlaceholder}, Volumes: []string{"rabbitmq_data:/var/lib/rabbitmq"}},
	"memcached":   {Key: "memcached", Image: "memcached:1.6-alpine", Ports: []string{"11211:11211"}},
	"meilisearch": {Key: "meilisearch", Image: "getmeili/meilisearch:v1.20", Ports: []string{"7700:7700"}, Env: map[string]string{"MEILI_ENV": "production", "MEILI_MASTER_KEY": AutoPasswordPlaceholder}, Volumes: []string{"meili_data:/meili_data"}},
}

func infrastructureCatalogue() map[string]string {
	catalogue := GetAllImages()
	delete(catalogue, "moodle")
	// Pro profiles must remain grounded even on a fresh installation without
	// a user-maintained catalogue JSON file.
	for key, image := range professionalAIImages {
		catalogue[key] = image
	}
	if catalogue["qdrant"] == "" {
		catalogue["qdrant"] = "qdrant/qdrant:v1.15"
	}
	for key, entry := range infrastructureReferenceServices {
		if catalogue[key] == "" {
			catalogue[key] = entry.Image
		}
	}
	return catalogue
}
