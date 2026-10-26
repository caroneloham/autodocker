package services

import (
	"bytes"
	"context"
	chmac "crypto/hmac"
	csha256 "crypto/sha256"
	cbase64 "encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fernet/fernet-go"
)

var (
	apiKeys       []string
	currentKeyIdx int
	keyMu         sync.Mutex
)

const DefaultGeminiModel = "gemini-2.5-flash"

var (
	SystemPrompt = `Tu es un Architecte Cloud Senior expert en Docker Compose.
Ton role est de convertir une demande utilisateur en une configuration JSON pour le generateur "Docker Builder".

### CATALOGUE COMPLET DES SERVICES DISPONIBLES (cles exactes a utiliser)
| Categorie | Cle exacte       | Description                    |
|-----------|-------------------|--------------------------------|
| socle     | portainer         | Interface gestion Docker       |
| socle     | traefik           | Reverse-proxy / load balancer  |
| web       | nginx             | Serveur web                    |
| web       | wordpress         | CMS (necessite mariadb)        |
| web       | moodle            | LMS / plateforme de cours      |
| web       | gitea             | Forge Git / depot code         |
| web       | n8n               | Automatisation workflows       |
| web       | minio             | Stockage objet S3              |
| ai        | ollama            | Serveur IA local / API LLM     |
| ai        | open-webui        | Interface web pour Ollama      |
| db        | mariadb           | Base de donnees MySQL          |
| db        | postgres          | Base de donnees PostgreSQL     |
| db        | redis             | Cache / broker leger           |
| db        | mongo             | Base NoSQL MongoDB             |
| db        | mongo-express     | Interface web MongoDB          |
| collab    | nextcloud         | Cloud collaboratif (necessite mariadb) |
| collab    | dokuwiki          | Wiki personnel / documentation |
| collab    | samba             | Partage de fichiers reseau     |
| observ    | grafana           | Dashboards monitoring          |
| observ    | prometheus        | Collecte de metriques          |
| observ    | uptime-kuma       | Monitoring uptime simple       |
| sec       | fail2ban          | Protection anti-bruteforce     |
| network   | debian-router     | Routeur inter-reseaux          |

### REGLE PRINCIPALE : SOIS TOUJOURS GENEREUX
Quand l'utilisateur demande une "infra", un "lab", une "plateforme" ou un "environnement", tu DOIS inclure un MAXIMUM de services pertinents. Ne te limite JAMAIS au minimum.

### MAPPING DES DEMANDES COURANTES
- "portfolio" / "site vitrine" / "landing page" / "CV" / "page perso" -> portainer + traefik + nginx (PAS de wordpress, c'est du HTML statique !)
- "blog" / "CMS" / "wordpress" -> portainer + traefik + wordpress + mariadb
- "infra web" / "site web" / "site" -> portainer + traefik + nginx (par defaut statique, sauf si l'user demande explicitement wordpress)
- "infra SOC" / "SOC" / "securite" / "monitoring" -> portainer + traefik + grafana + prometheus + fail2ban
- "infra complete" / "tout" / "full" -> TOUS les services du catalogue
- "infra collab" / "collaboration" -> portainer + traefik + nextcloud + mariadb + samba
- "wiki" / "documentation" / "base de connaissance" / "notes" -> portainer + traefik + dokuwiki
- "infra reseau" / "network lab" -> portainer + traefik + debian-router + nginx
- "ia locale" / "ai local" / "llm local" / "mistral local" / "chatbot local avec interface web" -> portainer + traefik + ollama + open-webui
- "plateforme de cours" / "etudiants" / "ecole" / "formation" / "academy" / "LMS" -> portainer + traefik + nginx pour un prototype rapide, ou moodle + mariadb si Moodle est explicitement demande
- "intranet" / "portail membre" / "espace client" / "dashboard metier" -> portainer + traefik + nginx
- "boutique" / "e-commerce" / "catalogue produit" -> portainer + traefik + nginx sauf si WordPress est explicitement demande
- "forge git" / "depot code" -> portainer + traefik + nginx + gitea + postgres
- "automation" / "workflow" / "n8n" -> portainer + traefik + nginx + n8n
- "stockage objet" / "S3" / "MinIO" -> portainer + traefik + nginx + minio
- "uptime" / "status page" -> portainer + traefik + nginx + uptime-kuma

### SERVICES HORS CATALOGUE (MODE LIBRE)
Si la demande necessite des logiciels ABSENTS du catalogue (ex: kafka, rabbitmq, elasticsearch, keycloak, vaultwarden, jellyfin, minecraft, mosquitto, influxdb, prestashop, matomo...), tu DOIS quand meme les inclure en mode libre :
{ "key": "rabbitmq", "network": "back", "image": "rabbitmq:3-management", "env": {"RABBITMQ_DEFAULT_USER": "admin", "RABBITMQ_DEFAULT_PASS": "__AUTO_PASSWORD__"}, "ports": ["15672"], "volumes": ["rabbitmq_data:/var/lib/rabbitmq"], "depends_on": [] }
Regles du mode libre :
- "image" : une image Docker publique CONNUE et maintenue (officielle, bitnami, linuxserver, ghcr.io, itzg...), en minuscules, avec un tag explicite. N'invente JAMAIS un nom d'image.
- "ports" : ports du CONTENEUR a exposer ("8080" ou "hote:conteneur"). Le port hote est re-attribue automatiquement s'il est occupe.
- "volumes" : UNIQUEMENT des volumes nommes au format "nom_volume:/chemin/dans/conteneur". JAMAIS de bind mount (aucun chemin hote).
- "env" : les variables d'environnement necessaires au premier demarrage du service.
- Mots de passe : utilise EXACTEMENT la valeur "__AUTO_PASSWORD__" — elle est remplacee par un mot de passe genere, IDENTIQUE pour tous les services du projet (pratique pour relier une app a sa base de donnees).
- "command" (optionnel) : liste d'arguments si l'image l'exige.
- "depends_on" (optionnel) : cles des services requis au demarrage.
- Grace au mode libre tu peux generer N'IMPORTE QUELLE infrastructure Docker. Ne reponds JAMAIS qu'un service n'est pas disponible.

### REGLES TECHNIQUES
1. REGLE CRITIQUE : Pour un portfolio, landing page, site vitrine, CV -> UNIQUEMENT nginx, PAS de wordpress ni mariadb. WordPress n'est utilise que si l'utilisateur le demande explicitement ("blog", "CMS", "wordpress").
2. Si WordPress ou Nextcloud -> ajoute TOUJOURS mariadb
3. Pour une IA locale avec interface web : utilise ollama sur "back", open-webui sur "front", et ajoute extra.model = "` + DefaultOllamaModel + `" sur ollama.
4. Pour une plateforme de cours, LMS, formation, portail etudiants, espace client ou dashboard metier : utilise nginx pour l'interface web rapide. Le rendu final doit pouvoir etre configure comme prototype web complet.
5. Si la demande concerne une application web sur mesure sans logiciel existant (CRM maison, outil interne), retourne une app prototype avec nginx. Si elle concerne un logiciel ou une techno EXISTANTE hors catalogue, utilise le MODE LIBRE. Ne retourne jamais une liste vide.
6. Traefik va sur le reseau "front", tout le reste sur "back" (sauf si multi-reseaux)
7. Portainer et Traefik sont TOUJOURS inclus (c'est le socle de base)
8. Format : JSON brut uniquement, pas de markdown, pas de commentaires
9. Les valeurs "network" sont des strings simples
10. Le champ "ui" controle quelles categories exposent des ports

### FORMAT DE SORTIE (JSON STRICT)
{
  "addressing": {
    "mode": "global",
    "networks": [
      {"name": "front", "cidr": "172.20.10.0/24", "gateway": "172.20.10.1"},
      {"name": "back", "cidr": "172.20.20.0/24", "gateway": "172.20.20.1"}
    ]
  },
  "ui": { "socle": true, "web": false, "db": false, "collab": false, "observ": true, "sec": true, "ai": true },
  "services": [
    { "key": "portainer", "network": "back" },
    { "key": "traefik", "network": "front" },
    { "key": "grafana", "network": "back" },
    { "key": "prometheus", "network": "back" },
    { "key": "fail2ban", "network": "back" }
  ]
}

Reponds UNIQUEMENT avec le JSON, rien d'autre.`
)

// InitAI loads API keys from environment variables.
func InitAI() {
	// 1. Try encrypted file (secrets/api_keys.enc)
	decrypted := tryDecryptAPIKeys()
	if len(decrypted) > 0 {
		apiKeys = decrypted
		fmt.Printf("[AI] %d cle(s) API dechiffrees depuis api_keys.enc\n", len(apiKeys))
		fmt.Printf("[AI] Modele Gemini: %s\n", GeminiModel())
		return
	}

	// 2. Try environment variables
	keys := os.Getenv("GEMINI_API_KEYS")
	if keys == "" {
		keys = os.Getenv("GEMINI_API_KEY")
	}
	if keys != "" {
		for _, k := range strings.Split(keys, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				apiKeys = append(apiKeys, k)
			}
		}
	}
	if len(apiKeys) > 0 {
		fmt.Printf("[AI] %d cle(s) API chargee(s) depuis env\n", len(apiKeys))
		fmt.Printf("[AI] Modele Gemini: %s\n", GeminiModel())
	} else {
		fmt.Println("[AI] Aucune cle API configuree (api_keys.enc / GEMINI_API_KEYS)")
	}
}

func GeminiModel() string {
	model := strings.TrimSpace(os.Getenv("GEMINI_MODEL"))
	if model == "" {
		return DefaultGeminiModel
	}
	return strings.TrimPrefix(model, "models/")
}

func geminiGenerateURL(key string) string {
	return fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", GeminiModel(), key)
}

func tryDecryptAPIKeys() []string {
	encFile := filepath.Join(secretsDir(), "api_keys.enc")
	saltFile := filepath.Join(secretsDir(), ".keystore_salt")
	secretFile := filepath.Join(secretsDir(), ".flask_secret")

	encData, err := os.ReadFile(encFile)
	if err != nil {
		return nil
	}
	salt, err := os.ReadFile(saltFile)
	if err != nil {
		return nil
	}
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		return nil
	}

	// Derive Fernet key: PBKDF2(secret, salt, 480000, SHA256) → base64url
	dk := pbkdf2Derive([]byte(strings.TrimSpace(string(secret))), salt, 480000, 32)
	fernetKey := base64Url(dk)

	// Decrypt Fernet token
	k, err := fernet.DecodeKey(string(fernetKey))
	if err != nil {
		fmt.Printf("[AI] Erreur decode Fernet key: %v\n", err)
		return nil
	}
	plaintext := fernet.VerifyAndDecrypt(encData, 0, []*fernet.Key{k})
	if plaintext == nil {
		fmt.Println("[AI] Erreur dechiffrement api_keys.enc (cle invalide?)")
		return nil
	}

	var keys []string
	for _, line := range strings.Split(string(plaintext), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			keys = append(keys, line)
		}
	}
	return keys
}

func secretsDir() string {
	// Try config package
	dir := filepath.Join(rootDir(), "secrets")
	return dir
}

func rootDir() string {
	// Same logic as config
	exe, _ := os.Executable()
	root := filepath.Dir(exe)
	if v := os.Getenv("AUTODOCKER_ROOT"); v != "" {
		root = v
	}
	if _, err := os.Stat(filepath.Join(root, "lab-ui")); err != nil {
		parent := filepath.Dir(root)
		if _, err2 := os.Stat(filepath.Join(parent, "lab-ui")); err2 == nil {
			root = parent
		}
	}
	return root
}

