package services

import (
	"fmt"
	"net/url"
	"strings"
)

// Exported stacks default to host-local access. A deliberately configured
// ingress domain is the only HTTP gateway; data and inference APIs stay private.
var privateGeneratedServices = map[string]bool{
	"mariadb": true, "nextcloud-db": true, "postgres": true, "mongo": true,
	"redis": true, "memcached": true, "ollama": true, "vllm": true,
	"tei": true, "qdrant": true, "prometheus": true, "loki": true,
	"rabbitmq": true, "meilisearch": true, "victoriametrics": true,
}

func secureGeneratedDefaults(compose M, payload BuildComposePayload) {
	svcs := compose["services"].(M)
	publicProxy := ""
	if payload.Ingress != nil && payload.Ingress.Domain != "localhost" && !strings.HasSuffix(payload.Ingress.Domain, ".localhost") && ValidateInfrastructureOptions(payload) == nil {
		publicProxy = payload.Ingress.Proxy
	}
	for key, value := range svcs {
		svc, ok := value.(M)
		if !ok {
			continue
		}
		if privateGeneratedServices[key] {
			delete(svc, "ports")
		} else if key != publicProxy {
			ports := []string{}
			for _, spec := range toStringList(svc["ports"]) {
				// Trusted built-ins use short port syntax. Preserve protocol suffixes.
				parts := strings.Split(spec, ":")
				if len(parts) >= 2 {
					ports = append(ports, "127.0.0.1:"+strings.Join(parts[len(parts)-2:], ":"))
				}
			}
			if len(ports) > 0 {
				svc["ports"] = ports
			}
		}
		if key == "open-webui" {
			env, _ := svc["environment"].(M)
			if env == nil {
				env = M{}
				svc["environment"] = env
			}
			env["WEBUI_AUTH"], env["ENABLE_LOGIN_FORM"], env["ENABLE_SIGNUP"] = "True", "True", "False"
			if env["WEBUI_ADMIN_EMAIL"] == nil || env["WEBUI_ADMIN_EMAIL"] == "" {
				env["WEBUI_ADMIN_EMAIL"] = "admin@autodocker.local"
			}
			if env["WEBUI_ADMIN_PASSWORD"] == nil || env["WEBUI_ADMIN_PASSWORD"] == "" {
				env["WEBUI_ADMIN_PASSWORD"] = generatePassword(32)
			}
			if env["WEBUI_SECRET_KEY"] == nil || env["WEBUI_SECRET_KEY"] == "" {
				env["WEBUI_SECRET_KEY"] = generatePassword(40)
			}
		}
		if key == "grafana" {
			env, _ := svc["environment"].(M)
			if env == nil {
				env = M{}
				svc["environment"] = env
			}
			env["GF_AUTH_ANONYMOUS_ENABLED"] = "false"
			env["GF_USERS_ALLOW_SIGN_UP"] = "false"
			if env["GF_SECURITY_ADMIN_PASSWORD"] == nil || env["GF_SECURITY_ADMIN_PASSWORD"] == "admin" || env["GF_SECURITY_ADMIN_PASSWORD"] == "" {
				env["GF_SECURITY_ADMIN_PASSWORD"] = generatePassword(32)
			}
		}
		if key == "redis" && applicationCommandOption(svc, "--requirepass") == "" && svc["command"] == nil {
			svc["command"] = []string{"redis-server", "--appendonly", "yes", "--requirepass", generatePassword(32)}
		}
		if key == "mongo-express" {
			env, _ := svc["environment"].(M)
			if env == nil {
				env = M{}
				svc["environment"] = env
			}
			env["ME_CONFIG_BASICAUTH_ENABLED"] = "true"
			env["ME_CONFIG_BASICAUTH_USERNAME"] = "admin"
			env["ME_CONFIG_BASICAUTH_PASSWORD"] = generatePassword(32)
			delete(env, "ME_CONFIG_BASICAUTH")
			if db, ok := svcs["mongo"].(M); ok && (env["ME_CONFIG_MONGODB_URL"] == nil || env["ME_CONFIG_MONGODB_URL"] == "") {
				dbEnv, _ := db["environment"].(M)
				user, _ := dbEnv["MONGO_INITDB_ROOT_USERNAME"].(string)
				secret, _ := dbEnv["MONGO_INITDB_ROOT_PASSWORD"].(string)
				env["ME_CONFIG_MONGODB_URL"] = "mongodb://" + url.UserPassword(user, secret).String() + "@mongo:27017/?authSource=admin"
				connectApplicationNetwork(compose, svc, db)
				addApplicationDependency(svc, "mongo")
			}
		}
	}
	if web, ok := svcs["open-webui"].(M); ok && svcs["ollama-model-init"] != nil {
		deps, ok := web["depends_on"].(M)
		if !ok {
			deps = M{}
			for _, key := range toStringList(web["depends_on"]) {
				deps[key] = M{"condition": "service_started"}
			}
		}
		deps["ollama-model-init"] = M{"condition": "service_completed_successfully"}
		web["depends_on"] = deps
	}
	if app, ok := svcs["nextcloud"].(M); ok {
		env, _ := app["environment"].(M)
		if redis, ok := svcs["redis"].(M); ok && env["REDIS_HOST"] == "redis" && env["REDIS_HOST_PASSWORD_FILE"] == nil {
			if password := applicationCommandOption(redis, "--requirepass"); password != "" {
				env["REDIS_HOST_PASSWORD"] = password
			}
		}
	}
	metadata, _ := compose["x-autodocker"].(M)
	if metadata == nil {
		metadata = M{}
		compose["x-autodocker"] = metadata
	}
	metadata["security"] = M{"version": 1, "internal_ports_private": true, "host_access": "loopback_except_configured_ingress", "webui_authentication": true}
}

