package services

import (
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestNextcloudRedisUsesFinalAuthenticationAndNetwork(t *testing.T) {
	payload := BuildComposePayload{Services: []ServiceEntry{
		{Key: "nextcloud", Network: "front"},
		{Key: "redis", Network: "back", Command: []string{"redis-server", "--port", "6380", "--requirepass", "test-redis-password"}},
	}, Addressing: AddressingConfig{Networks: []NetworkDef{{Name: "front", CIDR: "172.29.10.0/24", Gateway: "172.29.10.1"}, {Name: "back", CIDR: "172.29.11.0/24", Gateway: "172.29.11.1"}}}}
	compose := BuildCompose(payload, "nextcloud-redis-connections")
	svcs := compose["services"].(M)
	nextcloud := svcs["nextcloud"].(M)
	env := nextcloud["environment"].(M)
	if env["REDIS_HOST"] != "redis" || env["REDIS_HOST_PORT"] != "6380" || env["REDIS_HOST_PASSWORD"] != "test-redis-password" {
		t.Fatal("Nextcloud Redis endpoint/authentication does not match the final Redis command")
	}
	if nextcloud["networks"].(M)["back"] == nil || !containsInfrastructureString(toStringList(nextcloud["depends_on"]), "redis") {
		t.Fatal("Nextcloud cannot reach/wait for Redis")
	}
	if nextcloud["networks"].(M)["front"] == nil || env["MYSQL_HOST"] == nil {
		t.Fatal("Existing application network/database settings lost")
	}
}

func TestNextcloudRedisPreservesExplicitEndpointAndSecrets(t *testing.T) {
	for _, endpoint := range []string{"redis.example.test", ""} {
		env := M{"REDIS_HOST": endpoint, "REDIS_HOST_PASSWORD": "explicit"}
		nextcloud := M{"environment": env}
		compose := M{"services": M{"nextcloud": nextcloud, "redis": M{"command": []string{"redis-server", "--requirepass=other"}}}}
		applyApplicationConnections(compose)
		if env["REDIS_HOST"] != endpoint || env["REDIS_HOST_PASSWORD"] != "explicit" || nextcloud["depends_on"] != nil {
			t.Fatal("Explicit external/disabled Redis configuration was replaced")
		}
	}
	for _, protected := range []string{"REDIS_HOST_PASSWORD", "REDIS_HOST_PASSWORD_FILE"} {
		env := M{"REDIS_HOST": "redis", protected: "user-value"}
		compose := M{"services": M{"nextcloud": M{"environment": env}, "redis": M{"command": []string{"redis-server", "--requirepass=server-password"}}}}
		applyApplicationConnections(compose)
		if env[protected] != "user-value" || protected == "REDIS_HOST_PASSWORD_FILE" && env["REDIS_HOST_PASSWORD"] != nil {
			t.Fatal("Explicit Redis password/password-file was overwritten")
		}
	}
	unauthenticated := M{"environment": M{"REDIS_PASSWORD": "unused-by-official-image"}}
	app := M{}
	compose := M{"services": M{"nextcloud": app, "redis": unauthenticated}}
	applyApplicationConnections(compose)
	if app["environment"].(M)["REDIS_HOST_PASSWORD"] != nil {
		t.Fatal("An unused Redis environment variable was mistaken for configured authentication")
	}
}

func TestGrafanaSourcesAreProvisionedAsReadOnlyConfigs(t *testing.T) {
	compose := M{"services": M{
		"grafana":    M{"networks": M{"front": M{"ipv4_address": "172.29.12.10"}}, "depends_on": M{"existing": M{"condition": "service_healthy"}}},
		"prometheus": M{"networks": M{"metrics": M{}}},
		"loki":       M{"networks": M{"logs": M{}}},
	}, "networks": M{"front": M{}, "metrics": M{}, "logs": M{}}}
	applyApplicationConnections(compose)
	grafana := compose["services"].(M)["grafana"].(M)
	sources := compose["configs"].(M)["autodocker_grafana_datasources"].(M)["content"].(string)
	var provision struct {
		APIVersion  int `yaml:"apiVersion"`
		Datasources []struct {
			Type string `yaml:"type"`
			URL  string `yaml:"url"`
			UID  string `yaml:"uid"`
		} `yaml:"datasources"`
	}
	if err := yaml.Unmarshal([]byte(sources), &provision); err != nil {
		t.Fatal(err)
	}
	if provision.APIVersion != 1 || len(provision.Datasources) != 2 {
		t.Fatal("Invalid Grafana provisioning document")
	}
	for _, source := range provision.Datasources {
		if source.Type == "prometheus" && source.URL != "http://prometheus:9090" || source.Type == "loki" && source.URL != "http://loki:3100" || !strings.HasPrefix(source.UID, "autodocker-") {
			t.Fatal("Datasource does not use an internal endpoint/owned UID")
		}
	}
	mount := grafana["configs"].([]M)[0]
	if mount["target"] != "/etc/grafana/provisioning/datasources/autodocker.yaml" || mount["mode"] != 0444 || grafana["command"] != nil {
		t.Fatal("Provisioning must be a read-only config with no shell command")
	}
	for _, network := range []string{"front", "metrics", "logs"} {
		if grafana["networks"].(M)[network] == nil {
			t.Fatal("Grafana network lost or datasource unreachable")
		}
	}
	deps := grafana["depends_on"].(M)
	if deps["prometheus"] == nil || deps["loki"] == nil || deps["existing"].(M)["condition"] != "service_healthy" {
		t.Fatal("Dependencies lost or datasource startup not declared")
	}
	if !applicationMountCovers(compose["services"].(M)["prometheus"].(M), "/prometheus") || !applicationMountCovers(compose["services"].(M)["loki"].(M), "/loki") {
		t.Fatal("Metric/log storage is not persisted")
	}
	before := sources
	applyApplicationConnections(compose)
	if len(grafana["configs"].([]M)) != 1 || compose["configs"].(M)["autodocker_grafana_datasources"].(M)["content"] != before {
		t.Fatal("Repeated application overwrote/duplicated provisioning")
	}
}

func TestApplicationConnectionsPreserveCustomProvisioningAndStorage(t *testing.T) {
	for _, grafana := range []M{
		{"volumes": []string{"custom_sources:/etc/grafana/provisioning/datasources"}},
		{"configs": []M{{"source": "custom", "target": "/etc/grafana/provisioning/datasources/custom.yaml"}}},
		{"environment": M{"GF_PATHS_PROVISIONING": "/custom/provisioning"}},
	} {
		compose := M{"services": M{"grafana": grafana, "prometheus": M{}}}
		applyApplicationConnections(compose)
		if compose["configs"] != nil {
			t.Fatal("Explicit Grafana provisioning was overwritten or supplemented")
		}
	}
	prometheus := M{"volumes": []M{{"type": "volume", "source": "client_metrics", "target": "/metrics"}}, "command": []string{"--storage.tsdb.path=/metrics"}}
	loki := M{"volumes": []string{"client_logs:/loki"}}
	compose := M{"services": M{"prometheus": prometheus, "loki": loki}}
	applyApplicationConnections(compose)
	if len(prometheus["volumes"].([]M)) != 1 || len(loki["volumes"].([]string)) != 1 || compose["volumes"] != nil {
		t.Fatal("User data volumes were replaced or redundant volumes added")
	}
}

func TestApplicationConnectionsSkipNetworkModesAndUnselectedServices(t *testing.T) {
	nextcloud := M{"network_mode": "host"}
	grafana := M{"network_mode": "none"}
	compose := M{"services": M{"nextcloud": nextcloud, "redis": M{}, "grafana": grafana, "prometheus": M{}}}
	applyApplicationConnections(compose)
	if nextcloud["networks"] != nil || nextcloud["environment"] != nil || grafana["networks"] != nil || grafana["configs"] != nil {
		t.Fatal("Automatic wiring conflicted with network_mode")
	}
	only := M{"services": M{"nextcloud": M{}, "grafana": M{}}}
	applyApplicationConnections(only)
	if len(only["services"].(M)) != 2 || only["configs"] != nil {
		t.Fatal("Unselected dependencies were inserted")
	}
}

func TestGrafanaUsesConfiguredBackendListenPorts(t *testing.T) {
	compose := M{"services": M{
		"grafana":    M{},
		"prometheus": M{"command": []string{"--web.listen-address=:9095"}},
		"loki":       M{"command": []string{"-server.http-listen-port", "3105"}},
	}}
	applyApplicationConnections(compose)
	sources := compose["configs"].(M)["autodocker_grafana_datasources"].(M)["content"].(string)
	if !strings.Contains(sources, "http://prometheus:9095") || !strings.Contains(sources, "http://loki:3105") {
		t.Fatal("Datasource URLs ignore explicit backend listen ports")
	}
}