func pbkdf2Derive(password, salt []byte, iterations, keyLen int) []byte {
	dk := make([]byte, keyLen)
	mac := hmacSHA256(password)
	var block [4]byte
	block[3] = 1 // block number = 1 for single block

	mac.Reset()
	mac.Write(salt)
	mac.Write(block[:])
	u := mac.Sum(nil)
	copy(dk, u)

	for i := 1; i < iterations; i++ {
		mac.Reset()
		mac.Write(u)
		u = mac.Sum(nil)
		for j := range dk {
			if j < len(u) {
				dk[j] ^= u[j]
			}
		}
	}
	return dk[:keyLen]
}

func hmacSHA256(key []byte) hmacWriter {
	return hmacWriter{key: key}
}

type hmacWriter struct {
	key  []byte
	data []byte
}

func (h *hmacWriter) Reset()                      { h.data = nil }
func (h *hmacWriter) Write(p []byte) (int, error) { h.data = append(h.data, p...); return len(p), nil }
func (h *hmacWriter) Sum(b []byte) []byte {
	mac := chmac.New(csha256.New, h.key)
	mac.Write(h.data)
	return mac.Sum(b)
}

func base64Url(data []byte) []byte {
	enc := cbase64.URLEncoding.WithPadding(cbase64.StdPadding)
	buf := make([]byte, enc.EncodedLen(len(data)))
	enc.Encode(buf, data)
	return buf
}

// AIAvailable returns true if at least one API key is configured.
func AIAvailable() bool { return len(apiKeys) > 0 }

func getNextKey() string {
	keyMu.Lock()
	defer keyMu.Unlock()
	if len(apiKeys) == 0 {
		return ""
	}
	key := apiKeys[currentKeyIdx%len(apiKeys)]
	currentKeyIdx++
	return key
}

type geminiRequest struct {
	Contents       []geminiContent  `json:"contents"`
	SystemInstruct *geminiContent   `json:"system_instruction,omitempty"`
	GenConfig      *geminiGenConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenConfig struct {
	Temperature  float64 `json:"temperature"`
	ResponseMIME string  `json:"responseMimeType,omitempty"`
}

type MagicIntentInfo struct {
	Intent     string
	Confidence string
}

type MagicAIResult struct {
	DesignChecks []InfrastructureDesignCheck
	ManualSteps  []string
	Summary      string
	Reasons      map[string]string
	Questions    []string
	Attempts     int
	Payload      map[string]interface{}
	Intent       string
	Confidence   string
	Source       string
	Model        string
	Warnings     []string
}

// RobustAICall sends a prompt to Gemini with key rotation and retries.
func RobustAICall(prompt string) (string, error) {
	if !AIAvailable() {
		return "", fmt.Errorf("aucune cle API configuree")
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		key := getNextKey()
		url := geminiGenerateURL(key)

		reqBody := geminiRequest{
			SystemInstruct: &geminiContent{Parts: []geminiPart{{Text: SystemPrompt}}},
			Contents:       []geminiContent{{Parts: []geminiPart{{Text: prompt}}}},
			GenConfig:      &geminiGenConfig{Temperature: 0.2},
		}

		bodyBytes, _ := json.Marshal(reqBody)
		client := &http.Client{Timeout: 60 * time.Second}
		resp, err := client.Post(url, "application/json", bytes.NewReader(bodyBytes))
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}

		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("API status %d", resp.StatusCode)
			time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
			continue
		}

		if resp.StatusCode != 200 {
			truncated := string(data)
			if len(truncated) > 200 {
				truncated = truncated[:200]
			}
			lastErr = fmt.Errorf("API status %d: %s", resp.StatusCode, truncated)
			continue
		}

		// Parse response
		var result map[string]interface{}
		if err := json.Unmarshal(data, &result); err != nil {
			lastErr = err
			continue
		}

		text := extractTextFromGemini(result)
		if text == "" {
			lastErr = fmt.Errorf("reponse vide de Gemini")
			continue
		}
		return text, nil
	}
	return "", fmt.Errorf("echec apres 3 tentatives: %v", lastErr)
}

// CallGeminiChat sends a chat message to Gemini with a custom system prompt.
func CallGeminiChat(systemPrompt, userMessage string) (string, error) {
	if !AIAvailable() {
		return "", fmt.Errorf("aucune cle API Gemini configuree")
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		key := getNextKey()
		url := geminiGenerateURL(key)
		reqBody := geminiRequest{
			SystemInstruct: &geminiContent{Parts: []geminiPart{{Text: systemPrompt}}},
			Contents:       []geminiContent{{Parts: []geminiPart{{Text: userMessage}}}},
			GenConfig:      &geminiGenConfig{Temperature: 0.4},
		}
		bodyBytes, _ := json.Marshal(reqBody)
		client := &http.Client{Timeout: 60 * time.Second}
		resp, err := client.Post(url, "application/json", bytes.NewReader(bodyBytes))
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("API status %d", resp.StatusCode)
			time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
			continue
		}
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("API status %d", resp.StatusCode)
			continue
		}
		var result map[string]interface{}
		if err := json.Unmarshal(data, &result); err != nil {
			lastErr = err
			continue
		}
		text := extractTextFromGemini(result)
		if text == "" {
			lastErr = fmt.Errorf("reponse vide de Gemini")
			continue
		}
		return text, nil
	}
	return "", fmt.Errorf("Gemini: echec apres 3 tentatives: %v", lastErr)
}

// GeneratePresetConfig returns deterministic payloads for known high-value prompts.
// This keeps Magic useful for common scenarios even when external AI is unavailable.
func GeneratePresetConfig(prompt string) (map[string]interface{}, bool) {
	intent := DetectMagicIntent(prompt)
	switch intent.Intent {
	case "browser_game":
		return genericAppPayload(), true
	case "local_ai":
		model := localAIModelForPrompt(prompt)
		return magicPayload(
			map[string]interface{}{"socle": true, "web": false, "db": false, "collab": false, "observ": false, "sec": false, "ai": true},
			[]map[string]interface{}{
				{"key": "portainer", "network": "back"},
				{"key": "traefik", "network": "front"},
				{"key": "ollama", "network": "back", "extra": map[string]string{"model": model}},
				{"key": "open-webui", "network": "front"},
			},
		), true
	case "blog":
		return magicWebPayload([]map[string]interface{}{
			{"key": "wordpress", "network": "front"},
			{"key": "mariadb", "network": "back"},
		}), true
	case "collab":
		return magicPayload(
			map[string]interface{}{"socle": true, "web": false, "db": false, "collab": true, "observ": false, "sec": false, "ai": false},
			[]map[string]interface{}{
				{"key": "portainer", "network": "back"},
				{"key": "traefik", "network": "front"},
				{"key": "nextcloud", "network": "front"},
				{"key": "mariadb", "network": "back"},
				{"key": "samba", "network": "back"},
			},
		), true
	case "wiki":
		return magicPayload(
			map[string]interface{}{"socle": true, "web": false, "db": false, "collab": true, "observ": false, "sec": false, "ai": false},
			[]map[string]interface{}{
				{"key": "portainer", "network": "back"},
				{"key": "traefik", "network": "front"},
				{"key": "dokuwiki", "network": "front"},
			},
		), true
	case "soc":
		return magicPayload(
			map[string]interface{}{"socle": true, "web": false, "db": false, "collab": false, "observ": true, "sec": true, "ai": false},
			[]map[string]interface{}{
				{"key": "portainer", "network": "back"},
				{"key": "traefik", "network": "front"},
				{"key": "grafana", "network": "back"},
				{"key": "prometheus", "network": "back"},
				{"key": "fail2ban", "network": "back"},
			},
		), true
	case "monitoring":
		if strings.Contains(normalizePrompt(prompt), "uptime") || strings.Contains(normalizePrompt(prompt), "status") {
			return magicWebPayload([]map[string]interface{}{{"key": "uptime-kuma", "network": "front"}}), true
		}
		return magicPayload(
			map[string]interface{}{"socle": true, "web": false, "db": false, "collab": false, "observ": true, "sec": false, "ai": false},
			[]map[string]interface{}{
				{"key": "portainer", "network": "back"},
				{"key": "traefik", "network": "front"},
				{"key": "grafana", "network": "back"},
				{"key": "prometheus", "network": "back"},
			},
		), true
	case "course_platform":
		if strings.Contains(normalizePrompt(prompt), "moodle") {
			return magicWebPayload([]map[string]interface{}{
				{"key": "nginx", "network": "front"},
				{"key": "moodle", "network": "front"},
				{"key": "mariadb", "network": "back"},
			}), true
		}
		return genericAppPayload(), true
	case "dev_stack":
		return magicWebPayload([]map[string]interface{}{{"key": "nginx", "network": "front"}, {"key": "postgres", "network": "back"}, {"key": "gitea", "network": "front"}}), true
	case "automation":
		return magicWebPayload([]map[string]interface{}{{"key": "nginx", "network": "front"}, {"key": "n8n", "network": "front"}}), true
	case "storage":
		return magicWebPayload([]map[string]interface{}{{"key": "nginx", "network": "front"}, {"key": "minio", "network": "front"}}), true
	case "game_server":
		return magicWebPayload([]map[string]interface{}{
			{"key": "nginx", "network": "front"},
			{
				"key":     "minecraft",
				"network": "back",
				"image":   "itzg/minecraft-server:latest",
				"env": map[string]string{
					"EULA":        "TRUE",
					"TYPE":        "PAPER",
					"MEMORY":      "1G",
					"ONLINE_MODE": "FALSE",
				},
				"ports":   []string{"25565:25565"},
				"volumes": []string{"minecraft_data:/data"},
			},
		}), true
	case "ecommerce", "crm", "reservation", "portfolio", "app_business", "generic_app":
		return genericAppPayload(), true
	}

	return nil, false
}

func DetectMagicIntent(prompt string) MagicIntentInfo {
	p := normalizePrompt(prompt)
	switch {
	case promptHasBrowserVoxelGameIntent(prompt):
		return MagicIntentInfo{"browser_game", "high"}
	case promptHasLocalAIIntent(prompt):
		return MagicIntentInfo{"local_ai", "high"}
	case promptHasCoursePlatformIntent(prompt):
		return MagicIntentInfo{"course_platform", "high"}
	case hasAny(p, "reservation", "booking", "restaurant", "calendrier", "planning", "creneau", "agenda"):
		return MagicIntentInfo{"reservation", "high"}
	case hasAny(p, "boutique", "ecommerce", "e-commerce", "shop", "panier", "paiement", "vendre", "produit"):
		return MagicIntentInfo{"ecommerce", "high"}
	case hasAny(p, "crm", "client", "prospect", "devis", "pipeline", "contact", "rdv", "rendez-vous"):
		return MagicIntentInfo{"crm", "high"}
	case hasAny(p, "portfolio", "cv", "landing", "site vitrine", "page perso"):
		return MagicIntentInfo{"portfolio", "high"}
	case hasAny(p, "soc", "securite", "cyber", "intrusion", "logs", "siem"):
		return MagicIntentInfo{"soc", "high"}
	case hasAny(p, "git", "gitea", "forge", "depot", "repo", "repository", "code source", "developpeur"):
		return MagicIntentInfo{"dev_stack", "high"}
	case hasAny(p, "n8n", "automation", "automatisation", "workflow", "zapier", "scenario"):
		return MagicIntentInfo{"automation", "high"}
	case hasAny(p, "dokuwiki", "wiki", "documentation", "docs", "notes", "base de connaissance", "knowledge base"):
		return MagicIntentInfo{"wiki", "high"}
	case hasAny(p, "minio", "s3", "stockage objet", "object storage", "bucket"):
		return MagicIntentInfo{"storage", "high"}
	case hasAny(p, "uptime", "status page", "statuspage", "surveillance", "monitoring", "prometheus", "grafana"):
		return MagicIntentInfo{"monitoring", "high"}
	case hasAny(p, "nextcloud", "collab", "collaboration", "fichiers", "partage"):
		return MagicIntentInfo{"collab", "high"}
	case hasAny(p, "blog", "cms", "wordpress"):
		return MagicIntentInfo{"blog", "high"}
	case hasAny(p, "minecraft", "jeu", "game server", "serveur de jeu"):
		return MagicIntentInfo{"game_server", "medium"}
	case hasAny(p, "app", "application", "plateforme", "portail", "outil", "dashboard", "saas", "intranet", "logiciel", "systeme"):
		return MagicIntentInfo{"app_business", "medium"}
	default:
		return MagicIntentInfo{"unknown", "low"}
	}
}

func hasAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if needle == "" {
			continue
		}
		if shouldMatchAsToken(needle) {
			re := regexp.MustCompile(`(^|[^a-z0-9])` + regexp.QuoteMeta(needle) + `([^a-z0-9]|$)`)
			if re.MatchString(text) {
				return true
			}
			continue
		}
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func shouldMatchAsToken(needle string) bool {
	if strings.ContainsAny(needle, " -_") {
		return false
	}
	return len(needle) <= 3
}

func genericAppPayload() map[string]interface{} {
	return magicWebPayload([]map[string]interface{}{
		{"key": "nginx", "network": "front"},
	})
}

func magicWebPayload(extraServices []map[string]interface{}) map[string]interface{} {
	services := []map[string]interface{}{
		{"key": "portainer", "network": "back"},
		{"key": "traefik", "network": "front"},
	}
	services = append(services, extraServices...)
	return magicPayload(
		map[string]interface{}{"socle": true, "web": true, "db": false, "collab": false, "observ": false, "sec": false, "ai": false},
		services,
	)
}

func magicPayload(ui map[string]interface{}, services []map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"addressing": map[string]interface{}{
			"mode": "global",
			"networks": []map[string]string{
				{"name": "front", "cidr": "172.20.10.0/24", "gateway": "172.20.10.1"},
				{"name": "back", "cidr": "172.20.20.0/24", "gateway": "172.20.20.1"},
			},
		},
		"ui":       ui,
		"services": services,
	}
}

func GeneratePresetProvision(prompt string, project string, containers []string) (map[string]interface{}, bool) {
	intent := DetectMagicIntent(prompt)
	if intent.Intent == "local_ai" || intent.Intent == "blog" || intent.Intent == "collab" || intent.Intent == "soc" {
		return nil, false
	}
	nginxContainer := ""
	for _, name := range containers {
		if strings.Contains(name, "nginx") {
			nginxContainer = name
			break
		}
	}
	if nginxContainer == "" {
		return nil, false
	}
	if intent.Intent == "course_platform" {
		title := coursePlatformTitle(prompt)
		return map[string]interface{}{
			"files": []interface{}{
				map[string]interface{}{
					"container":   nginxContainer,
					"path":        "/usr/share/nginx/html/index.html",
					"content":     coursePlatformHTML(title),
					"description": "Prototype web complet de plateforme de cours",
				},
			},
			"summary": "Plateforme de cours avec catalogue, espace etudiant, progression, devoirs et tableau de bord enseignant.",
		}, true
	}
	if intent.Intent == "game_server" {
		return map[string]interface{}{
			"files": []interface{}{
				map[string]interface{}{
					"container":   nginxContainer,
					"path":        "/usr/share/nginx/html/index.html",
					"content":     minecraftWebHTML(project),
					"description": "Page web jouable et launcher Minecraft",
				},
			},
			"summary": "Mini-jeu web type blocs + serveur Minecraft Docker pret. Le port Minecraft est visible dans le Dashboard.",
		}, true
	}
	if intent.Intent == "browser_game" {
		return map[string]interface{}{
			"files": []interface{}{
				map[string]interface{}{
					"container":   nginxContainer,
					"path":        "/usr/share/nginx/html/index.html",
					"content":     voxelBrowserGameHTML(project),
					"description": "Jeu 3D voxel statique HTML/CSS/JS",
				},
			},
			"summary": "Jeu 3D voxel jouable dans le navigateur, servi par nginx sans backend.",
		}, true
	}

	return map[string]interface{}{
		"files": []interface{}{
			map[string]interface{}{
				"container":   nginxContainer,
				"path":        "/usr/share/nginx/html/index.html",
				"content":     genericPrototypeHTML(prompt, intent.Intent),
				"description": "Prototype web complet genere par Mode Magic",
			},
		},
		"summary": "Prototype web interactif genere pour la demande : " + shortSummary(prompt, 90),
	}, true
}

func localAIModelForPrompt(prompt string) string {
	p := normalizePrompt(prompt)
	if strings.Contains(p, "rapide") || strings.Contains(p, "vite") || strings.Contains(p, "leger") || strings.Contains(p, "light") || strings.Contains(p, "fast") || strings.Contains(p, "prebuild") || strings.Contains(p, "pre build") || strings.Contains(p, "test") || strings.Contains(p, "demo") || strings.Contains(p, "minuscule") || strings.Contains(p, "tiny") || strings.Contains(p, "tinyllama") {
		return FastOllamaModel
	}
	return DefaultOllamaModel
}

func promptHasLocalAIIntent(prompt string) bool {
	p := normalizePrompt(prompt)
	hasAI := hasAny(p, "ia", "ai", "llm", "mistral", "ollama", "chatbot", "assistant")
	hasLocal := strings.Contains(p, "local") || strings.Contains(p, "docker") || strings.Contains(p, "conteneur")
	hasWebUI := strings.Contains(p, "interface") || strings.Contains(p, "web") || strings.Contains(p, "navigateur") || strings.Contains(p, "open-webui") || strings.Contains(p, "open webui") || strings.Contains(p, "ui") || strings.Contains(p, "chat")
	return strings.Contains(p, "ollama") || strings.Contains(p, "open-webui") || strings.Contains(p, "open webui") || strings.Contains(p, "tinyllama") || (hasAI && hasLocal && hasWebUI) || (strings.Contains(p, "mistral") && hasLocal)
}

func promptHasBrowserVoxelGameIntent(prompt string) bool {
	p := normalizePrompt(prompt)
	hasGame := hasAny(p, "jeu", "game") || strings.Contains(p, "minecraft") || strings.Contains(p, "voxel") || strings.Contains(p, "bloc")
	hasBrowser := strings.Contains(p, "page web") || strings.Contains(p, "navigateur") || strings.Contains(p, "browser") || strings.Contains(p, "html") || strings.Contains(p, "css") || strings.Contains(p, "javascript") || strings.Contains(p, " js") || strings.Contains(p, "three") || strings.Contains(p, "nginx") || strings.Contains(p, "localstorage")
	has3D := strings.Contains(p, "3d") || strings.Contains(p, "voxel") || strings.Contains(p, "minecraft") || strings.Contains(p, "three")
	noServer := strings.Contains(p, "pas de serveur") || strings.Contains(p, "sans serveur") || strings.Contains(p, "pas de backend") || strings.Contains(p, "statique") || strings.Contains(p, "static")

	return hasGame && hasBrowser && (has3D || noServer)
}

func promptHasCoursePlatformIntent(prompt string) bool {
	p := normalizePrompt(prompt)
	hasLearning := strings.Contains(p, "cours") || strings.Contains(p, "formation") || strings.Contains(p, "lms") || strings.Contains(p, "apprentissage") || strings.Contains(p, "academy") || strings.Contains(p, "ecole")
	hasAudience := strings.Contains(p, "etudiant") || strings.Contains(p, "eleve") || strings.Contains(p, "classe") || strings.Contains(p, "prof") || strings.Contains(p, "enseignant")
	hasPlatform := strings.Contains(p, "plateform") || strings.Contains(p, "platform") || strings.Contains(p, "portail") || strings.Contains(p, "espace") || strings.Contains(p, "dashboard") || strings.Contains(p, "site") || strings.Contains(p, "ligne")
	return strings.Contains(p, "moodle") || strings.Contains(p, "lms") || (hasLearning && (hasAudience || hasPlatform)) || strings.Contains(p, "cours en ligne")
}

func normalizePrompt(prompt string) string {
	p := strings.ToLower(prompt)
	return strings.NewReplacer(
		"à", "a", "â", "a", "ä", "a",
		"é", "e", "è", "e", "ê", "e", "ë", "e",
		"î", "i", "ï", "i",
		"ô", "o", "ö", "o",
		"ù", "u", "û", "u", "ü", "u",
		"ç", "c",
		"à", "a", "â", "a", "ä", "a",
		"é", "e", "è", "e", "ê", "e", "ë", "e",
		"î", "i", "ï", "i",
		"ô", "o", "ö", "o",
		"ù", "u", "û", "u", "ü", "u",
		"ç", "c",
	).Replace(p)
}

func coursePlatformTitle(prompt string) string {
	p := normalizePrompt(prompt)
	switch {
	case strings.Contains(p, "math"):
		return "MathCampus"
	case strings.Contains(p, "physique") || strings.Contains(p, "chimie") || strings.Contains(p, "science"):
		return "SciencesLab"
	case strings.Contains(p, "anglais") || strings.Contains(p, "langue") || strings.Contains(p, "english"):
		return "LinguaCampus"
	case strings.Contains(p, "cyber"):
		return "CyberClass"
	case strings.Contains(p, "dev") || strings.Contains(p, "code"):
		return "CodeCampus"
	case strings.Contains(p, "business") || strings.Contains(p, "commerce"):
		return "Business Academy"
	default:
		return "Campus Nova"
	}
}

