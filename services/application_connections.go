package services

import (
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
)

// Complete connections from the final service definitions, after user overrides.
// Explicit application endpoints and provisioning files remain authoritative.
func applyApplicationConnections(compose M) {
	services, _ := compose["services"].(M)
	if services == nil {
		return
	}
	connectBuiltinSQLApplications(compose, services)
	connectNextcloudRedis(compose, services)
	provisionGrafanaSources(compose, services)
	if prometheus, ok := services["prometheus"].(M); ok {
		dataPath := applicationCommandOption(prometheus, "--storage.tsdb.path")
		if dataPath == "" {
			dataPath = "/prometheus"
		}
		ensureApplicationDataVolume(compose, prometheus, "prometheus_data", dataPath)
	}
	if loki, ok := services["loki"].(M); ok {
		// The official Docker configuration uses /loki. A custom configuration
		// may use another store/path; do not pretend that this default covers it.
		config := applicationCommandOption(loki, "-config.file")
		if (config == "" || config == "/etc/loki/local-config.yaml") && !applicationMountCovers(loki, "/etc/loki/local-config.yaml") {
			ensureApplicationDataVolume(compose, loki, "loki_data", "/loki")
		}
	}
}

// Trusted application profiles that target an included database must use that
// database's final credentials, rather than independently invented model values.
// Explicit external database endpoints remain untouched. Shared MariaDB is one
// database/account; per-application SQL isolation remains an explicit limit.
func connectBuiltinSQLApplications(compose, services M) {
	for _, binding := range []struct {
		app, db, host, database, user, password string
	}{
		{"wordpress", "mariadb", "WORDPRESS_DB_HOST", "WORDPRESS_DB_NAME", "WORDPRESS_DB_USER", "WORDPRESS_DB_PASSWORD"},
		{"nextcloud", "mariadb", "MYSQL_HOST", "MYSQL_DATABASE", "MYSQL_USER", "MYSQL_PASSWORD"},
		{"gitea", "postgres", "GITEA__database__HOST", "GITEA__database__NAME", "GITEA__database__USER", "GITEA__database__PASSWD"},
	} {
		app, appOK := services[binding.app].(M)
		db, dbOK := services[binding.db].(M)
		if !appOK || !dbOK || applicationUsesNetworkMode(app) || applicationUsesNetworkMode(db) {
			continue
		}
		env, _ := app["environment"].(M)
		dbEnv, _ := db["environment"].(M)
		host, _ := env[binding.host].(string)
		if host != binding.db && !strings.HasPrefix(host, binding.db+":") {
			continue
		}
		read := func(keys ...string) interface{} {
			for _, key := range keys {
				if value, ok := dbEnv[key]; ok && value != "" && value != nil {
					return value
				}
			}
			return nil
		}
		var database, user, password interface{}
		if binding.db == "mariadb" {
			database, user, password = read("MARIADB_DATABASE", "MYSQL_DATABASE"), read("MARIADB_USER", "MYSQL_USER"), read("MARIADB_PASSWORD", "MYSQL_PASSWORD")
		} else {
			database, user, password = read("POSTGRES_DB"), read("POSTGRES_USER"), read("POSTGRES_PASSWORD")
		}
		if database == nil || user == nil || password == nil || !connectApplicationNetwork(compose, app, db) {
			continue
		}
		env[binding.database], env[binding.user], env[binding.password] = database, user, password
		if binding.app == "gitea" {
			env["GITEA__database__DB_TYPE"] = "postgres"
		}
		addApplicationDependency(app, binding.db)
	}
}