func GeneratedSecurityGuide(compose M) string {
	svcs, _ := compose["services"].(M)
	guide := "\n## Accès et sécurité\n\nLes bases, caches, moteurs IA et API techniques ne publient aucun port hôte. Les interfaces restantes écoutent sur 127.0.0.1. Un proxy avec un domaine explicitement configuré peut devenir la porte d’entrée du serveur. Les protocoles de jeu/SSH ne passent pas par un proxy HTTP : adaptez explicitement leur publication et le pare-feu sur la cible.\n\nLe fichier Compose et ses configs contiennent les secrets générés. Gardez l’archive privée ; ne la publiez pas dans Git. Les secrets changent à chaque génération : une nouvelle archive n’est pas une migration d’un volume existant.\n"
	if svcs["open-webui"] != nil {
		guide += "\nOpen WebUI : connexion obligatoire et inscriptions fermées. Le compte initial figure dans WEBUI_ADMIN_EMAIL / WEBUI_ADMIN_PASSWORD. Changez son mot de passe après connexion ; l’administrateur crée ensuite les comptes autorisés.\n"
	}
	if svcs["grafana"] != nil {
		guide += "\nGrafana : compte admin et mot de passe généré dans GF_SECURITY_ADMIN_PASSWORD ; accès anonyme et inscriptions désactivés.\n"
	}
	if configs, ok := compose["configs"].(M); ok && (configs["autodocker_mariadb_apps"] != nil || configs["autodocker_postgres_apps"] != nil) {
		guide += "\n## Comptes SQL\n\nChaque application gérée reçoit son compte et sa base dédiés, avec des droits limités à cette base. Les configs SQL sont montées dans /docker-entrypoint-initdb.d et exécutées seulement sur un volume de base neuf. Pour des données existantes, sauvegardez et appliquez une migration contrôlée ; ne supprimez pas vos volumes pour appliquer ces réglages.\n"
	}
	if meta, ok := compose["x-autodocker"].(M); ok {
		if ingress, ok := meta["ingress"].(M); ok {
			guide += fmt.Sprintf("\nProxy : %v, protocole %v, mode TLS %v. Le TLS internal utilise une autorité privée, pas un certificat public Let's Encrypt. Pour Internet : domaine/DNS, certificat public et pare-feu doivent être configurés et vérifiés.\n", ingress["proxy"], ingress["protocol"], ingress["tls"])
		}
	}
	return guide
}