func coursePlatformHTML(title string) string {
	return fmt.Sprintf(`<!doctype html>
<html lang="fr">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>%s</title>
  <style>
    :root{color-scheme:dark;--bg:#07111f;--panel:#0f1f33;--text:#f7fbff;--muted:#95a9bd;--docker:#2496ed;--cyan:#16c7f2;--green:#35d07f;--orange:#ffb347;--line:rgba(255,255,255,.12)}
    *{box-sizing:border-box}body{margin:0;font-family:Inter,Segoe UI,system-ui,sans-serif;background:radial-gradient(circle at 20%% 0,#12355a 0,#07111f 42%%,#050b12 100%%);color:var(--text)}
    .shell{min-height:100vh;display:grid;grid-template-columns:270px 1fr}aside{border-right:1px solid var(--line);background:rgba(7,17,31,.82);backdrop-filter:blur(14px);padding:24px;position:sticky;top:0;height:100vh}
    .brand{display:flex;align-items:center;gap:12px;font-weight:900;font-size:1.15rem}.logo{width:38px;height:38px;border-radius:10px;background:linear-gradient(135deg,var(--docker),var(--cyan));display:grid;place-items:center;font-weight:900}
    nav{margin-top:34px;display:grid;gap:8px}.nav{border:1px solid transparent;color:var(--muted);padding:12px 14px;border-radius:10px;cursor:pointer}.nav.active,.nav:hover{border-color:rgba(36,150,237,.45);background:rgba(36,150,237,.13);color:#fff}
    main{padding:28px;max-width:1440px;width:100%%}.top{display:flex;justify-content:space-between;align-items:center;gap:18px;margin-bottom:24px}.eyebrow{color:var(--cyan);font-size:.78rem;font-weight:800;text-transform:uppercase;letter-spacing:.08em}
    h1{font-size:clamp(2rem,4vw,4.8rem);line-height:.95;margin:8px 0 14px;letter-spacing:0}.lead{color:var(--muted);font-size:1.08rem;max-width:760px;line-height:1.55}.cta{display:flex;gap:10px;flex-wrap:wrap}
    .btn{border:0;border-radius:10px;padding:12px 16px;font-weight:800;color:#04111f;background:linear-gradient(135deg,var(--docker),var(--cyan));cursor:pointer}.btn.secondary{background:#132a42;color:#d9edff;border:1px solid var(--line)}
    .grid{display:grid;grid-template-columns:1.5fr .9fr;gap:18px}.panel{background:linear-gradient(180deg,rgba(19,42,66,.92),rgba(11,24,39,.92));border:1px solid var(--line);border-radius:14px;padding:18px;box-shadow:0 24px 60px rgba(0,0,0,.22)}
    .cards{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:12px}.course{padding:15px;border:1px solid rgba(255,255,255,.11);border-radius:12px;background:rgba(255,255,255,.04)}.course strong{display:block;font-size:1rem;margin-bottom:8px}.course p{color:var(--muted);font-size:.9rem;line-height:1.45}.tag{display:inline-flex;padding:5px 8px;border-radius:999px;background:rgba(36,150,237,.15);color:#bfe6ff;font-size:.72rem;font-weight:800;margin-bottom:10px}
    .progress{display:grid;gap:12px}.bar{height:10px;background:#07111f;border-radius:999px;overflow:hidden}.bar span{display:block;height:100%%;background:linear-gradient(90deg,var(--green),var(--cyan))}
    .metric{display:flex;justify-content:space-between;align-items:center;padding:12px;border:1px solid var(--line);border-radius:10px;background:rgba(255,255,255,.035)}.metric b{font-size:1.35rem}.lesson{display:grid;grid-template-columns:72px 1fr auto;align-items:center;gap:12px;padding:12px;border-bottom:1px solid var(--line)}.play{height:48px;border-radius:10px;background:rgba(36,150,237,.18);display:grid;place-items:center;color:#bfe6ff;font-weight:900}
    table{width:100%%;border-collapse:collapse}td,th{text-align:left;padding:12px;border-bottom:1px solid var(--line);color:#d8e7f5}th{color:#8fb7d9;font-size:.78rem;text-transform:uppercase}.status{color:var(--green);font-weight:900}.warn{color:var(--orange);font-weight:900}
    @media(max-width:900px){.shell{display:block}aside{height:auto;position:relative}.grid,.cards{grid-template-columns:1fr}.top{align-items:flex-start;flex-direction:column}main{padding:18px}.lesson{grid-template-columns:52px 1fr}}
  </style>
</head>
<body>
  <div class="shell">
    <aside><div class="brand"><div class="logo">AD</div><span>%s</span></div><nav><div class="nav active">Accueil</div><div class="nav">Catalogue</div><div class="nav">Mes cours</div><div class="nav">Devoirs</div><div class="nav">Enseignants</div></nav></aside>
    <main>
      <section class="top"><div><div class="eyebrow">Plateforme etudiants</div><h1>Apprendre, suivre, reussir.</h1><p class="lead">Un espace de cours moderne avec catalogue, progression, planning, devoirs et ressources pour garder les etudiants engages du premier module jusqu'a la validation finale.</p></div><div class="cta"><button class="btn">Voir les cours</button><button class="btn secondary">Planning</button></div></section>
      <section class="grid">
        <div class="panel"><h2>Catalogue prioritaire</h2><div class="cards"><article class="course"><span class="tag">Nouveau</span><strong>Fondations Web</strong><p>HTML, CSS, interfaces responsives et bonnes pratiques projet.</p><div class="bar"><span style="width:76%%"></span></div></article><article class="course"><span class="tag">Atelier</span><strong>Docker pour debutants</strong><p>Images, conteneurs, volumes, reseaux et premiers deploiements.</p><div class="bar"><span style="width:54%%"></span></div></article><article class="course"><span class="tag">Evaluation</span><strong>Projet final</strong><p>Construire une mini-plateforme et presenter son architecture.</p><div class="bar"><span style="width:28%%"></span></div></article></div><h2 style="margin-top:24px">Parcours du jour</h2><div class="lesson"><div class="play">01</div><div><strong>Revoir le chapitre reseau</strong><br><span style="color:var(--muted)">Video courte + quiz de verification</span></div><span class="status">Pret</span></div><div class="lesson"><div class="play">02</div><div><strong>Rendre le devoir Docker</strong><br><span style="color:var(--muted)">Archive compose.yaml + capture du resultat</span></div><span class="warn">18:00</span></div><div class="lesson"><div class="play">03</div><div><strong>Session questions</strong><br><span style="color:var(--muted)">Classe virtuelle avec l'enseignant</span></div><span class="status">Live</span></div></div>
        <div class="panel progress"><h2>Tableau de bord</h2><div class="metric"><span>Etudiants actifs</span><b>128</b></div><div class="metric"><span>Cours publies</span><b>14</b></div><div class="metric"><span>Devoirs a corriger</span><b>23</b></div><div class="metric"><span>Taux de progression</span><b>72%%</b></div><table><thead><tr><th>Groupe</th><th>Avancee</th></tr></thead><tbody><tr><td>Classe A</td><td class="status">84%%</td></tr><tr><td>Classe B</td><td>69%%</td></tr><tr><td>Mentorat</td><td class="warn">42%%</td></tr></tbody></table></div>
      </section>
    </main>
  </div>
  <script>document.querySelectorAll('.nav').forEach(item=>item.addEventListener('click',()=>{document.querySelectorAll('.nav').forEach(n=>n.classList.remove('active'));item.classList.add('active')}));</script>
</body>
</html>`, title, title)
}

func minecraftWebHTML(project string) string {
	html := `<!doctype html>
<html lang="fr">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>MineWeb - {{PROJECT}}</title>
  <style>
    :root{color-scheme:dark;--bg:#08111f;--panel:#102033;--line:rgba(255,255,255,.13);--text:#eef7ff;--muted:#9ab0c5;--green:#4ade80;--blue:#38bdf8;--gold:#facc15}
    *{box-sizing:border-box}body{margin:0;font-family:Inter,Segoe UI,system-ui,sans-serif;background:linear-gradient(135deg,#08111f,#14351f 48%,#06101d);color:var(--text)}
    .app{min-height:100vh;display:grid;grid-template-columns:300px 1fr}.side{padding:24px;background:rgba(6,14,25,.86);border-right:1px solid var(--line)}
    .brand{display:flex;gap:12px;align-items:center;font-weight:900;font-size:1.2rem}.cube{width:42px;height:42px;border-radius:9px;background:linear-gradient(135deg,#65a30d,#166534);display:grid;place-items:center;box-shadow:inset 0 -10px rgba(0,0,0,.18)}
    .hint{color:var(--muted);line-height:1.55;font-size:.93rem}.pill{display:inline-flex;margin:6px 6px 0 0;padding:7px 9px;border-radius:999px;background:rgba(56,189,248,.12);border:1px solid rgba(56,189,248,.25);color:#d8f3ff;font-size:.76rem;font-weight:800}
    main{padding:24px;display:grid;gap:16px}.top{display:flex;justify-content:space-between;gap:14px;align-items:flex-start}h1{margin:0;font-size:clamp(2rem,4vw,4rem);line-height:.95}.status{padding:12px 14px;border:1px solid var(--line);background:rgba(255,255,255,.05);border-radius:10px;color:var(--green);font-weight:900}
    .board{display:grid;grid-template-columns:minmax(0,1fr) 260px;gap:16px}.canvasWrap{border:1px solid var(--line);border-radius:12px;background:#87ceeb;padding:10px;box-shadow:0 20px 60px rgba(0,0,0,.3)}
    canvas{width:100%;aspect-ratio:16/9;display:block;background:#87ceeb;border-radius:8px;image-rendering:pixelated}.panel{border:1px solid var(--line);border-radius:12px;background:rgba(16,32,51,.9);padding:16px}
    .hotbar{display:grid;grid-template-columns:repeat(4,1fr);gap:8px;margin-top:12px}.slot{border:1px solid var(--line);background:rgba(255,255,255,.06);border-radius:8px;padding:10px;text-align:center;cursor:pointer;font-weight:800}.slot.active{outline:2px solid var(--gold);color:var(--gold)}
    .meter{height:9px;background:#06101d;border-radius:999px;overflow:hidden;margin:8px 0 14px}.meter span{display:block;height:100%;background:linear-gradient(90deg,var(--green),var(--blue))}
    @media(max-width:950px){.app{display:block}.board{grid-template-columns:1fr}.side{border-right:0;border-bottom:1px solid var(--line)}}
  </style>
</head>
<body>
<div class="app">
  <aside class="side">
    <div class="brand"><div class="cube">MW</div><span>{{PROJECT}}</span></div>
    <p class="hint">Mini-jeu web de blocs genere par AutoDocker. Le serveur Minecraft Docker tourne a cote; retrouve son port dans le Dashboard, ligne <strong>{{PROJECT}}-minecraft</strong>.</p>
    <span class="pill">WASD / fleches</span><span class="pill">Clic gauche: casser</span><span class="pill">Clic droit: placer</span><span class="pill">1-4: bloc</span>
  </aside>
  <main>
    <section class="top"><div><h1>Construis ton monde.</h1><p class="hint">Un terrain jouable dans le navigateur, plus un vrai serveur Minecraft pret pour se connecter depuis le client Minecraft.</p></div><div class="status">Serveur Docker pret</div></section>
    <section class="board">
      <div class="canvasWrap"><canvas id="game" width="960" height="540"></canvas></div>
      <aside class="panel">
        <h2>Inventaire</h2>
        <div>Vie</div><div class="meter"><span style="width:94%"></span></div>
        <div>Monde charge</div><div class="meter"><span style="width:100%"></span></div>
        <div class="hotbar" id="hotbar"></div>
        <p class="hint">Astuce: place des blocs sous tes pieds pour grimper. Le mini-jeu est local a cette page; le serveur complet est expose par Docker.</p>
      </aside>
    </section>
  </main>
</div>
<script>
const canvas=document.getElementById('game'),ctx=canvas.getContext('2d'),tile=24,cols=40,rows=22;
const types=[{n:'Herbe',c:'#4ade80'},{n:'Terre',c:'#8b5a2b'},{n:'Pierre',c:'#7c8794'},{n:'Or',c:'#facc15'}];
const world=Array.from({length:rows},(_,y)=>Array.from({length:cols},(_,x)=> y>15?1:y===15?0:(Math.random()<.03&&y>9?2:null)));
let player={x:5,y:13,vx:0,vy:0,on:false},keys={},sel=0;
const hb=document.getElementById('hotbar');types.forEach((t,i)=>{const b=document.createElement('button');b.className='slot'+(i?'':' active');b.textContent=(i+1)+' '+t.n;b.onclick=()=>setSel(i);hb.appendChild(b)});
function setSel(i){sel=i;document.querySelectorAll('.slot').forEach((b,n)=>b.classList.toggle('active',n===i))}
addEventListener('keydown',e=>{keys[e.key.toLowerCase()]=true;if(/[1-4]/.test(e.key))setSel(Number(e.key)-1);if((e.key==='w'||e.key==='ArrowUp'||e.key===' ')&&player.on){player.vy=-9;player.on=false}});
addEventListener('keyup',e=>keys[e.key.toLowerCase()]=false);
canvas.addEventListener('contextmenu',e=>e.preventDefault());
canvas.addEventListener('mousedown',e=>{const r=canvas.getBoundingClientRect(),x=Math.floor((e.clientX-r.left)/r.width*canvas.width/tile),y=Math.floor((e.clientY-r.top)/r.height*canvas.height/tile);if(x<0||y<0||x>=cols||y>=rows)return;if(e.button===2&&!world[y][x])world[y][x]=sel;else if(e.button===0)world[y][x]=null});
function solid(x,y){return y>=rows||x<0||x>=cols||(y>=0&&world[y][x]!=null)}
function step(){player.vx=(keys.a||keys.arrowleft?-3:0)+(keys.d||keys.arrowright?3:0);player.vy+=.45;let nx=player.x+player.vx/10,ny=player.y+player.vy/10;if(!solid(Math.floor(nx),Math.floor(player.y+1)))player.x=nx;if(player.vy>0&&solid(Math.floor(player.x),Math.floor(ny+1))){player.vy=0;player.on=true;ny=Math.floor(ny)}else if(player.vy<0&&solid(Math.floor(player.x),Math.floor(ny))){player.vy=0}else player.on=false;player.y=ny;if(player.y>rows)player={x:5,y:13,vx:0,vy:0,on:false};draw();requestAnimationFrame(step)}
function draw(){ctx.fillStyle='#86d5ff';ctx.fillRect(0,0,canvas.width,canvas.height);ctx.fillStyle='rgba(255,255,255,.8)';ctx.beginPath();ctx.arc(790,82,34,0,7);ctx.fill();for(let y=0;y<rows;y++)for(let x=0;x<cols;x++){let id=world[y][x];if(id==null)continue;ctx.fillStyle=types[id].c;ctx.fillRect(x*tile,y*tile,tile,tile);ctx.strokeStyle='rgba(0,0,0,.18)';ctx.strokeRect(x*tile,y*tile,tile,tile)}ctx.fillStyle='#2563eb';ctx.fillRect(player.x*tile,player.y*tile,tile*.8,tile*.95);ctx.fillStyle='#f8c08a';ctx.fillRect(player.x*tile+5,player.y*tile-8,14,12)}
step();
</script>
</body>
</html>`
	return strings.ReplaceAll(html, "{{PROJECT}}", escapeHTML(project))
}