func connectNextcloudRedis(compose, services M) {
	// https://github.com/nextcloud/docker#redis-memory-caching
	nextcloud, hasNextcloud := services["nextcloud"].(M)
	redis, hasRedis := services["redis"].(M)
	if !hasNextcloud || !hasRedis || applicationUsesNetworkMode(nextcloud) || applicationUsesNetworkMode(redis) {
		return
	}
	env, _ := nextcloud["environment"].(M)
	if env == nil {
		env = M{}
	}
	if host, explicit := env["REDIS_HOST"]; explicit && host != "redis" {
		return
	}
	if !connectApplicationNetwork(compose, nextcloud, redis) {
		return
	}
	env["REDIS_HOST"] = "redis"
	if _, explicit := env["REDIS_HOST_PORT"]; !explicit {
		port := applicationCommandOption(redis, "--port")
		if port == "" {
			port = "6379"
		}
		env["REDIS_HOST_PORT"] = port
	}
	if _, explicit := env["REDIS_HOST_PASSWORD"]; !explicit {
		if _, fileExplicit := env["REDIS_HOST_PASSWORD_FILE"]; !fileExplicit {
			if password := applicationCommandOption(redis, "--requirepass"); password != "" {
				env["REDIS_HOST_PASSWORD"] = password
			}
		}
	}
	nextcloud["environment"] = env
	addApplicationDependency(nextcloud, "redis")
}

func provisionGrafanaSources(compose, services M) {
	// https://grafana.com/docs/grafana/latest/administration/provisioning/#data-sources
	grafana, ok := services["grafana"].(M)
	if !ok || applicationUsesNetworkMode(grafana) {
		return
	}
	env, _ := grafana["environment"].(M)
	if configured, exists := env["GF_PATHS_PROVISIONING"]; exists && configured != "/etc/grafana/provisioning" {
		return
	}
	const target = "/etc/grafana/provisioning/datasources/autodocker.yaml"
	if applicationMountTouches(grafana, "/etc/grafana/provisioning/datasources") {
		return
	}
	configs, _ := compose["configs"].(M)
	const configName = "autodocker_grafana_datasources"
	if _, exists := configs[configName]; exists {
		return
	}
	datasources := []M{}
	for _, backend := range []struct {
		key, label, flag string
		port             int
	}{
		{"prometheus", "Prometheus", "--web.listen-address", 9090},
		{"loki", "Loki", "-server.http-listen-port", 3100},
	} {
		svc, exists := services[backend.key].(M)
		if !exists || applicationUsesNetworkMode(svc) {
			continue
		}
		port := backend.port
		if value := applicationCommandOption(svc, backend.flag); value != "" {
			if backend.key == "prometheus" {
				if at := strings.LastIndex(value, ":"); at >= 0 {
					value = value[at+1:]
				}
			}
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > 65535 {
				continue
			}
			port = parsed
		}
		if !connectApplicationNetwork(compose, grafana, svc) {
			continue
		}
		datasources = append(datasources, M{
			"name": "AutoDocker " + backend.label, "uid": "autodocker-" + backend.key,
			"type": backend.key, "access": "proxy", "url": "http://" + backend.key + ":" + strconv.Itoa(port),
			"editable": true,
		})
		addApplicationDependency(grafana, backend.key)
	}
	if len(datasources) == 0 {
		return
	}
	content, err := yaml.Marshal(M{"apiVersion": 1, "datasources": datasources})
	if err != nil {
		return
	}
	if configs == nil {
		configs = M{}
		compose["configs"] = configs
	}
	configs[configName] = M{"content": string(content)}
	// Compose configs are read-only file mounts, with no init shell/container.
	mount := M{"source": configName, "target": target, "mode": 0444}
	switch existing := grafana["configs"].(type) {
	case []M:
		grafana["configs"] = append(existing, mount)
	case []interface{}:
		grafana["configs"] = append(existing, mount)
	case []string:
		merged := make([]interface{}, 0, len(existing)+1)
		for _, item := range existing {
			merged = append(merged, item)
		}
		grafana["configs"] = append(merged, mount)
	default:
		grafana["configs"] = []M{mount}
	}
}

