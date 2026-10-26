package services

import (
	"context"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// ---------------------------------------------------------------------------
// Constants & CIS definitions
// ---------------------------------------------------------------------------

var severityWeight = map[string]int{
	"CRITICAL": 10, "HIGH": 7, "MEDIUM": 4, "LOW": 2, "INFO": 0,
}

var dangerousCaps = map[string]bool{
	"SYS_ADMIN": true, "NET_ADMIN": true, "SYS_PTRACE": true,
	"SYS_RAWIO": true, "DAC_OVERRIDE": true, "FOWNER": true,
	"SETUID": true, "SETGID": true,
}

var sensitiveMountPaths = []string{"/etc", "/var/run/docker.sock", "/proc", "/sys", "/dev"}

var catLabels = map[string]string{
	"privileges": "Privileges",
	"filesystem": "Systeme de fichiers",
	"resources":  "Ressources",
	"reliability": "Fiabilite",
	"network":    "Reseau",
	"isolation":  "Isolation",
}

// CISCheckDef describes one CIS benchmark check.
type CISCheckDef struct {
	ID       string
	Title    string
	Severity string
	Category string
	Fn       func(types.ContainerJSON) CheckResult
}

// CheckResult holds the outcome of a single CIS check.
type CheckResult struct {
	Passed      bool   `json:"passed"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation"`
}

// AuditCheck is a single check result with metadata.
type AuditCheck struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	Passed      bool   `json:"passed"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation"`
}

// AuditContainer holds audit results for one container.
type AuditContainer struct {
	Name   string       `json:"name"`
	Image  string       `json:"image"`
	Status string       `json:"status"`
	Checks []AuditCheck `json:"checks"`
	Passed int          `json:"passed"`
	Failed int          `json:"failed"`
	Total  int          `json:"total"`
}

// AuditCategorySummary tracks pass/fail per category.
type AuditCategorySummary struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
	Total  int `json:"total"`
}

// AuditSummary is the aggregate audit summary.
type AuditSummary struct {
	TotalContainers int                         `json:"total_containers"`
	TotalChecks     int                         `json:"total_checks"`
	TotalPassed     int                         `json:"total_passed"`
	TotalFailed     int                         `json:"total_failed"`
	Score           int                         `json:"score"`
	Categories      map[string]AuditCategorySummary `json:"categories"`
}

// AuditData is the full audit result.
type AuditData struct {
	Timestamp  string           `json:"timestamp"`
	Containers []AuditContainer `json:"containers"`
	Summary    AuditSummary     `json:"summary"`
}

// CIS checks list
var cisChecks = []CISCheckDef{
	{"CIS-5.1", "Mode privilegied desactive", "CRITICAL", "privileges", checkPrivilegedMode},
	{"CIS-5.2", "Utilisateur non-root", "HIGH", "privileges", checkNonRootUser},
	{"CIS-5.3", "Capacites ajoutees minimales", "HIGH", "privileges", checkCapabilitiesAdded},
	{"CIS-5.4", "cap_drop ALL configure", "MEDIUM", "privileges", checkCapabilitiesDropped},
	{"CIS-5.5", "Rootfs lecture seule", "MEDIUM", "filesystem", checkReadonlyRootfs},
	{"CIS-5.6", "no-new-privileges active", "HIGH", "privileges", checkNoNewPrivileges},
	{"CIS-5.7", "Limite memoire definie", "MEDIUM", "resources", checkMemoryLimit},
	{"CIS-5.8", "Limite CPU definie", "MEDIUM", "resources", checkCPULimit},
	{"CIS-5.9", "Limite PID definie", "LOW", "resources", checkPidsLimit},
	{"CIS-5.10", "Healthcheck configure", "MEDIUM", "reliability", checkHealthcheck},
	{"CIS-5.11", "Restart policy securisee", "LOW", "reliability", checkRestartPolicy},
	{"CIS-5.12", "Reseau non-host", "HIGH", "network", checkNetworkMode},
	{"CIS-5.13", "IPC non-host", "HIGH", "isolation", checkIPCMode},
	{"CIS-5.14", "PID non-host", "HIGH", "isolation", checkPIDMode},
	{"CIS-5.15", "Montages sensibles proteges", "CRITICAL", "filesystem", checkSensitiveMounts},
	// --- Extended security checks ---
	{"CIS-5.16", "Ports privileges non exposes", "HIGH", "network", checkPrivilegedPorts},
	{"CIS-5.17", "Variables d'env sans secrets", "CRITICAL", "privileges", checkEnvSecrets},
	{"CIS-5.18", "Image tag non 'latest'", "MEDIUM", "reliability", checkImageTag},
	{"CIS-5.19", "Docker socket non monte", "CRITICAL", "privileges", checkDockerSocket},
	{"CIS-5.20", "Pas de SYS_ADMIN capability", "CRITICAL", "privileges", checkSysAdmin},
	{"CIS-5.21", "Ulimits configures (nofile)", "LOW", "resources", checkUlimits},
	{"CIS-5.22", "Log driver configure", "MEDIUM", "reliability", checkLogDriver},
	{"CIS-5.23", "Tmpfs pour /tmp", "LOW", "filesystem", checkTmpfs},
	{"CIS-5.24", "Conteneur non en pause", "LOW", "reliability", checkNotPaused},
	{"CIS-5.25", "Labels presents (metadata)", "LOW", "reliability", checkLabels},
	{"CIS-5.26", "Ports exposes necessaires", "MEDIUM", "network", checkExposedPorts},
	{"CIS-5.27", "AppArmor ou Seccomp actif", "HIGH", "isolation", checkSecurityProfile},
	{"CIS-5.28", "Pas de --privileged-like flags", "HIGH", "isolation", checkPrivilegedFlags},
	{"CIS-5.29", "Volume avec driver local", "LOW", "filesystem", checkVolumeDriver},
	{"CIS-5.30", "Pas d'env MYSQL_ROOT_PASSWORD visible", "HIGH", "privileges", checkDBPasswords},
}