func voxelBrowserGameHTML(project string) string {
	html := `<!doctype html>
<html lang="fr">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>{{PROJECT}} - Voxel Web</title>
  <style>
    :root{color-scheme:dark;--bg:#07111f;--panel:rgba(8,18,32,.78);--line:rgba(255,255,255,.18);--text:#eff8ff;--muted:#9fb4c8;--cyan:#38bdf8;--green:#22c55e;--gold:#facc15}
    *{box-sizing:border-box}html,body{height:100%;margin:0;overflow:hidden;background:#07111f;color:var(--text);font-family:Inter,Segoe UI,system-ui,sans-serif}
    #game{position:fixed;inset:0;background:#87ceeb}canvas{display:block;width:100%;height:100%}
    .hud{position:fixed;inset:0;pointer-events:none}.top{position:absolute;top:16px;left:16px;right:16px;display:flex;justify-content:space-between;gap:12px;align-items:flex-start}
    .card{pointer-events:auto;background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:12px 14px;box-shadow:0 14px 40px rgba(0,0,0,.28);backdrop-filter:blur(12px)}
    h1{font-size:1rem;margin:0 0 6px}.muted{color:var(--muted);font-size:.86rem;line-height:1.45}.stats{font-family:Consolas,monospace;font-size:.86rem;color:#d9f3ff}
    .cross{position:absolute;left:50%;top:50%;width:20px;height:20px;transform:translate(-50%,-50%)}.cross:before,.cross:after{content:"";position:absolute;background:rgba(255,255,255,.86);box-shadow:0 0 6px rgba(0,0,0,.55)}.cross:before{left:9px;top:0;width:2px;height:20px}.cross:after{left:0;top:9px;width:20px;height:2px}
    .hotbar{position:absolute;left:50%;bottom:20px;transform:translateX(-50%);display:flex;gap:8px;pointer-events:auto}.slot{min-width:92px;border:1px solid var(--line);background:var(--panel);color:var(--text);border-radius:9px;padding:10px 12px;font-weight:800;cursor:pointer}.slot.active{outline:2px solid var(--gold);color:var(--gold)}
    .actions{position:absolute;right:16px;bottom:20px;display:flex;gap:8px;pointer-events:auto}.btn{border:1px solid var(--line);background:rgba(8,18,32,.82);color:var(--text);border-radius:9px;padding:10px 12px;font-weight:800;cursor:pointer}.btn:hover,.slot:hover{border-color:var(--cyan)}
    .centerHint{position:absolute;left:50%;top:62%;transform:translate(-50%,-50%);pointer-events:auto;text-align:center}.play{border:0;border-radius:14px;padding:14px 22px;background:linear-gradient(135deg,var(--green),var(--cyan));color:#04111f;font-weight:950;font-size:1rem;cursor:pointer;box-shadow:0 16px 50px rgba(56,189,248,.32)}
    #error{position:fixed;left:50%;top:50%;transform:translate(-50%,-50%);max-width:560px;display:none}.kbd{display:inline-block;border:1px solid var(--line);border-bottom-width:3px;border-radius:6px;padding:2px 6px;background:rgba(255,255,255,.08);font-weight:800;color:#fff}
  </style>
</head>
<body>
  <div id="game"></div>
  <div id="error" class="card"></div>
  <div class="hud">
    <div class="top">
      <div class="card">
        <h1>{{PROJECT}} - MiniCraft Web</h1>
        <div class="muted"><span class="kbd">WASD</span> bouger · <span class="kbd">Souris</span> regarder · <span class="kbd">Espace</span> sauter · clic gauche casser · clic droit poser</div>
      </div>
      <div class="card stats" id="stats">FPS --<br>XYZ --</div>
    </div>
    <div class="cross"></div>
    <div class="centerHint" id="lockHint"><button class="play" id="playBtn">Cliquer pour jouer</button><div class="muted" style="margin-top:10px">Le curseur sera capture par le jeu.</div></div>
    <div class="hotbar" id="hotbar"></div>
    <div class="actions"><button class="btn" id="saveBtn">Sauver</button><button class="btn" id="resetBtn">Reset monde</button></div>
  </div>
  <script src="https://unpkg.com/three@0.160.0/build/three.min.js"></script>
  <script>
  (function(){
    var storageKey = 'autodocker_voxel_world_{{PROJECT}}';
    var blockTypes = [
      {name:'Herbe', color:0x5fcf65},
      {name:'Terre', color:0x8b5a2b},
      {name:'Pierre', color:0x7b8794},
      {name:'Bois', color:0x9a6b3a},
      {name:'Eau', color:0x38bdf8}
    ];
    var selected = 0, keys = {}, yaw = 0, pitch = 0, last = performance.now(), fpsLast = last, frames = 0, fps = 0;
    var player = {x:4, y:8, z:6, vy:0, onGround:false};
    var scene, camera, renderer, raycaster, materials = [], blocks = new Map(), meshes = new Map();
    var hotbar = document.getElementById('hotbar'), stats = document.getElementById('stats'), hint = document.getElementById('lockHint');

    blockTypes.forEach(function(t, i){
      var b = document.createElement('button');
      b.className = 'slot' + (i === 0 ? ' active' : '');
      b.textContent = (i + 1) + ' ' + t.name;
      b.onclick = function(){ selectBlock(i); };
      hotbar.appendChild(b);
    });
    function selectBlock(i){ selected = i; Array.from(document.querySelectorAll('.slot')).forEach(function(el,n){ el.classList.toggle('active', n === i); }); }
    function key(x,y,z){ return x + ',' + y + ',' + z; }
    function parseKey(k){ return k.split(',').map(function(v){ return parseInt(v,10); }); }

    function init(){
      if (!window.THREE) {
        document.getElementById('error').style.display = 'block';
        document.getElementById('error').innerHTML = '<h1>Three.js non charge</h1><p class="muted">Le jeu est statique, mais il a besoin de charger Three.js depuis un CDN. Verifie la connexion internet du navigateur.</p>';
        return;
      }
      scene = new THREE.Scene();
      scene.background = new THREE.Color(0x86d5ff);
      scene.fog = new THREE.Fog(0x86d5ff, 35, 95);
      camera = new THREE.PerspectiveCamera(75, innerWidth / innerHeight, 0.1, 220);
      renderer = new THREE.WebGLRenderer({antialias:false});
      renderer.setPixelRatio(Math.min(devicePixelRatio, 1.6));
      renderer.setSize(innerWidth, innerHeight);
      document.getElementById('game').appendChild(renderer.domElement);
      raycaster = new THREE.Raycaster();
      materials = blockTypes.map(function(t){ return new THREE.MeshLambertMaterial({color:t.color}); });
      scene.add(new THREE.HemisphereLight(0xffffff, 0x334455, 1.8));
      var sun = new THREE.DirectionalLight(0xffffff, 1.2); sun.position.set(20,40,10); scene.add(sun);
      var grid = new THREE.GridHelper(80, 80, 0xffffff, 0xffffff); grid.material.opacity = .08; grid.material.transparent = true; scene.add(grid);
      loadWorld();
      bind();
      loop(performance.now());
    }

    function bind(){
      addEventListener('resize', function(){ camera.aspect = innerWidth / innerHeight; camera.updateProjectionMatrix(); renderer.setSize(innerWidth, innerHeight); });
      addEventListener('keydown', function(e){ keys[e.key.toLowerCase()] = true; if (/^[1-5]$/.test(e.key)) selectBlock(parseInt(e.key,10)-1); if (e.code === 'Space' && player.onGround) { player.vy = 8.5; player.onGround = false; } });
      addEventListener('keyup', function(e){ keys[e.key.toLowerCase()] = false; });
      document.addEventListener('mousemove', function(e){
        if (document.pointerLockElement !== renderer.domElement) return;
        yaw -= e.movementX * 0.0022;
        pitch -= e.movementY * 0.0022;
        pitch = Math.max(-1.35, Math.min(1.35, pitch));
      });
      renderer.domElement.addEventListener('contextmenu', function(e){ e.preventDefault(); });
      renderer.domElement.addEventListener('mousedown', function(e){
        if (document.pointerLockElement !== renderer.domElement) renderer.domElement.requestPointerLock();
        editBlock(e.button);
      });
      document.getElementById('playBtn').onclick = function(){ renderer.domElement.requestPointerLock(); };
      document.addEventListener('pointerlockchange', function(){ hint.style.display = document.pointerLockElement === renderer.domElement ? 'none' : 'block'; });
      document.getElementById('saveBtn').onclick = saveWorld;
      document.getElementById('resetBtn').onclick = function(){ localStorage.removeItem(storageKey); location.reload(); };
    }

    function addBlock(x,y,z,type){
      var k = key(x,y,z);
      if (blocks.has(k)) return;
      blocks.set(k, type);
      var mesh = new THREE.Mesh(new THREE.BoxGeometry(1,1,1), materials[type]);
      mesh.position.set(x + .5, y + .5, z + .5);
      mesh.userData.pos = {x:x,y:y,z:z};
      scene.add(mesh);
      meshes.set(k, mesh);
    }
    function removeBlock(x,y,z){
      var k = key(x,y,z), mesh = meshes.get(k);
      if (!mesh) return;
      scene.remove(mesh); mesh.geometry.dispose();
      blocks.delete(k); meshes.delete(k);
    }
    function loadWorld(){
      var saved = localStorage.getItem(storageKey);
      if (saved) {
        try { JSON.parse(saved).forEach(function(b){ addBlock(b[0],b[1],b[2],b[3]); }); return; } catch(e) {}
      }
      for (var x=-18; x<=18; x++) for (var z=-18; z<=18; z++) {
        var h = 2 + Math.floor(1.7 * Math.sin(x*.34) + 1.5 * Math.cos(z*.28) + Math.random() * 1.8);
        for (var y=0; y<h; y++) addBlock(x,y,z, y === h-1 ? 0 : (y < h-3 ? 2 : 1));
        if (Math.random() < .03) makeTree(x, h, z);
      }
    }
    function makeTree(x,y,z){
      for (var i=0;i<4;i++) addBlock(x,y+i,z,3);
      for (var dx=-1;dx<=1;dx++) for (var dz=-1;dz<=1;dz++) for (var dy=2;dy<=4;dy++) if (Math.abs(dx)+Math.abs(dz)+dy < 6) addBlock(x+dx,y+dy,z+dz,0);
    }
    function saveWorld(){
      var arr = [];
      blocks.forEach(function(type,k){ var p = parseKey(k); arr.push([p[0],p[1],p[2],type]); });
      localStorage.setItem(storageKey, JSON.stringify(arr));
    }

    function blockAt(x,y,z){ return blocks.has(key(Math.floor(x), Math.floor(y), Math.floor(z))); }
    function overlapsPlayerBlock(x,y,z){
      var r=.34, h=1.72;
      return x < player.x + r && x + 1 > player.x - r && z < player.z + r && z + 1 > player.z - r && y < player.y + h && y + 1 > player.y;
    }
    function collides(x,y,z){
      var r=.32, h=1.72;
      for (var dx=-r; dx<=r; dx+=r*2) for (var dz=-r; dz<=r; dz+=r*2) {
        if (blockAt(x+dx, y+.08, z+dz) || blockAt(x+dx, y+h, z+dz)) return true;
      }
      return false;
    }
    function movePlayer(dt){
      var speed = keys.shift ? 8 : 5;
      var forward = new THREE.Vector3(-Math.sin(yaw),0,-Math.cos(yaw));
      var right = new THREE.Vector3(Math.cos(yaw),0,-Math.sin(yaw));
      var move = new THREE.Vector3();
      if (keys.w || keys.arrowup) move.add(forward);
      if (keys.s || keys.arrowdown) move.sub(forward);
      if (keys.d || keys.arrowright) move.add(right);
      if (keys.a || keys.arrowleft) move.sub(right);
      if (move.lengthSq() > 0) move.normalize().multiplyScalar(speed * dt);
      var nx = player.x + move.x, nz = player.z + move.z;
      if (!collides(nx, player.y, player.z)) player.x = nx;
      if (!collides(player.x, player.y, nz)) player.z = nz;
      player.vy -= 18 * dt;
      var ny = player.y + player.vy * dt;
      if (collides(player.x, ny, player.z)) {
        if (player.vy < 0) { while (collides(player.x, ny, player.z)) ny += .04; player.onGround = true; }
        player.vy = 0;
      } else {
        player.onGround = false;
      }
      player.y = ny;
      if (player.y < -12) { player.x = 4; player.y = 10; player.z = 6; player.vy = 0; }
    }
    function editBlock(button){
      raycaster.setFromCamera({x:0,y:0}, camera);
      var hits = raycaster.intersectObjects(Array.from(meshes.values()), false);
      if (!hits.length || hits[0].distance > 7) return;
      var hit = hits[0], pos = hit.object.userData.pos;
      if (button === 0) {
        if (!(pos.y < 2 && Math.abs(pos.x) < 2 && Math.abs(pos.z) < 2)) removeBlock(pos.x,pos.y,pos.z);
      } else if (button === 2) {
        var n = hit.face.normal.clone().transformDirection(hit.object.matrixWorld);
        var x = pos.x + Math.round(n.x), y = pos.y + Math.round(n.y), z = pos.z + Math.round(n.z);
        if (!overlapsPlayerBlock(x,y,z) && !blockAt(x,y,z)) addBlock(x,y,z,selected);
      }
      saveWorld();
    }
    function loop(now){
      var dt = Math.min(.05, (now-last)/1000); last = now; frames++;
      if (now - fpsLast > 500) { fps = Math.round(frames * 1000 / (now - fpsLast)); frames = 0; fpsLast = now; }
      movePlayer(dt);
      camera.position.set(player.x, player.y + 1.62, player.z);
      camera.rotation.order = 'YXZ'; camera.rotation.y = yaw; camera.rotation.x = pitch;
      stats.innerHTML = 'FPS ' + fps + '<br>XYZ ' + player.x.toFixed(1) + ' / ' + player.y.toFixed(1) + ' / ' + player.z.toFixed(1) + '<br>Blocs ' + blocks.size;
      renderer.render(scene, camera);
      requestAnimationFrame(loop);
    }
    init();
  })();
  </script>
</body>
</html>`
	return strings.ReplaceAll(html, "{{PROJECT}}", escapeHTML(project))
}