func applicationCommandOption(service M, option string) string {
	args := toStringList(service["command"])
	for i, arg := range args {
		if strings.HasPrefix(arg, option+"=") {
			return strings.TrimPrefix(arg, option+"=")
		}
		if arg == option && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func applicationUsesNetworkMode(service M) bool {
	mode, _ := service["network_mode"].(string)
	return mode != ""
}

func applicationNetworkMap(service M) M {
	if networks, ok := service["networks"].(M); ok && len(networks) > 0 {
		return networks
	}
	networks := M{}
	for _, name := range toStringList(service["networks"]) {
		networks[name] = M{}
	}
	if len(networks) == 0 {
		networks["default"] = M{}
	}
	return networks
}

func connectApplicationNetwork(compose, application, backend M) bool {
	if applicationUsesNetworkMode(application) || applicationUsesNetworkMode(backend) {
		return false
	}
	appNetworks, backendNetworks := applicationNetworkMap(application), applicationNetworkMap(backend)
	for name := range appNetworks {
		if _, common := backendNetworks[name]; common {
			return true
		}
	}
	available := make([]string, 0, len(backendNetworks))
	for name := range backendNetworks {
		available = append(available, name)
	}
	sort.Strings(available)
	name := available[0]
	appNetworks[name] = M{}
	application["networks"] = appNetworks
	rootNetworks, _ := compose["networks"].(M)
	if rootNetworks == nil {
		rootNetworks = M{}
		compose["networks"] = rootNetworks
	}
	for attached := range appNetworks {
		if _, declared := rootNetworks[attached]; !declared && attached == "default" {
			rootNetworks[attached] = M{}
		}
	}
	return true
}

func addApplicationDependency(service M, dependency string) {
	if dependencies, ok := service["depends_on"].(M); ok {
		if _, exists := dependencies[dependency]; !exists {
			dependencies[dependency] = M{"condition": "service_started"}
		}
		return
	}
	dependencies := toStringList(service["depends_on"])
	if !containsInfrastructureString(dependencies, dependency) {
		service["depends_on"] = append(dependencies, dependency)
	}
}

func applicationMountTargets(service M) []string {
	targets := []string{}
	for _, kind := range []string{"volumes", "configs", "secrets"} {
		appendEntry := func(value interface{}) {
			switch mount := value.(type) {
			case M:
				if target, ok := mount["target"].(string); ok {
					targets = append(targets, path.Clean(target))
				}
			case string:
				if kind == "volumes" {
					parts := strings.Split(mount, ":")
					if len(parts) > 1 {
						targets = append(targets, path.Clean(parts[1]))
					} else if strings.HasPrefix(mount, "/") {
						targets = append(targets, path.Clean(mount))
					}
				}
			}
		}
		switch mounts := service[kind].(type) {
		case []M:
			for _, mount := range mounts {
				appendEntry(mount)
			}
		case []interface{}:
			for _, mount := range mounts {
				appendEntry(mount)
			}
		case []string:
			for _, mount := range mounts {
				appendEntry(mount)
			}
		}
	}
	return targets
}

func applicationMountCovers(service M, target string) bool {
	for _, mounted := range applicationMountTargets(service) {
		if mounted == target || mounted == "/" || strings.HasPrefix(target, mounted+"/") {
			return true
		}
	}
	return false
}

func applicationMountTouches(service M, target string) bool {
	if applicationMountCovers(service, target) {
		return true
	}
	for _, mounted := range applicationMountTargets(service) {
		if strings.HasPrefix(mounted, target+"/") {
			return true
		}
	}
	return false
}

func ensureApplicationDataVolume(compose, service M, name, target string) {
	if !strings.HasPrefix(target, "/") || applicationMountCovers(service, target) {
		return
	}
	volumes, _ := compose["volumes"].(M)
	if volumes == nil {
		volumes = M{}
		compose["volumes"] = volumes
	}
	if _, exists := volumes[name]; !exists {
		volumes[name] = M{}
	}
	mount := name + ":" + target
	switch existing := service["volumes"].(type) {
	case []M:
		service["volumes"] = append(existing, M{"type": "volume", "source": name, "target": target})
	case []interface{}:
		service["volumes"] = append(existing, mount)
	default:
		service["volumes"] = append(toStringList(service["volumes"]), mount)
	}
}