// ---------------------------------------------------------------------------
// Individual check functions
// ---------------------------------------------------------------------------

func cr(passed bool, detail, remediation string) CheckResult {
	return CheckResult{Passed: passed, Detail: detail, Remediation: remediation}
}

func checkPrivilegedMode(j types.ContainerJSON) CheckResult {
	if j.HostConfig != nil && j.HostConfig.Privileged {
		return cr(false, "Conteneur en mode privilegied (acces complet au kernel)",
			"Supprimer 'privileged: true'. Utiliser les capabilities specifiques necessaires.")
	}
	return cr(true, "Mode privilegied desactive", "")
}

func checkNonRootUser(j types.ContainerJSON) CheckResult {
	user := ""
	if j.Config != nil {
		user = j.Config.User
	}
	if user == "" || user == "0" || user == "root" {
		label := user
		if label == "" {
			label = "non defini"
		}
		return cr(false, fmt.Sprintf("Tourne en root (User='%s')", label),
			"Ajouter 'user: \"1000:1000\"' ou definir un USER non-root dans le Dockerfile.")
	}
	return cr(true, fmt.Sprintf("Utilisateur non-root: %s", user), "")
}

func checkCapabilitiesAdded(j types.ContainerJSON) CheckResult {
	if j.HostConfig == nil {
		return cr(true, "Aucune capability ajoutee", "")
	}
	capAdd := j.HostConfig.CapAdd
	var dangerous []string
	for _, c := range capAdd {
		if dangerousCaps[c] {
			dangerous = append(dangerous, c)
		}
	}
	if len(dangerous) > 0 {
		return cr(false, fmt.Sprintf("Capabilities dangereuses: %s", strings.Join(dangerous, ", ")),
			"Supprimer les capabilities dangereuses. Utiliser cap_drop: [ALL] + cap_add minimal.")
	}
	if len(capAdd) > 0 {
		caps := make([]string, len(capAdd))
		for i, c := range capAdd {
			caps[i] = c
		}
		return cr(true, fmt.Sprintf("Capabilities ajoutees (non dangereuses): %s", strings.Join(caps, ", ")), "")
	}
	return cr(true, "Aucune capability ajoutee", "")
}

func checkCapabilitiesDropped(j types.ContainerJSON) CheckResult {
	if j.HostConfig == nil {
		return cr(false, "Aucune capability supprimee (cap_drop absent)",
			"Ajouter 'cap_drop: [ALL]' dans la configuration du service.")
	}
	capDrop := j.HostConfig.CapDrop
	for _, c := range capDrop {
		if strings.EqualFold(c, "ALL") {
			return cr(true, "cap_drop: ALL configure", "")
		}
	}
	if len(capDrop) > 5 {
		return cr(true, fmt.Sprintf("%d capabilities supprimees", len(capDrop)), "")
	}
	if len(capDrop) > 0 {
		return cr(false, fmt.Sprintf("Seulement %d capabilities supprimees", len(capDrop)),
			"Utiliser 'cap_drop: [ALL]' et reajouter uniquement les capabilities necessaires.")
	}
	return cr(false, "Aucune capability supprimee (cap_drop absent)",
		"Ajouter 'cap_drop: [ALL]' dans la configuration du service.")
}

func checkReadonlyRootfs(j types.ContainerJSON) CheckResult {
	if j.HostConfig != nil && j.HostConfig.ReadonlyRootfs {
		return cr(true, "Systeme de fichiers racine en lecture seule", "")
	}
	return cr(false, "Rootfs en ecriture (read_only non active)",
		"Ajouter 'read_only: true' et utiliser des tmpfs pour les repertoires temporaires.")
}

func checkNoNewPrivileges(j types.ContainerJSON) CheckResult {
	if j.HostConfig != nil {
		for _, opt := range j.HostConfig.SecurityOpt {
			if strings.Contains(opt, "no-new-privileges") {
				return cr(true, "no-new-privileges active", "")
			}
		}
	}
	return cr(false, "no-new-privileges non defini",
		"Ajouter 'security_opt: [no-new-privileges]' au service.")
}

func checkMemoryLimit(j types.ContainerJSON) CheckResult {
	if j.HostConfig != nil && j.HostConfig.Memory > 0 {
		mb := j.HostConfig.Memory / (1024 * 1024)
		return cr(true, fmt.Sprintf("Limite memoire: %d MB", mb), "")
	}
	return cr(false, "Aucune limite memoire",
		"Ajouter 'mem_limit: 512m' (adapter selon le service).")
}

func checkCPULimit(j types.ContainerJSON) CheckResult {
	if j.HostConfig == nil {
		return cr(false, "Aucune limite CPU", "Ajouter 'cpus: 0.5' au service.")
	}
	if j.HostConfig.NanoCPUs > 0 {
		cpus := float64(j.HostConfig.NanoCPUs) / 1e9
		return cr(true, fmt.Sprintf("Limite CPU: %.2f CPUs", cpus), "")
	}
	if j.HostConfig.CPUQuota > 0 {
		return cr(true, fmt.Sprintf("CPU quota: %d", j.HostConfig.CPUQuota), "")
	}
	if j.HostConfig.CPUShares > 0 && j.HostConfig.CPUShares != 1024 {
		return cr(true, fmt.Sprintf("CPU shares: %d", j.HostConfig.CPUShares), "")
	}
	return cr(false, "Aucune limite CPU", "Ajouter 'cpus: 0.5' au service.")
}