func genericPrototypeHTML(prompt string, intent string) string {
	title := prototypeTitle(prompt, intent)
	safePrompt := escapeHTML(shortSummary(prompt, 180))
	primary := prototypePrimaryNoun(intent)
	return fmt.Sprintf(`<!doctype html>
<html lang="fr">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>%s</title>
  <style>
    :root{color-scheme:dark;--bg:#07111f;--panel:#0f2035;--panel2:#132c46;--text:#f7fbff;--muted:#98aec3;--docker:#2496ed;--cyan:#16c7f2;--green:#35d07f;--orange:#ffb347;--line:rgba(255,255,255,.12)}
    *{box-sizing:border-box}body{margin:0;font-family:Inter,Segoe UI,system-ui,sans-serif;background:linear-gradient(135deg,#07111f,#0c2238 48%%,#06101d);color:var(--text)}
    .app{min-height:100vh;display:grid;grid-template-columns:260px 1fr}.side{padding:24px;border-right:1px solid var(--line);background:rgba(5,12,22,.72);backdrop-filter:blur(16px)}
    .brand{display:flex;align-items:center;gap:12px;font-weight:900}.logo{width:38px;height:38px;border-radius:10px;background:linear-gradient(135deg,var(--docker),var(--cyan));display:grid;place-items:center;color:white}
    .nav{margin-top:34px;display:grid;gap:8px}.nav button{all:unset;padding:12px;border-radius:10px;color:var(--muted);cursor:pointer}.nav button.active,.nav button:hover{background:rgba(36,150,237,.14);color:white}
    main{padding:28px;max-width:1420px;width:100%%}.hero{display:flex;justify-content:space-between;gap:18px;align-items:flex-start;margin-bottom:20px}.kicker{color:var(--cyan);font-weight:900;text-transform:uppercase;font-size:.76rem;letter-spacing:.08em}
    h1{margin:8px 0 10px;font-size:clamp(2rem,4vw,4.6rem);line-height:.95;letter-spacing:0}.lead{color:var(--muted);max-width:820px;line-height:1.55;font-size:1.03rem}
    .btn{border:0;border-radius:10px;padding:12px 16px;font-weight:900;color:#04111f;background:linear-gradient(135deg,var(--docker),var(--cyan));cursor:pointer}.btn.secondary{background:#132c46;color:#d9edff;border:1px solid var(--line)}
    .grid{display:grid;grid-template-columns:1.4fr .9fr;gap:16px}.panel{border:1px solid var(--line);border-radius:14px;padding:18px;background:linear-gradient(180deg,rgba(19,44,70,.94),rgba(9,21,35,.94));box-shadow:0 24px 60px rgba(0,0,0,.25)}
    .cards{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:12px}.card{padding:14px;border:1px solid var(--line);border-radius:12px;background:rgba(255,255,255,.04)}.tag{display:inline-flex;padding:5px 8px;border-radius:999px;background:rgba(36,150,237,.16);color:#bfe6ff;font-size:.72rem;font-weight:900;margin-bottom:10px}
    .metric{display:flex;justify-content:space-between;align-items:center;padding:12px;border:1px solid var(--line);border-radius:10px;background:rgba(255,255,255,.035);margin-bottom:10px}.metric b{font-size:1.35rem}
    table{width:100%%;border-collapse:collapse;margin-top:8px}td,th{text-align:left;padding:12px;border-bottom:1px solid var(--line);color:#d8e7f5}th{color:#8fb7d9;font-size:.78rem;text-transform:uppercase}.ok{color:var(--green);font-weight:900}.warn{color:var(--orange);font-weight:900}
    form{display:grid;grid-template-columns:1fr 1fr auto;gap:10px;margin-top:14px}input,select{border:1px solid var(--line);background:#081829;color:white;border-radius:10px;padding:12px}
    @media(max-width:900px){.app{display:block}.side{position:relative}.grid,.cards{grid-template-columns:1fr}.hero{flex-direction:column}form{grid-template-columns:1fr}main{padding:18px}}
  </style>
</head>
<body>
  <div class="app">
    <aside class="side"><div class="brand"><div class="logo">AD</div><span>%s</span></div><div class="nav"><button class="active">Vue generale</button><button>%s</button><button>Planning</button><button>Rapports</button><button>Parametres</button></div></aside>
    <main>
      <section class="hero"><div><div class="kicker">Prototype Mode Magic</div><h1>%s</h1><p class="lead">Demande traitee : %s</p></div><div><button class="btn">Nouvelle entree</button> <button class="btn secondary">Exporter</button></div></section>
      <section class="grid">
        <div class="panel"><h2>%s actifs</h2><div class="cards"><div class="card"><span class="tag">Priorite</span><h3>Suivi centralise</h3><p class="lead">Vue rapide des elements importants, statuts et prochaines actions.</p></div><div class="card"><span class="tag">Automatise</span><h3>Flux simplifie</h3><p class="lead">Formulaire, tri, et donnees plausibles pour presenter le parcours.</p></div><div class="card"><span class="tag">Equipe</span><h3>Collaboration</h3><p class="lead">Un espace clair pour partager decisions, notes et etats.</p></div></div><form><input placeholder="Nom"><select><option>Priorite haute</option><option>En cours</option><option>Archive</option></select><button class="btn" type="button">Ajouter</button></form><table><thead><tr><th>Element</th><th>Responsable</th><th>Statut</th></tr></thead><tbody><tr><td>%s principal</td><td>Equipe demo</td><td class="ok">Pret</td></tr><tr><td>Validation client</td><td>AutoDocker</td><td class="warn">A verifier</td></tr><tr><td>Publication</td><td>Ops</td><td class="ok">Planifie</td></tr></tbody></table></div>
        <div class="panel"><h2>Indicateurs</h2><div class="metric"><span>Total</span><b>128</b></div><div class="metric"><span>En cours</span><b>34</b></div><div class="metric"><span>Alertes</span><b>3</b></div><div class="metric"><span>Satisfaction</span><b>92%%</b></div></div>
      </section>
    </main>
  </div>
  <script>document.querySelectorAll('.nav button').forEach(btn=>btn.addEventListener('click',()=>{document.querySelectorAll('.nav button').forEach(b=>b.classList.remove('active'));btn.classList.add('active')}));</script>
</body>
</html>`, title, title, primary, title, safePrompt, primary, primary)
}

