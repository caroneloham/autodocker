package services

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// IngressConfig publishes HTTP applications through one selected gateway.
// Domain is a DNS suffix, never a URL or configuration fragment.
type IngressConfig struct {
	TLS    string `json:"tls,omitempty"`
	Proxy  string `json:"proxy"`
	Domain string `json:"domain"`
	Port   int    `json:"port"`
}

var ingressDomainRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)
var runtimeModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*(?::[A-Za-z0-9][A-Za-z0-9._-]*)?$`)

func ValidateInfrastructureOptions(payload BuildComposePayload) error {
	if err := validateClientDelivery(payload); err != nil {
		return err
	}
	if err := validateProfessionalAI(payload); err != nil {
		return err
	}
	for _, s := range payload.Services {
		if s.Key == "moodle" {
			return fmt.Errorf("Le profil Moodle est temporairement indisponible : son image de référence n’existe plus. Choisissez un autre service ou attendez un profil maintenu")
		}
		if s.Key == "ollama" || s.Key == "open-webui" {
			model := s.Extra["model"]
			if model != "" && (len(model) > 255 || !runtimeModelRe.MatchString(model) || strings.Contains(model, "..")) {
				return fmt.Errorf("Modèle IA invalide : utilisez un identifiant Ollama ou hf.co/auteur/depot:quantification")
			}
		}
	}
	if payload.Ingress == nil {
		return nil
	}
	in := payload.Ingress
	if in.TLS != "" && in.TLS != "internal" {
		return fmt.Errorf("Mode TLS non pris en charge")
	}
	if in.TLS != "" && in.Proxy != "caddy" {
		return fmt.Errorf("Le HTTPS privé géré utilise Caddy")
	}
	if in.Proxy != "caddy" && in.Proxy != "traefik" && in.Proxy != "haproxy" {
		return fmt.Errorf("Reverse proxy non pris en charge")
	}
	if in.Domain == "" || len(in.Domain) > 190 || !ingressDomainRe.MatchString(in.Domain) {
		return fmt.Errorf("Domaine invalide : indiquez un nom sans protocole, chemin ni port")
	}
	if in.Port < 1024 || in.Port > 65535 {
		return fmt.Errorf("Le port du proxy doit être compris entre 1024 et 65535")
	}
	found := false
	for _, s := range payload.Services {
		if s.Key == in.Proxy {
			image := GetImage(s.Key)
			if image == "" {
				image = s.Image
			}
			if !ValidFreeformImage(image) {
				return fmt.Errorf("Le reverse proxy nécessite une image Docker valide")
			}
			found = true
		}
		if (s.Key == "caddy" || s.Key == "traefik" || s.Key == "haproxy" || s.Key == "nginx-proxy-manager") && s.Key != in.Proxy {
			return fmt.Errorf("Choisissez un seul reverse proxy pour le routage automatique")
		}
	}
	if !found {
		return fmt.Errorf("Le reverse proxy choisi doit faire partie des services")
	}
	return nil
}

// Known HTTP targets only. Databases, SSH and game ports are never HTTP routes.
var infrastructureHTTPPorts = map[string]int{
	"nginx": 80, "wordpress": 80, "nextcloud": 80, "dokuwiki": 8080, "moodle": 8080,
	"open-webui": 8080, "gitea": 3000, "forgejo": 3000, "n8n": 5678, "uptime-kuma": 3001,
	"loki": 3100, "grafana": 3000, "prometheus": 9090, "adminer": 8080, "pgadmin": 80, "mongo-express": 8081,
	"minio": 9001, "portainer": 9000, "rabbitmq": 15672, "meilisearch": 7700,
	"anythingllm": 3001, "hermes-agent": 8642, "qdrant": 6333, "victoriametrics": 8428, "ntfy": 80, "gotenberg": 3000, "whoami": 80,
}

func infrastructureTargetPort(key string, entry *ServiceEntry) int {
	if p := infrastructureHTTPPorts[key]; p > 0 {
		return p
	}
	if entry != nil {
		p, _ := strconv.Atoi(entry.Extra["http_port"])
		if p > 0 && p < 65536 {
			return p
		}
	}
	return 0
}

// Trusted templates also publish loopback-only ports. Keep freeform validation
// strict while letting ingress remove those direct application mappings.
func infrastructurePublishedPort(spec string) (int, int, bool) {
	if strings.HasPrefix(spec, "127.0.0.1:") {
		spec = strings.TrimPrefix(spec, "127.0.0.1:")
	}
	return sanitizePortSpec(spec)
}

func applyInfrastructureIngress(compose M, payload BuildComposePayload, entries map[string]*ServiceEntry, pt *buildPortTracker) {
	if payload.Ingress == nil || ValidateInfrastructureOptions(payload) != nil {
		return
	}
	in := payload.Ingress
	svcs := compose["services"].(M)
	proxy, ok := svcs[in.Proxy].(M)
	if !ok {
		return
	}
	keys := []string{}
	for key := range svcs {
		if key != in.Proxy && !privateGeneratedServices[key] && infrastructureTargetPort(key, entries[key]) > 0 {
			if config, active := professionalAIConfig(payload); active && (key == "qdrant" || key == "tei" || key == config.Engine) {
				continue
			}
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	// Release existing proxy/application HTTP mappings before allocating the gateway.
	for _, key := range append([]string{in.Proxy}, keys...) {
		svc := svcs[key].(M)
		keep := []string{}
		target := infrastructureTargetPort(key, entries[key])
		for _, spec := range toStringList(svc["ports"]) {
			host, port, valid := infrastructurePublishedPort(spec)
			remove := key == in.Proxy || valid && (port == target || key == "portainer" && port == 9443 || key == "minio" && port == 9000)
			if remove {
				if valid {
					delete(pt.used, host)
				}
			} else {
				keep = append(keep, spec)
			}
		}
		if len(keep) == 0 {
			delete(svc, "ports")
		} else {
			svc["ports"] = keep
		}
	}
	proxyNets, _ := proxy["networks"].(M)
	if proxyNets == nil {
		proxyNets = M{}
		proxy["networks"] = proxyNets
	}
	if runtime, ok := svcs["ollama"].(M); ok {
		delete(runtime, "ports")
	}
	protocol, innerPort := "http", 80
	if in.TLS == "internal" {
		protocol = "https"
		innerPort = 443
	}
	hostPort := pt.getFreePort(in.Port)
	routes := []M{}
	for _, key := range keys {
		svc := svcs[key].(M)
		if key == "portainer" {
			svc["command"] = []string{"--http-enabled"}
		}
		nets, _ := svc["networks"].(M)
		for name := range nets {
			if _, exists := proxyNets[name]; !exists {
				proxyNets[name] = M{}
			}
		}
		host := strings.ReplaceAll(key, "_", "-") + "." + in.Domain
		routes = append(routes, M{"service": key, "host": host, "port": infrastructureTargetPort(key, entries[key])})
		if key == "minio" {
			routes = append(routes, M{"service": key, "host": "minio-api." + in.Domain, "port": 9000})
		}
		env, _ := svc["environment"].(M)
		if env != nil {
			if key == "nextcloud" {
				env["NEXTCLOUD_TRUSTED_DOMAINS"] = host + " localhost"
				env["OVERWRITEPROTOCOL"] = protocol
				trusted := []string{}
				for _, n := range payload.Addressing.Networks {
					if _, yes := nets[n.Name]; yes {
						trusted = append(trusted, n.CIDR)
					}
				}
				env["TRUSTED_PROXIES"] = strings.Join(trusted, " ")
			}
			if key == "open-webui" {
				env["WEBUI_AUTH"] = "True"
				env["ENABLE_LOGIN_FORM"] = "True"
				if config, active := professionalAIConfig(payload); !active || !config.PrivatePlatform {
					env["ENABLE_SIGNUP"] = "True"
				}
				env["WEBUI_URL"] = fmt.Sprintf("%s://%s:%d", protocol, host, hostPort)
				if protocol == "https" {
					env["WEBUI_SESSION_COOKIE_SECURE"] = "True"
					env["WEBUI_AUTH_COOKIE_SECURE"] = "True"
				}
			}
			if key == "minio" {
				env["MINIO_BROWSER_REDIRECT_URL"] = fmt.Sprintf("%s://%s:%d", protocol, host, hostPort)
			}
			if key == "n8n" {
				env["WEBHOOK_URL"] = fmt.Sprintf("%s://%s:%d/", protocol, host, hostPort)
				env["N8N_PROXY_HOPS"] = "1"
				env["N8N_SECURE_COOKIE"] = strconv.FormatBool(protocol == "https")
			}
		}
	}
	proxy["ports"] = []string{fmt.Sprintf("%d:%d", hostPort, innerPort)}
	if ai, active := professionalAIConfig(payload); active && ai.PrivatePlatform && svcs["open-webui"] != nil {
		proxy["depends_on"] = M{"open-webui": M{"condition": "service_healthy"}}
	}
	config := ""
	bin := ""
	args := ""
	switch in.Proxy {
	case "caddy":
		rules := []M{}
		for _, r := range routes {
			rules = append(rules, M{"match": []M{{"host": []string{r["host"].(string)}}}, "handle": []M{{"handler": "reverse_proxy", "upstreams": []M{{"dial": fmt.Sprintf("%s:%d", r["service"], r["port"])}}}}, "terminal": true})
		}
		rules = append(rules, M{"handle": []M{{"handler": "static_response", "status_code": 404, "body": "AutoDocker : utilisez le sous-domaine de votre application."}}})
		server := M{"listen": []string{fmt.Sprintf(":%d", innerPort)}, "routes": rules, "automatic_https": M{"disable": true}}
		apps := M{"http": M{"servers": M{"autodocker": server}}}
		if in.TLS == "internal" {
			server["automatic_https"] = M{"disable_redirects": true}
			server["tls_connection_policies"] = []M{{}}
			subjects := []string{}
			for _, route := range routes {
				subjects = append(subjects, route["host"].(string))
			}
			apps["tls"] = M{"automation": M{"policies": []M{{"subjects": subjects, "issuers": []M{{"module": "internal"}}}}}}
			vols := compose["volumes"].(M)
			vols["caddy_data"] = M{}
			vols["caddy_config"] = M{}
			proxy["volumes"] = []string{"caddy_data:/data", "caddy_config:/config"}
		}
		raw, _ := json.Marshal(M{"admin": M{"disabled": true}, "apps": apps})
		config = string(raw)
		bin = "caddy"
		args = "run --config /tmp/autodocker-proxy.conf"
	case "traefik":
		routers, backends := M{}, M{}
		for i, r := range routes {
			id := fmt.Sprintf("route%d", i)
			routers[id] = M{"rule": "Host(`" + r["host"].(string) + "`)", "entryPoints": []string{"web"}, "service": id}
			backends[id] = M{"loadBalancer": M{"servers": []M{{"url": fmt.Sprintf("http://%s:%d", r["service"], r["port"])}}}}
		}
		raw, _ := json.Marshal(M{"http": M{"routers": routers, "services": backends}})
		config = string(raw)
		bin = "traefik"
		args = "--providers.file.filename=/tmp/autodocker-proxy.conf --providers.docker=false --entrypoints.web.address=:80 --api.dashboard=false"
		// Traefik's file provider requires a supported extension.
		args = strings.ReplaceAll(args, ".conf", ".json")
		delete(proxy, "volumes")
	case "haproxy":
		var b strings.Builder
		b.WriteString("global\n  log stdout format raw local0\ndefaults\n  mode http\n  timeout connect 5s\n  timeout client 60s\n  timeout server 60s\n  timeout tunnel 1h\nresolvers docker\n  nameserver dns 127.0.0.11:53\n  resolve_retries 3\n  timeout retry 1s\n  hold valid 10s\nfrontend web\n  bind :80\n  option forwardfor\n  http-request set-header X-Forwarded-Proto http\n")
		for i, r := range routes {
			fmt.Fprintf(&b, "  use_backend app%d if { hdr(host),lower,field(1,:) -i %s }\n", i, r["host"])
		}
		b.WriteString("  default_backend missing\nbackend missing\n  http-request return status 404 content-type text/plain string AutoDocker\n")
		for i, r := range routes {
			fmt.Fprintf(&b, "backend app%d\n  server app %s:%d resolvers docker init-addr libc,none check\n", i, r["service"], r["port"])
		}
		config = b.String()
		bin = "haproxy"
		args = "-W -db -f /tmp/autodocker-proxy.conf"
	}
	path := "/tmp/autodocker-proxy.conf"
	if in.Proxy == "traefik" {
		path = "/tmp/autodocker-proxy.json"
	}
	proxy["environment"] = M{"AUTODOCKER_PROXY_CONFIG": config}
	// Static shell code: configuration is data, never interpolated into shell source.
	proxy["command"] = []string{"/bin/sh", "-ec", "printf '%s' \"$$AUTODOCKER_PROXY_CONFIG\" > " + path + "; exec " + bin + " " + args}
	metadata, _ := compose["x-autodocker"].(M)
	if metadata == nil {
		metadata = M{}
		compose["x-autodocker"] = metadata
	}
	metadata["ingress"] = M{"proxy": in.Proxy, "domain": in.Domain, "port": hostPort, "routes": routes, "protocol": protocol, "tls": in.TLS}
}

func applySelectedRuntimeModel(compose M, entries map[string]*ServiceEntry) {
	model := ""
	for _, key := range []string{"ollama", "open-webui"} {
		if s := entries[key]; s != nil && s.Extra["model"] != "" {
			model = s.Extra["model"]
		}
	}
	if model == "" {
		return
	}
	if !runtimeModelRe.MatchString(model) || len(model) > 255 || strings.Contains(model, "..") {
		return
	}
	svcs := compose["services"].(M)
	if web, ok := svcs["open-webui"].(M); ok {
		env, _ := web["environment"].(M)
		if env == nil {
			env = M{}
			web["environment"] = env
		}
		env["DEFAULT_MODELS"] = model
		env["DEFAULT_PINNED_MODELS"] = model

	}
	if _, exists := svcs["ollama-model-init"]; !exists {
		if ollama, ok := svcs["ollama"].(M); ok {
			svcs["ollama-model-init"] = M{"image": GetImage("ollama"), "restart": "no", "depends_on": M{"ollama": M{"condition": "service_healthy"}}, "entrypoint": []string{"ollama"}, "command": []string{"pull", model}, "environment": M{"OLLAMA_HOST": "http://ollama:11434", "OLLAMA_MODEL": model}, "networks": ollama["networks"]}
		}
	}
	if init, ok := svcs["ollama-model-init"].(M); ok {
		init["command"] = []string{"pull", model}
		env, _ := init["environment"].(M)
		if env != nil {
			env["OLLAMA_MODEL"] = model
		}
	}
}