func checkPidsLimit(j types.ContainerJSON) CheckResult {
	if j.HostConfig != nil && j.HostConfig.PidsLimit != nil && *j.HostConfig.PidsLimit > 0 {
		return cr(true, fmt.Sprintf("Limite PID: %d", *j.HostConfig.PidsLimit), "")
	}
	return cr(false, "Aucune limite PID", "Ajouter 'pids_limit: 200'.")
}

func checkHealthcheck(j types.ContainerJSON) CheckResult {
	if j.Config != nil && j.Config.Healthcheck != nil && len(j.Config.Healthcheck.Test) > 0 {
		cmd := strings.Join(j.Config.Healthcheck.Test, " ")
		if len(cmd) > 80 {
			cmd = cmd[:80]
		}
		return cr(true, fmt.Sprintf("Healthcheck: %s", cmd), "")
	}
	return cr(false, "Aucun healthcheck configure",
		"Ajouter un healthcheck: test, interval, timeout, retries.")
}

func checkRestartPolicy(j types.ContainerJSON) CheckResult {
	policy := ""
	if j.HostConfig != nil {
		policy = string(j.HostConfig.RestartPolicy.Name)
	}
	if policy == "always" {
		return cr(false, "Politique 'always' (redemarre meme apres arret manuel)",
			"Utiliser 'restart: unless-stopped' ou 'on-failure'.")
	}
	if policy == "unless-stopped" || policy == "on-failure" {
		return cr(true, fmt.Sprintf("Politique: %s", policy), "")
	}
	if policy == "" {
		policy = "no"
	}
	return cr(true, fmt.Sprintf("Politique: %s", policy), "")
}

func checkNetworkMode(j types.ContainerJSON) CheckResult {
	net := ""
	if j.HostConfig != nil {
		net = string(j.HostConfig.NetworkMode)
	}
	if net == "host" {
		return cr(false, "Reseau host (pas d'isolation reseau)",
			"Utiliser un reseau bridge dedie.")
	}
	return cr(true, fmt.Sprintf("Mode reseau: %s", net), "")
}

func checkIPCMode(j types.ContainerJSON) CheckResult {
	ipc := ""
	if j.HostConfig != nil {
		ipc = string(j.HostConfig.IpcMode)
	}
	if ipc == "host" {
		return cr(false, "IPC host partage", "Supprimer 'ipc: host'.")
	}
	if ipc == "" {
		ipc = "private"
	}
	return cr(true, fmt.Sprintf("IPC: %s", ipc), "")
}

func checkPIDMode(j types.ContainerJSON) CheckResult {
	pid := ""
	if j.HostConfig != nil {
		pid = string(j.HostConfig.PidMode)
	}
	if pid == "host" {
		return cr(false, "PID host partage", "Supprimer 'pid: host'.")
	}
	if pid == "" {
		pid = "private"
	}
	return cr(true, fmt.Sprintf("PID: %s", pid), "")
}

func checkSensitiveMounts(j types.ContainerJSON) CheckResult {
	var issues []string
	if j.Mounts != nil {
		for _, m := range j.Mounts {
			dest := m.Destination
			rw := m.RW
			src := m.Source
			if src == "" {
				src = m.Name
			}
			for _, s := range sensitiveMountPaths {
				if (strings.HasPrefix(dest, s) || strings.HasSuffix(src, "docker.sock")) && rw {
					issues = append(issues, fmt.Sprintf("%s -> %s (RW)", src, dest))
				}
			}
		}
	}
	if len(issues) > 0 {
		return cr(false, fmt.Sprintf("Montages sensibles RW: %s", strings.Join(issues, "; ")),
			"Monter en lecture seule (:ro). Ex: docker.sock:/var/run/docker.sock:ro")
	}
	return cr(true, "Pas de montage sensible en ecriture", "")
}

// ---------------------------------------------------------------------------
// Extended check functions (CIS-5.16 to CIS-5.30)
// ---------------------------------------------------------------------------

func checkPrivilegedPorts(j types.ContainerJSON) CheckResult {
	if j.HostConfig == nil || j.HostConfig.PortBindings == nil {
		return cr(true, "Aucun port expose", "")
	}
	var privPorts []string
	for port, bindings := range j.HostConfig.PortBindings {
		for _, b := range bindings {
			p := b.HostPort
			if p == "" {
				continue
			}
			pInt := 0
			fmt.Sscanf(p, "%d", &pInt)
			if pInt > 0 && pInt < 1024 {
				privPorts = append(privPorts, fmt.Sprintf("%s->%s", b.HostPort, port))
			}
		}
	}
	if len(privPorts) > 0 {
		return cr(false, fmt.Sprintf("Ports privileges exposes: %s", strings.Join(privPorts, ", ")),
			"Utiliser des ports > 1024 et configurer un reverse proxy pour les ports 80/443.")
	}
	return cr(true, "Aucun port privilege expose (<1024)", "")
}

func checkEnvSecrets(j types.ContainerJSON) CheckResult {
	if j.Config == nil {
		return cr(true, "Pas de variables d'environnement", "")
	}
	secretKeywords := []string{"PASSWORD", "SECRET", "API_KEY", "TOKEN", "PRIVATE_KEY", "APIKEY"}
	var found []string
	for _, env := range j.Config.Env {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToUpper(parts[0])
		val := parts[1]
		for _, kw := range secretKeywords {
			if strings.Contains(key, kw) && val != "" && val != "changeme" && len(val) > 3 {
				found = append(found, parts[0])
				break
			}
		}
	}
	if len(found) > 0 {
		return cr(false, fmt.Sprintf("Secrets en clair dans env: %s", strings.Join(found, ", ")),
			"Utiliser Docker Secrets ou un fichier .env avec des permissions restrictives. Ne jamais stocker de mots de passe dans docker-compose.yaml.")
	}
	return cr(true, "Pas de secret detecte dans les variables d'environnement", "")
}