func prototypeTitle(prompt string, intent string) string {
	switch intent {
	case "crm":
		return "ClientFlow"
	case "reservation":
		return "ReserveHub"
	case "ecommerce":
		return "CommercePilot"
	case "dev_stack":
		return "DevForge"
	case "automation":
		return "FlowDesk"
	case "storage":
		return "StorageHub"
	case "game_server":
		return "GamePanel"
	default:
		if strings.TrimSpace(prompt) != "" {
			return "AutoDocker App"
		}
		return "Magic Prototype"
	}
}

func prototypePrimaryNoun(intent string) string {
	switch intent {
	case "crm":
		return "Clients"
	case "reservation":
		return "Reservations"
	case "ecommerce":
		return "Commandes"
	case "dev_stack":
		return "Projets"
	case "automation":
		return "Workflows"
	case "storage":
		return "Espaces"
	case "game_server":
		return "Serveurs"
	default:
		return "Elements"
	}
}

func shortSummary(text string, max int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= max {
		return text
	}
	return text[:max] + "..."
}

func escapeHTML(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(text)
}

func ensureLocalAIServices(prompt string, payload map[string]interface{}) {
	if !promptHasLocalAIIntent(prompt) {
		return
	}
	ui, _ := payload["ui"].(map[string]interface{})
	if ui == nil {
		ui = map[string]interface{}{}
	}
	ui["ai"] = true
	payload["ui"] = ui

	svcs, _ := payload["services"].([]interface{})
	existing := make(map[string]bool)
	for _, s := range svcs {
		if svc, ok := s.(map[string]interface{}); ok {
			if key, ok := svc["key"].(string); ok {
				existing[key] = true
			}
		}
	}
	if !existing["ollama"] {
		svcs = append(svcs, map[string]interface{}{"key": "ollama", "network": "back", "extra": map[string]string{"model": localAIModelForPrompt(prompt)}})
	}
	if !existing["open-webui"] {
		svcs = append(svcs, map[string]interface{}{"key": "open-webui", "network": "front"})
	}
	payload["services"] = svcs
}

func GenerateMagicConfig(prompt string) (*MagicAIResult, error) {
	return GenerateInfrastructureConfig(context.Background(), prompt)
}

// presetPreferredIntent lists intents whose deterministic preset already is
// the right architecture; other intents are routed to the AI (freeform mode)
// when a provider is configured, with the preset kept as fallback.
func presetPreferredIntent(intent string) bool {
	switch intent {
	case "local_ai", "blog", "collab", "soc", "monitoring", "course_platform",
		"dev_stack", "automation", "storage", "portfolio", "game_server", "browser_game", "generic_app":
		return true
	}
	return false
}

type magicProviderRunner struct {
	source   string
	generate func(string) (map[string]interface{}, error)
}

type magicProviderResult struct {
	source   string
	payload  map[string]interface{}
	warnings []string
	valid    bool
	err      error
}

func providerDisplayName(source string) string {
	switch source {
	case "local":
		return "Local"
	case "gemini":
		return "Gemini"
	case "openrouter":
		return "OpenRouter"
	default:
		return source
	}
}

// GenerateAIConfig calls Gemini with the infrastructure system prompt and returns parsed JSON.
func GenerateAIConfig(prompt string) (map[string]interface{}, error) {
	return generateInfrastructurePayload(prompt, "gemini")
}

func GenerateOpenRouterConfig(prompt string) (map[string]interface{}, error) {
	return generateInfrastructurePayload(prompt, "openrouter")
}

func magicInfraPrompt(prompt string) string {
	return prompt + "\n\nIMPORTANT: Verifie que les bases de donnees et leurs applications sont sur le MEME reseau. Si la demande necessite un logiciel hors catalogue, utilise le MODE LIBRE avec une image Docker publique connue. Si c'est une application web sur mesure sans logiciel existant, retourne une app prototype avec nginx."
}

func parseAIConfigText(text string) (map[string]interface{}, error) {
	cleaned := text
	re := regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")
	if m := re.FindStringSubmatch(cleaned); len(m) > 1 {
		cleaned = strings.TrimSpace(m[1])
	}
	idx := strings.Index(cleaned, "{")
	if idx > 0 {
		cleaned = cleaned[idx:]
	}
	lastIdx := strings.LastIndex(cleaned, "}")
	if lastIdx >= 0 {
		cleaned = cleaned[:lastIdx+1]
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(cleaned), &payload); err != nil {
		return nil, fmt.Errorf("JSON invalide recu de l'IA: %s", text[:minInt(len(text), 300)])
	}
	return payload, nil
}

func NormalizeMagicPayload(prompt string, payload map[string]interface{}) (map[string]interface{}, []string, bool) {
	if payload == nil {
		return genericAppPayload(), []string{"Payload IA vide."}, false
	}
	warnings := []string{}
	if _, ok := payload["addressing"].(map[string]interface{}); !ok {
		payload["addressing"] = magicPayload(map[string]interface{}{}, []map[string]interface{}{})["addressing"]
		warnings = append(warnings, "Adressage reseau ajoute automatiquement.")
	}

	services := normalizeServiceList(payload["services"])
	filtered := make([]map[string]interface{}, 0, len(services)+4)
	seen := map[string]bool{}
	hasFreeform := false
	for _, svc := range services {
		key, _ := svc["key"].(string)
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if key != "debian-router" && GetImage(key) == "" {
			// Freeform service: keep it when the AI provided a valid public image.
			if cleaned, ok := SanitizeFreeformSpec(key, svc); ok {
				if !seen[key] {
					filtered = append(filtered, cleaned)
					seen[key] = true
					hasFreeform = true
					warnings = append(warnings, "Service hors catalogue ajoute (mode libre): "+key)
				}
				continue
			}
			warnings = append(warnings, "Service ignore car absent du catalogue: "+key)
			continue
		}
		if svc["network"] == nil || strings.TrimSpace(fmt.Sprint(svc["network"])) == "" {
			svc["network"] = defaultNetworkForService(key)
		}
		if !seen[key] {
			filtered = append(filtered, svc)
			seen[key] = true
		}
	}
	addMissingService := func(key, network string) {
		if !seen[key] {
			filtered = append(filtered, map[string]interface{}{"key": key, "network": network})
			seen[key] = true
		}
	}
	addMissingService("portainer", "back")
	addMissingService("traefik", "front")
	if seen["wordpress"] || seen["nextcloud"] || seen["moodle"] {
		addMissingService("mariadb", "back")
	}
	if seen["gitea"] {
		addMissingService("postgres", "back")
	}
	if seen["open-webui"] {
		addMissingService("ollama", "back")
	}
	if promptHasLocalAIIntent(prompt) {
		if !seen["ollama"] {
			filtered = append(filtered, map[string]interface{}{"key": "ollama", "network": "back", "extra": map[string]string{"model": localAIModelForPrompt(prompt)}})
			seen["ollama"] = true
		}
		addMissingService("open-webui", "front")
	}
	// Freeform stacks (Kafka, Keycloak, game servers...) don't need a forced
	// web preview; only catalogue-only payloads get the nginx safety net.
	if !hasVisualService(seen) && !hasFreeform {
		addMissingService("nginx", "front")
		warnings = append(warnings, "Nginx ajoute pour garantir une preview web.")
	}
	payload["services"] = filtered
	payload["ui"] = uiForServices(filtered)
	return payload, warnings, len(filtered) > 0
}

func normalizeServiceList(raw interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	switch svcs := raw.(type) {
	case []map[string]interface{}:
		return svcs
	case []interface{}:
		for _, item := range svcs {
			if svc, ok := item.(map[string]interface{}); ok {
				out = append(out, svc)
			}
		}
	}
	return out
}

func defaultNetworkForService(key string) string {
	switch key {
	case "traefik", "nginx", "wordpress", "open-webui", "nextcloud", "dokuwiki", "moodle", "gitea", "n8n", "minio", "uptime-kuma", "mongo-express":
		return "front"
	default:
		return "back"
	}
}

func hasVisualService(seen map[string]bool) bool {
	for _, key := range []string{"nginx", "wordpress", "open-webui", "nextcloud", "dokuwiki", "grafana", "prometheus", "moodle", "gitea", "n8n", "minio", "uptime-kuma", "mongo-express"} {
		if seen[key] {
			return true
		}
	}
	return false
}

func uiForServices(services []map[string]interface{}) map[string]interface{} {
	ui := map[string]interface{}{"socle": true, "web": false, "db": false, "collab": false, "observ": false, "sec": false, "ai": false}
	for _, svc := range services {
		key, _ := svc["key"].(string)
		switch CategoryMap[key] {
		case "web":
			ui["web"] = true
		case "db":
			if key == "mongo-express" {
				ui["db"] = true
			}
		case "collab":
			ui["collab"] = true
		case "observ":
			ui["observ"] = true
		case "sec":
			ui["sec"] = true
		case "ai":
			ui["ai"] = true
		}
	}
	return ui
}

func extractTextFromGemini(result map[string]interface{}) string {
	candidates, _ := result["candidates"].([]interface{})
	if len(candidates) == 0 {
		return ""
	}
	cand, _ := candidates[0].(map[string]interface{})
	content, _ := cand["content"].(map[string]interface{})
	parts, _ := content["parts"].([]interface{})
	if len(parts) == 0 {
		return ""
	}
	part, _ := parts[0].(map[string]interface{})
	text, _ := part["text"].(string)
	// Strip markdown fences
	re := regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")
	if m := re.FindStringSubmatch(text); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(text)
}