func checkImageTag(j types.ContainerJSON) CheckResult {
	img := ""
	if j.Config != nil {
		img = j.Config.Image
	}
	if strings.HasSuffix(img, ":latest") || (!strings.Contains(img, ":") && !strings.Contains(img, "@sha256:")) {
		return cr(false, fmt.Sprintf("Image utilise le tag 'latest' ou pas de tag: %s", img),
			"Epingler une version specifique. Ex: nginx:1.25-alpine au lieu de nginx:latest")
	}
	return cr(true, fmt.Sprintf("Image avec tag specifique: %s", img), "")
}

func checkDockerSocket(j types.ContainerJSON) CheckResult {
	if j.Mounts != nil {
		for _, m := range j.Mounts {
			if strings.Contains(m.Source, "docker.sock") || strings.Contains(m.Destination, "docker.sock") {
				if m.RW {
					return cr(false, "Docker socket monte en lecture-ecriture",
						"Monter en lecture seule (:ro) ou utiliser un proxy Docker (socket-proxy).")
				}
				return cr(true, "Docker socket monte en lecture seule", "")
			}
		}
	}
	return cr(true, "Docker socket non monte", "")
}

func checkSysAdmin(j types.ContainerJSON) CheckResult {
	if j.HostConfig == nil {
		return cr(true, "Pas de SYS_ADMIN", "")
	}
	for _, c := range j.HostConfig.CapAdd {
		if strings.EqualFold(c, "SYS_ADMIN") {
			return cr(false, "Capability SYS_ADMIN ajoutee (quasi-equivalent a --privileged)",
				"Supprimer SYS_ADMIN. Cette capability donne un acces presque total au systeme hote.")
		}
	}
	return cr(true, "Pas de SYS_ADMIN capability", "")
}

func checkUlimits(j types.ContainerJSON) CheckResult {
	if j.HostConfig != nil && len(j.HostConfig.Ulimits) > 0 {
		var names []string
		for _, u := range j.HostConfig.Ulimits {
			names = append(names, u.Name)
		}
		return cr(true, fmt.Sprintf("Ulimits: %s", strings.Join(names, ", ")), "")
	}
	return cr(false, "Aucun ulimit configure",
		"Ajouter des ulimits pour prevenir les fork bombs: ulimits: { nofile: { soft: 65536, hard: 65536 }, nproc: { soft: 4096, hard: 4096 } }")
}

func checkLogDriver(j types.ContainerJSON) CheckResult {
	if j.HostConfig != nil && j.HostConfig.LogConfig.Type != "" && j.HostConfig.LogConfig.Type != "none" {
		return cr(true, fmt.Sprintf("Log driver: %s", j.HostConfig.LogConfig.Type), "")
	}
	if j.HostConfig != nil && j.HostConfig.LogConfig.Type == "none" {
		return cr(false, "Log driver desactive (none)",
			"Configurer un log driver: json-file, syslog, ou fluentd avec rotation.")
	}
	return cr(true, "Log driver par defaut (json-file)", "")
}

func checkTmpfs(j types.ContainerJSON) CheckResult {
	if j.HostConfig != nil && j.HostConfig.ReadonlyRootfs {
		hasTmp := false
		if j.HostConfig.Tmpfs != nil {
			for path := range j.HostConfig.Tmpfs {
				if strings.HasPrefix(path, "/tmp") || strings.HasPrefix(path, "/run") {
					hasTmp = true
					break
				}
			}
		}
		if !hasTmp {
			return cr(false, "Rootfs read-only mais pas de tmpfs pour /tmp",
				"Ajouter 'tmpfs: [\"/tmp\", \"/run\"]' pour les repertoires temporaires.")
		}
		return cr(true, "Tmpfs configure pour /tmp", "")
	}
	return cr(true, "Rootfs en ecriture (tmpfs non necessaire)", "")
}

func checkNotPaused(j types.ContainerJSON) CheckResult {
	if j.State != nil && j.State.Paused {
		return cr(false, "Conteneur en pause", "Reprendre ou supprimer le conteneur en pause.")
	}
	return cr(true, "Conteneur actif", "")
}

func checkLabels(j types.ContainerJSON) CheckResult {
	if j.Config != nil && len(j.Config.Labels) > 2 {
		return cr(true, fmt.Sprintf("%d labels definis", len(j.Config.Labels)), "")
	}
	return cr(false, "Peu ou pas de labels (metadata manquante)",
		"Ajouter des labels: maintainer, version, description pour la tracabilite.")
}

func checkExposedPorts(j types.ContainerJSON) CheckResult {
	if j.HostConfig == nil || j.HostConfig.PortBindings == nil {
		return cr(true, "Aucun port expose sur l'hote", "")
	}
	count := len(j.HostConfig.PortBindings)
	if count > 5 {
		return cr(false, fmt.Sprintf("%d ports exposes (surface d'attaque elevee)", count),
			"Reduire le nombre de ports exposes. Utiliser un reverse proxy pour consolider.")
	}
	// Check if binding to 0.0.0.0 (all interfaces)
	for _, bindings := range j.HostConfig.PortBindings {
		for _, b := range bindings {
			if b.HostIP == "" || b.HostIP == "0.0.0.0" {
				return cr(false, "Ports binds sur 0.0.0.0 (toutes les interfaces)",
					"Binder sur 127.0.0.1 si l'acces externe n'est pas necessaire: '127.0.0.1:8080:80'")
			}
		}
	}
	return cr(true, fmt.Sprintf("%d port(s) expose(s) correctement", count), "")
}