// ProvisionPrompt is the system prompt for generating service configurations (files, content).
var ProvisionPrompt = `Tu es un Ingenieur DevOps expert. Tu recois une demande utilisateur et la liste des services deployes.
Tu dois generer les FICHIERS DE CONFIGURATION necessaires pour que les services fonctionnent selon la demande.

### REGLES
1. Reponds UNIQUEMENT en JSON valide, rien d'autre
2. Chaque fichier est un objet avec "container" (nom du conteneur), "path" (chemin dans le conteneur), "content" (contenu du fichier)
3. REGLE CRITIQUE : Pour un portfolio, landing page, ou site vitrine → utilise UNIQUEMENT le conteneur nginx avec des fichiers HTML/CSS/JS statiques dans /usr/share/nginx/html/. NE PAS utiliser WordPress pour un site statique.
4. Mets TOUT le CSS inline dans le fichier HTML (balise <style>) pour que ca marche immediatement sans fichier externe
5. Mets TOUT le JavaScript inline dans le HTML (balise <script>) pour la meme raison
6. Le HTML doit etre un SEUL fichier index.html complet avec tout le contenu, le style et les animations intégrées
7. Pour les bases de donnees : genere des scripts SQL d'initialisation si necessaire
8. Sois CREATIF et COMPLET — genere du VRAI contenu avec du vrai texte, pas des placeholders "Lorem ipsum"
9. Le design doit etre SPECTACULAIRE : dark theme (#0a0a0a), gradients lumineux, animations CSS (fade-in, slide-up, hover effects), glassmorphism, responsive
10. Inclus des sections completes avec du vrai contenu personnalise selon la demande de l'utilisateur
11. N'utilise WordPress QUE si l'utilisateur demande explicitement un blog ou un CMS
12. Pour une plateforme de cours, LMS, ecole, formation ou portail etudiants : genere un vrai prototype applicatif dans nginx avec catalogue de cours, espace etudiant, progression, devoirs, planning, ressources, tableau enseignant, et etats interactifs simples en JavaScript.
13. Pour un dashboard metier, CRM, portail client ou intranet : genere une interface dense et utilisable avec navigation, tableaux, filtres, cartes de statut, formulaires et donnees plausibles.
14. REGLE CONTENU PEDAGOGIQUE : si l'utilisateur demande des cours sur un SUJET precis (maths, physique, anglais, code...), genere de VRAIES lecons sur ce sujet : un index.html (catalogue) + une page HTML par cours dans /usr/share/nginx/html/cours/ (ex: cours/fractions.html, cours/equations.html). Chaque lecon contient un vrai cours structure : explications claires, formules, exemples resolus pas a pas, et exercices avec solutions cachees via <details><summary>. Le catalogue lie vers chaque lecon. Minimum 3 lecons completes sur le sujet demande.
15. REGLE MULTI-SITE : si l'utilisateur demande PLUSIEURS sites sur le meme nginx, place chaque site dans son propre sous-dossier (/usr/share/nginx/html/<nom-site>/index.html) et genere une page d'accueil racine /usr/share/nginx/html/index.html qui presente chaque site sous forme de carte cliquable (nom, description, lien vers /<nom-site>/). La page d'accueil racine sert de selecteur de sites.

### FORMAT DE SORTIE
{
  "files": [
    {
      "container": "projet-nginx",
      "path": "/usr/share/nginx/html/index.html",
      "content": "<!DOCTYPE html>...",
      "description": "Page d'accueil portfolio"
    },
    {
      "container": "projet-nginx",
      "path": "/usr/share/nginx/html/styles.css",
      "content": "body { ... }",
      "description": "Styles CSS"
    }
  ],
  "summary": "Portfolio web professionnel avec page de contact"
}

Genere du contenu REEL et COMPLET, pas des exemples minimaux.`

// GenerateAIProvision generates configuration files for deployed services.
// Retries with key rotation like RobustAICall: a single 429 must not leave
// the user with an empty nginx default page.
func GenerateAIProvision(prompt string, project string, containers []string) (map[string]interface{}, error) {
	fullPrompt := fmt.Sprintf(`Demande utilisateur : %s

Projet : %s
Conteneurs deployes : %s

Genere les fichiers de configuration pour que cette infrastructure fonctionne selon la demande.
Les noms de conteneurs a utiliser sont EXACTEMENT ceux listes ci-dessus.`, prompt, project, strings.Join(containers, ", "))

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		key := getNextKey()
		url := geminiGenerateURL(key)

		reqBody := geminiRequest{
			SystemInstruct: &geminiContent{Parts: []geminiPart{{Text: ProvisionPrompt}}},
			Contents:       []geminiContent{{Parts: []geminiPart{{Text: fullPrompt}}}},
			GenConfig:      &geminiGenConfig{Temperature: 0.7, ResponseMIME: "application/json"},
		}

		bodyBytes, _ := json.Marshal(reqBody)
		client := &http.Client{Timeout: 300 * time.Second}
		resp, err := client.Post(url, "application/json", bytes.NewReader(bodyBytes))
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}

		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("API status %d", resp.StatusCode)
			time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
			continue
		}
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("API status %d", resp.StatusCode)
			continue
		}

		var result map[string]interface{}
		if err := json.Unmarshal(data, &result); err != nil {
			lastErr = err
			continue
		}

		text := extractTextFromGemini(result)
		if text == "" {
			lastErr = fmt.Errorf("reponse vide")
			continue
		}

		payload, err := parseAIConfigText(text)
		if err != nil {
			lastErr = err
			continue
		}
		// A provision payload without files is useless: retry, then let the
		// caller fall back to a preset instead of claiming success.
		if files, ok := payload["files"].([]interface{}); !ok || len(files) == 0 {
			lastErr = fmt.Errorf("l'IA n'a retourne aucun fichier")
			continue
		}
		return payload, nil
	}
	return nil, fmt.Errorf("provision IA: echec apres 3 tentatives: %v", lastErr)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// AI compose editing — modify an existing compose.yaml from a natural
// language instruction ("ajoute un redis", "expose grafana sur le port 3500").
// ---------------------------------------------------------------------------

// ComposeEditPrompt is the system prompt for AI-assisted compose edits.
var ComposeEditPrompt = `Tu es un Ingenieur DevOps expert en Docker Compose.
Tu recois un fichier compose.yaml existant et une instruction de modification en langage naturel.
Tu retournes le fichier compose.yaml COMPLET et VALIDE apres modification.

### REGLES
1. Reponds UNIQUEMENT avec le YAML complet, sans markdown, sans explication.
2. Conserve TOUT ce qui n'est pas concerne par l'instruction : services, volumes, reseaux, ports, container_name, adresses IP existants.
3. SECURITE : jamais de "privileged: true", jamais de montage de chemins hote sensibles (/, /etc, docker.sock sur un nouveau service), jamais de "network_mode: host" sauf si deja present.
4. Si tu ajoutes un service : image publique connue avec tag explicite, volumes nommes uniquement, et declare le volume dans la section volumes.
5. Si tu ajoutes un service qui doit parler a un autre : mets-les sur le meme reseau.
6. Si l'instruction est ambigue, choisis l'interpretation la plus sure et la plus simple.`

// EditComposeWithAI returns the full modified compose YAML for an instruction.
func EditComposeWithAI(currentYAML, instruction string) (string, error) {
	userMsg := fmt.Sprintf("### COMPOSE ACTUEL\n%s\n\n### INSTRUCTION\n%s\n\nRetourne le compose.yaml complet modifie.", currentYAML, instruction)

	var text string
	var err error
	if AIAvailable() {
		text, err = CallGeminiChat(ComposeEditPrompt, userMsg)
	} else if OpenRouterAvailable() {
		text, err = CallOpenRouter(ComposeEditPrompt, userMsg)
	} else {
		return "", fmt.Errorf("aucun fournisseur IA configure")
	}
	if err != nil {
		return "", err
	}

	cleaned := stripYAMLFences(text)
	if strings.TrimSpace(cleaned) == "" {
		return "", fmt.Errorf("reponse IA vide")
	}
	if !strings.Contains(cleaned, "services:") {
		return "", fmt.Errorf("la reponse IA ne ressemble pas a un compose.yaml")
	}
	return cleaned, nil
}

func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// FileEditPrompt is the system prompt for AI-assisted edits of a single
// configuration or content file inside a container.
var FileEditPrompt = `Tu es un assistant d'edition de fichiers expert (HTML, CSS, JS, nginx.conf, YAML, SQL...).
Tu recois le contenu COMPLET d'un fichier et une instruction de modification.
Tu retournes le fichier COMPLET modifie, pret a etre sauvegarde.

### REGLES
1. Reponds UNIQUEMENT avec le contenu complet du fichier, sans markdown, sans explication.
2. Conserve tout ce qui n'est pas concerne par l'instruction (structure, styles, logique existante).
3. Respecte la syntaxe du format du fichier (HTML valide, conf nginx valide, etc.).
4. Si l'instruction est ambigue, choisis l'interpretation la plus simple et la plus sure.`

// EditFileWithAI returns the full modified content of a file for an instruction.
func EditFileWithAI(content, instruction string) (string, error) {
	userMsg := fmt.Sprintf("### FICHIER ACTUEL\n%s\n\n### INSTRUCTION\n%s\n\nRetourne le fichier complet modifie.", content, instruction)

	var text string
	var err error
	if AIAvailable() {
		text, err = CallGeminiChat(FileEditPrompt, userMsg)
	} else if OpenRouterAvailable() {
		text, err = CallOpenRouter(FileEditPrompt, userMsg)
	} else {
		return "", fmt.Errorf("aucun fournisseur IA configure")
	}
	if err != nil {
		return "", err
	}

	cleaned := stripCodeFences(text)
	if strings.TrimSpace(cleaned) == "" {
		return "", fmt.Errorf("reponse IA vide")
	}
	return cleaned, nil
}

// stripCodeFences removes a surrounding markdown code fence of any language.
func stripCodeFences(text string) string {
	re := regexp.MustCompile("(?s)```[a-zA-Z0-9_-]*\\s*(.*?)```")
	if m := re.FindStringSubmatch(text); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(text)
}

func stripYAMLFences(text string) string {
	re := regexp.MustCompile("(?s)```(?:yaml|yml)?\\s*(.*?)```")
	if m := re.FindStringSubmatch(text); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(text)
}

// ---------------------------------------------------------------------------
// Multi-site selector — deterministic nginx homepage that lists every site
// deployed in its own subdirectory.
// ---------------------------------------------------------------------------

// MultiSiteSelectorHTML builds a root index.html that lets the visitor pick
// one of the sites hosted in subdirectories of the same nginx.
func MultiSiteSelectorHTML(project string, sites []string) string {
	var cards strings.Builder
	for _, site := range sites {
		label := titleCase(strings.ReplaceAll(strings.ReplaceAll(site, "-", " "), "_", " "))
		cards.WriteString(fmt.Sprintf(`<a class="card" href="/%s/"><div class="icon">🌐</div><h2>%s</h2><p>Acceder au site %s</p><span class="go">Ouvrir →</span></a>`,
			escapeHTML(site), escapeHTML(label), escapeHTML(label)))
	}
	return fmt.Sprintf(`<!doctype html>
<html lang="fr">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>%s — Selection du site</title>
  <style>
    :root{color-scheme:dark;--bg:#07111f;--text:#f7fbff;--muted:#95a9bd;--docker:#2496ed;--cyan:#16c7f2;--line:rgba(255,255,255,.12)}
    *{box-sizing:border-box}body{margin:0;min-height:100vh;font-family:Inter,Segoe UI,system-ui,sans-serif;background:radial-gradient(circle at 20%% 0,#12355a 0,#07111f 45%%,#050b12 100%%);color:var(--text);display:grid;place-items:center;padding:32px}
    .wrap{max-width:1080px;width:100%%}.kicker{color:var(--cyan);font-weight:900;text-transform:uppercase;letter-spacing:.1em;font-size:.78rem}
    h1{font-size:clamp(2rem,5vw,3.6rem);margin:10px 0 8px}.lead{color:var(--muted);margin-bottom:28px}
    .grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:16px}
    .card{display:block;text-decoration:none;color:var(--text);border:1px solid var(--line);border-radius:14px;padding:22px;background:linear-gradient(180deg,rgba(19,42,66,.92),rgba(11,24,39,.92));transition:transform .15s,border-color .15s}
    .card:hover{transform:translateY(-4px);border-color:var(--docker)}
    .icon{font-size:1.8rem;margin-bottom:10px}.card h2{margin:0 0 6px;font-size:1.15rem}.card p{color:var(--muted);font-size:.9rem;margin:0 0 14px}
    .go{color:var(--cyan);font-weight:800;font-size:.85rem}
    footer{margin-top:28px;color:var(--muted);font-size:.78rem}
  </style>
</head>
<body>
  <div class="wrap">
    <div class="kicker">AutoDocker — Multi-site</div>
    <h1>Choisissez un site</h1>
    <p class="lead">Ce serveur nginx heberge plusieurs sites. Selectionnez celui que vous voulez consulter.</p>
    <div class="grid">%s</div>
    <footer>Projet %s — genere par AutoDocker Mode Magic</footer>
  </div>
</body>
</html>`, escapeHTML(project), cards.String(), escapeHTML(project))
}