func checkSecurityProfile(j types.ContainerJSON) CheckResult {
	// On Windows Docker Desktop, seccomp/AppArmor are not supported — auto-pass
	if runtime.GOOS == "windows" {
		return cr(true, "Seccomp/AppArmor non applicable sur Windows Docker Desktop", "")
	}
	if j.HostConfig == nil {
		return cr(false, "Pas de profil AppArmor/Seccomp", "Activer seccomp ou AppArmor.")
	}
	hasSeccomp := false
	hasAppArmor := false
	for _, opt := range j.HostConfig.SecurityOpt {
		if strings.Contains(opt, "seccomp") {
			hasSeccomp = true
		}
		if strings.Contains(opt, "apparmor") {
			hasAppArmor = true
		}
	}
	if j.AppArmorProfile != "" {
		hasAppArmor = true
	}
	if hasSeccomp || hasAppArmor {
		profiles := []string{}
		if hasAppArmor {
			profiles = append(profiles, "AppArmor")
		}
		if hasSeccomp {
			profiles = append(profiles, "Seccomp")
		}
		return cr(true, fmt.Sprintf("Profil actif: %s", strings.Join(profiles, ", ")), "")
	}
	return cr(false, "Aucun profil de securite (AppArmor/Seccomp)",
		"Ajouter 'security_opt: [seccomp:default]' ou un profil AppArmor personnalise.")
}

func checkPrivilegedFlags(j types.ContainerJSON) CheckResult {
	if j.HostConfig == nil {
		return cr(true, "Pas de flags privilegies", "")
	}
	var issues []string
	if j.HostConfig.Privileged {
		issues = append(issues, "--privileged")
	}
	if j.HostConfig.UTSMode == "host" {
		issues = append(issues, "--uts=host")
	}
	if j.HostConfig.UsernsMode == "host" {
		issues = append(issues, "--userns=host")
	}
	if j.HostConfig.CgroupnsMode == "host" {
		issues = append(issues, "--cgroupns=host")
	}
	if len(issues) > 0 {
		return cr(false, fmt.Sprintf("Flags privilegies: %s", strings.Join(issues, ", ")),
			"Supprimer ces flags. Chacun reduit l'isolation du conteneur.")
	}
	return cr(true, "Pas de flags privilegies", "")
}

func checkVolumeDriver(j types.ContainerJSON) CheckResult {
	if j.Mounts == nil {
		return cr(true, "Pas de volumes", "")
	}
	for _, m := range j.Mounts {
		if m.Driver != "" && m.Driver != "local" {
			return cr(true, fmt.Sprintf("Driver de volume: %s", m.Driver), "")
		}
	}
	return cr(true, "Volumes avec driver local", "")
}

func checkDBPasswords(j types.ContainerJSON) CheckResult {
	if j.Config == nil {
		return cr(true, "Pas de variables d'environnement DB", "")
	}
	dbPasswords := []string{"MYSQL_ROOT_PASSWORD", "POSTGRES_PASSWORD", "MARIADB_ROOT_PASSWORD", "MONGO_INITDB_ROOT_PASSWORD"}
	for _, env := range j.Config.Env {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			continue
		}
		for _, dbp := range dbPasswords {
			if strings.EqualFold(parts[0], dbp) {
				val := parts[1]
				// Check password strength
				if len(val) < 12 {
					return cr(false, fmt.Sprintf("%s est trop faible (%d caracteres)", parts[0], len(val)),
						"Utiliser un mot de passe d'au moins 16 caracteres avec majuscules, chiffres et caracteres speciaux. Idealement, utiliser Docker Secrets.")
				}
				hasUpper := false
				hasDigit := false
				hasSpecial := false
				for _, c := range val {
					if c >= 'A' && c <= 'Z' {
						hasUpper = true
					}
					if c >= '0' && c <= '9' {
						hasDigit = true
					}
					if strings.ContainsRune("!@#$%^&*()_+-=[]{}|;':\",./<>?", c) {
						hasSpecial = true
					}
				}
				if !hasUpper || !hasDigit || !hasSpecial {
					return cr(false, fmt.Sprintf("%s manque de complexite (maj/chiffres/speciaux)", parts[0]),
						"Mot de passe DB doit contenir majuscules, chiffres et caracteres speciaux.")
				}
				return cr(true, fmt.Sprintf("%s avec complexite suffisante", parts[0]), "")
			}
		}
	}
	return cr(true, "Pas de mot de passe DB expose", "")
}

// ---------------------------------------------------------------------------
// Audit orchestrator
// ---------------------------------------------------------------------------

// RunAudit executes all CIS checks on the given container IDs.
// It inspects each container via the Docker API.
func RunAudit(cli *client.Client, project string) (*AuditData, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// List containers, filter by compose project label
	allContainers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("impossible de lister les conteneurs: %w", err)
	}

	var projectContainers []types.Container
	for _, c := range allContainers {
		if p, ok := c.Labels["com.docker.compose.project"]; ok {
			if strings.EqualFold(p, project) {
				projectContainers = append(projectContainers, c)
			}
		}
	}

	if len(projectContainers) == 0 {
		return nil, fmt.Errorf("aucun conteneur trouve pour le projet %s", project)
	}

	ts := time.Now().Format("2006-01-02 15:04:05")
	var results []AuditContainer
	totalChecks, totalPassed, totalWeight, passedWeight := 0, 0, 0, 0
	categories := make(map[string]AuditCategorySummary)

	for _, cont := range projectContainers {
		inspected, err := cli.ContainerInspect(ctx, cont.ID)
		if err != nil {
			continue
		}

		name := strings.TrimPrefix(cont.Names[0], "/")
		image := "<unknown>"
		if len(cont.Image) > 0 {
			image = cont.Image
		}

		var checks []AuditCheck
		passed, failed := 0, 0
		for _, cdef := range cisChecks {
			res := cdef.Fn(inspected)
			w := severityWeight[cdef.Severity]
			totalWeight += w
			totalChecks++
			if res.Passed {
				totalPassed++
				passedWeight += w
				passed++
			} else {
				failed++
			}

			checks = append(checks, AuditCheck{
				ID:          cdef.ID,
				Title:       cdef.Title,
				Severity:    cdef.Severity,
				Category:    cdef.Category,
				Passed:      res.Passed,
				Detail:      res.Detail,
				Remediation: res.Remediation,
			})

			cat := categories[cdef.Category]
			cat.Total++
			if res.Passed {
				cat.Passed++
			} else {
				cat.Failed++
			}
			categories[cdef.Category] = cat
		}

		state := cont.State
		results = append(results, AuditContainer{
			Name:   name,
			Image:  image,
			Status: state,
			Checks: checks,
			Passed: passed,
			Failed: failed,
			Total:  len(checks),
		})
	}

	score := 0
	if totalWeight > 0 {
		score = int(math.Round(float64(passedWeight) / float64(totalWeight) * 100))
	}

	return &AuditData{
		Timestamp:  ts,
		Containers: results,
		Summary: AuditSummary{
			TotalContainers: len(projectContainers),
			TotalChecks:     totalChecks,
			TotalPassed:     totalPassed,
			TotalFailed:     totalChecks - totalPassed,
			Score:           score,
			Categories:      categories,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// HTML report generation
// ---------------------------------------------------------------------------

func scoreColor(score int) string {
	if score >= 80 {
		return "#34d399"
	}
	if score >= 60 {
		return "#fbbf24"
	}
	if score >= 40 {
		return "#f97316"
	}
	return "#ef4444"
}

func sevColor(sev string) string {
	m := map[string]string{
		"CRITICAL": "#ef4444", "HIGH": "#f97316", "MEDIUM": "#fbbf24", "LOW": "#60a5fa",
	}
	if c, ok := m[sev]; ok {
		return c
	}
	return "#94a3b8"
}

// GenerateAuditHTML renders an HTML audit report from audit data.
func GenerateAuditHTML(data *AuditData, projectName, aiSummary string) string {
	s := data.Summary
	score := s.Score
	color := scoreColor(score)
	ts := data.Timestamp

	// Categories summary
	var catRows strings.Builder
	for catKey, catData := range s.Categories {
		label, ok := catLabels[catKey]
		if !ok {
			label = catKey
		}
		pct := 0
		if catData.Total > 0 {
			pct = int(math.Round(float64(catData.Passed) / float64(catData.Total) * 100))
		}
		barColor := scoreColor(pct)
		fmt.Fprintf(&catRows, `<tr>
			<td>%s</td>
			<td style="color:#10b981;">%d</td>
			<td style="color:#ef4444;">%d</td>
			<td><div style="background:#1e293b; border-radius:4px; overflow:hidden; height:8px; width:120px;">
				<div style="background:%s; height:100%%; width:%d%%;"></div>
			</div></td>
			<td style="color:%s; font-weight:bold;">%d%%</td>
		</tr>`, html.EscapeString(label), catData.Passed, catData.Failed, barColor, pct, barColor, pct)
	}

	// Container detail sections
	var containerSections strings.Builder
	for _, cr := range data.Containers {
		cScore := 0
		if cr.Total > 0 {
			cScore = int(math.Round(float64(cr.Passed) / float64(cr.Total) * 100))
		}
		cColor := scoreColor(cScore)
		statusColor := "#ef4444"
		if cr.Status == "running" {
			statusColor = "#10b981"
		}

		var checksRows strings.Builder
		for _, ch := range cr.Checks {
			icon := "&#10003;"
			iconColor := "#10b981"
			if !ch.Passed {
				icon = "&#10007;"
				iconColor = "#ef4444"
			}
			sevCol := sevColor(ch.Severity)
			remHTML := ""
			if !ch.Passed && ch.Remediation != "" {
				remHTML = fmt.Sprintf(`<div style="color:#94a3b8; font-size:0.8rem; margin-top:4px;">&#8627; %s</div>`,
					html.EscapeString(ch.Remediation))
			}
			fmt.Fprintf(&checksRows, `<tr>
				<td style="color:%s; font-size:1.2rem; text-align:center;">%s</td>
				<td style="font-family:monospace; font-size:0.8rem; color:#94a3b8;">%s</td>
				<td><span style="background:%s20; color:%s; padding:2px 8px; border-radius:4px; font-size:0.75rem;">%s</span></td>
				<td style="font-weight:600;">%s</td>
				<td style="font-size:0.85rem;">%s%s</td>
			</tr>`,
				iconColor, icon,
				html.EscapeString(ch.ID),
				sevCol, sevCol, html.EscapeString(ch.Severity),
				html.EscapeString(ch.Title),
				html.EscapeString(ch.Detail), remHTML)
		}

		fmt.Fprintf(&containerSections, `
		<div style="margin-top:30px;">
			<div style="display:flex; align-items:center; gap:15px; margin-bottom:15px;">
				<h3 style="margin:0; color:#f8fafc;">%s</h3>
				<span style="font-size:0.8rem; color:#94a3b8;">(%s)</span>
				<span style="background:%s20; color:%s; padding:2px 10px; border-radius:12px; font-size:0.75rem;">%s</span>
				<span style="margin-left:auto; font-size:1.1rem; font-weight:bold; color:%s;">%d%%</span>
			</div>
			<table style="width:100%%; border-collapse:separate; border-spacing:0; border-radius:8px; overflow:hidden;">
				<thead><tr style="background:#334155;">
					<th style="width:40px; padding:10px;"></th>
					<th style="padding:10px; font-size:0.8rem; color:#94a3b8;">ID</th>
					<th style="padding:10px; font-size:0.8rem; color:#94a3b8;">SEV</th>
					<th style="padding:10px; font-size:0.8rem; color:#94a3b8;">CHECK</th>
					<th style="padding:10px; font-size:0.8rem; color:#94a3b8;">DETAIL</th>
				</tr></thead>
				<tbody>%s</tbody>
			</table>
		</div>`,
			html.EscapeString(cr.Name),
			html.EscapeString(cr.Image),
			statusColor, statusColor, html.EscapeString(cr.Status),
			cColor, cScore,
			checksRows.String())
	}

	// AI summary section
	aiSection := ""
	if aiSummary != "" {
		aiSection = fmt.Sprintf(`
		<div style="margin-top:40px; padding:20px; background:#0f172a; border:1px solid #334155; border-radius:8px;">
			<h2 style="color:#a78bfa; border-left:5px solid #a78bfa; padding-left:15px;">Analyse IA complementaire</h2>
			<div style="color:#cbd5e1; line-height:1.8; white-space:pre-wrap;">%s</div>
		</div>`, html.EscapeString(aiSummary))
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="fr">
<head>
	<meta charset="UTF-8">
	<title>Audit CIS Docker - %s</title>
	<style>
		body { font-family: 'Segoe UI', system-ui, sans-serif; background: #0f172a; color: #e2e8f0; margin: 0; padding: 20px; }
		.container { max-width: 1200px; margin: auto; background: #1e293b; padding: 40px; border-radius: 16px; box-shadow: 0 25px 50px -12px rgba(0,0,0,0.5); }
		.header { display: flex; justify-content: space-between; align-items: center; border-bottom: 2px solid #334155; padding-bottom: 30px; margin-bottom: 30px; }
		.score-box { width: 140px; height: 140px; border-radius: 50%%; display: flex; align-items: center; justify-content: center; font-size: 40px; font-weight: 800; border: 8px solid %s; color: %s; background: #0f172a; box-shadow: 0 0 20px %s40; }
		h1 { margin: 0; font-size: 1.8rem; color: #f8fafc; }
		h2 { color: #38bdf8; border-left: 5px solid #38bdf8; padding-left: 15px; margin-top: 30px; }
		table { width: 100%%; border-collapse: separate; border-spacing: 0; margin-top: 10px; }
		th, td { padding: 12px; text-align: left; border-bottom: 1px solid #334155; }
		th { text-transform: uppercase; font-size: 0.75rem; letter-spacing: 1px; color: #94a3b8; }
		.summary-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 15px; margin: 20px 0; }
		.summary-card { background: #0f172a; padding: 20px; border-radius: 12px; text-align: center; border: 1px solid #334155; }
		.summary-card .value { font-size: 2rem; font-weight: 800; }
		.summary-card .label { font-size: 0.8rem; color: #94a3b8; margin-top: 5px; }
	</style>
</head>
<body>
	<div class="container">
		<div class="header">
			<div>
				<h1>Audit CIS Docker : %s</h1>
				<p style="color:#94a3b8; margin-top:8px;">Date : %s</p>
				<p style="color:#94a3b8; font-size:0.85rem;">%d conteneurs | %d checks | %d passes | %d echoues</p>
			</div>
			<div class="score-box">%d%%</div>
		</div>

		<div class="summary-grid">
			<div class="summary-card"><div class="value" style="color:#f8fafc;">%d</div><div class="label">Conteneurs</div></div>
			<div class="summary-card"><div class="value" style="color:#10b981;">%d</div><div class="label">Checks Passes</div></div>
			<div class="summary-card"><div class="value" style="color:#ef4444;">%d</div><div class="label">Checks Echoues</div></div>
			<div class="summary-card"><div class="value" style="color:%s;">%d%%</div><div class="label">Score Global</div></div>
		</div>

		<h2>Resume par Categorie</h2>
		<table>
			<thead><tr><th>Categorie</th><th>Passes</th><th>Echoues</th><th>Progression</th><th>Score</th></tr></thead>
			<tbody>%s</tbody>
		</table>

		<h2>Detail par Conteneur</h2>
		%s

		%s

		<div style="margin-top:40px; padding-top:20px; border-top:1px solid #334155; text-align:center; color:#475569; font-size:0.8rem;">
			AutoDocker Audit Engine | CIS Docker Benchmark | %s
		</div>
	</div>
</body>
</html>`,
		html.EscapeString(projectName),
		color, color, color,
		html.EscapeString(projectName), ts,
		s.TotalContainers, s.TotalChecks, s.TotalPassed, s.TotalFailed,
		score,
		s.TotalContainers, s.TotalPassed, s.TotalFailed,
		color, score,
		catRows.String(),
		containerSections.String(),
		aiSection,
		ts,
	)
}

// ---------------------------------------------------------------------------
// Hardening (placeholder -- real impl would use AI)
// ---------------------------------------------------------------------------

// HardeningResult holds the outcome of prepare-hardening.
type HardeningResult struct {
	Original   string `json:"original,omitempty"`
	Hardened   string `json:"hardened,omitempty"`
	Diff       string `json:"diff"`
	DiffLines  int    `json:"diff_lines"`
	BackupPath string `json:"backup_path"`
	Error      string `json:"error,omitempty"`
}

// PrepareHardening generates hardening suggestions using AI based on the compose file and audit results.
func PrepareHardening(composePath string) *HardeningResult {
	// Read the compose file
	data, err := os.ReadFile(composePath)
	if err != nil {
		return &HardeningResult{Error: "Impossible de lire le compose: " + err.Error()}
	}
	original := string(data)

	// Run audit to get current findings
	projectName := filepath.Base(filepath.Dir(composePath))
	auditData, _ := RunAudit(DockerClient, projectName)

	// Build findings summary for AI
	var findingSummary strings.Builder
	if auditData != nil {
		for _, ct := range auditData.Containers {
			for _, check := range ct.Checks {
				if !check.Passed {
					findingSummary.WriteString(fmt.Sprintf("- %s [%s] (%s): %s - %s\n", check.ID, ct.Name, check.Severity, check.Title, check.Remediation))
				}
			}
		}
	}

	// Platform-specific rules
	platformRules := ""
	if runtime.GOOS == "windows" {
		platformRules = `
IMPORTANT CONTRAINTES WINDOWS:
- NE PAS ajouter security_opt avec seccomp (non supporte sur Windows Docker Desktop)
- NE PAS ajouter de profil AppArmor (non supporte sur Windows)
- NE PAS ajouter pids_limit (peut ne pas fonctionner sur Windows)
- NE PAS mettre version: dans le compose (obsolete)
- Garder security_opt: [no-new-privileges:true] uniquement`
	}

	// Use AI to generate hardened compose
	prompt := fmt.Sprintf(`Tu es un expert en securite Docker. Voici un fichier docker-compose.yaml :

%s

Voici les problemes de securite detectes par l'audit CIS Docker Benchmark :
%s

Genere une version DURCIE de ce docker-compose.yaml qui corrige TOUS ces problemes.
Applique ces bonnes pratiques :
- Ajouter read_only: true quand possible (avec tmpfs pour /tmp et /run si necessaire)
- Ajouter security_opt: [no-new-privileges:true]
- Limiter les capabilities (cap_drop: [ALL], puis cap_add seulement ce qui est necessaire)
- Ajouter des limites memoire/CPU (mem_limit: 512m, cpus: '0.5')
- Ajouter des healthchecks pour chaque service
- Utiliser un user non-root quand possible (user: "1000:1000")
- Ne pas exposer de ports inutiles
- Remplacer les tags 'latest' par des versions fixees (ex: nginx:1.25-alpine)
- Si des mots de passe sont en clair, les renforcer (16+ caracteres, complexes)
- Ajouter des ulimits (nofile: 65536, nproc: 4096)
- Ajouter des labels de metadata (maintainer, version)
%s

IMPORTANT:
- Retourne UNIQUEMENT le YAML complet, SANS backticks, SANS commentaires markdown
- NE PAS mettre "version:" dans le compose (obsolete dans Docker Compose v2+)
- NE PAS utiliser Docker Secrets (secrets:, external: true) car on utilise Docker Compose standalone PAS Docker Swarm
- Garder les mots de passe dans environment: mais les renforcer (generer des mots de passe complexes)
- NE PAS ajouter de section "secrets:" au top-level
- Garder EXACTEMENT les memes container_name que l'original
- Garder EXACTEMENT les memes noms de reseaux et leur configuration IPAM`, original, findingSummary.String(), platformRules)

	hardened, err := RobustAICall(prompt)
	if err != nil {
		// Fallback: apply basic hardening rules without AI
		hardened = applyBasicHardening(original)
	}

	// Clean AI response (remove markdown fences if present)
	hardened = strings.TrimSpace(hardened)
	if strings.HasPrefix(hardened, "```") {
		lines := strings.Split(hardened, "\n")
		if len(lines) > 2 {
			lines = lines[1 : len(lines)-1] // Remove first and last lines
			hardened = strings.Join(lines, "\n")
		}
	}

	// Create backup
	backupPath := composePath + ".bak"
	os.WriteFile(backupPath, data, 0644)

	// Generate diff
	diff := generateSimpleDiff(original, hardened)

	return &HardeningResult{
		Original:   original,
		Hardened:   hardened,
		Diff:       diff,
		DiffLines:  strings.Count(diff, "\n"),
		BackupPath: backupPath,
	}
}

// applyBasicHardening applies security defaults without AI
func applyBasicHardening(compose string) string {
	// Parse YAML, add security options to each service
	lines := strings.Split(compose, "\n")
	var result []string
	inServices := false
	inService := false
	serviceIndent := 0

	for _, line := range lines {
		result = append(result, line)
		trimmed := strings.TrimSpace(line)

		if trimmed == "services:" {
			inServices = true
			continue
		}

		if inServices && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && trimmed != "" {
			inServices = false
		}

		if inServices && len(line) > 0 {
			indent := len(line) - len(strings.TrimLeft(line, " "))
			if indent == 2 && strings.HasSuffix(trimmed, ":") {
				inService = true
				serviceIndent = indent
			}
			if inService && trimmed == "restart: unless-stopped" {
				pad := strings.Repeat(" ", serviceIndent+2)
				result = append(result, pad+"security_opt:")
				result = append(result, pad+"  - no-new-privileges:true")
				result = append(result, pad+"read_only: true")
				result = append(result, pad+"mem_limit: 512m")
				result = append(result, pad+"cpus: '0.5'")
			}
		}
	}
	return strings.Join(result, "\n")
}

// generateSimpleDiff creates a unified-style diff
func generateSimpleDiff(original, modified string) string {
	origLines := strings.Split(original, "\n")
	modLines := strings.Split(modified, "\n")

	var diff strings.Builder
	diff.WriteString("--- compose.yaml (actuel)\n")
	diff.WriteString("+++ compose.yaml (durci)\n")

	maxLen := len(origLines)
	if len(modLines) > maxLen {
		maxLen = len(modLines)
	}

	for i := 0; i < maxLen; i++ {
		origLine := ""
		modLine := ""
		if i < len(origLines) {
			origLine = origLines[i]
		}
		if i < len(modLines) {
			modLine = modLines[i]
		}
		if origLine != modLine {
			if origLine != "" {
				diff.WriteString("- " + origLine + "\n")
			}
			if modLine != "" {
				diff.WriteString("+ " + modLine + "\n")
			}
		} else {
			diff.WriteString("  " + origLine + "\n")
		}
	}
	return diff.String()
}
