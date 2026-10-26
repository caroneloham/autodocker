package services

import (
	"strings"
	"testing"
)

func TestBuildComposeFreeformStack(t *testing.T) {
	payload := BuildComposePayload{
		Addressing: AddressingConfig{Networks: []NetworkDef{
			{Name: "front", CIDR: "172.20.10.0/24", Gateway: "172.20.10.1"},
			{Name: "back", CIDR: "172.20.20.0/24", Gateway: "172.20.20.1"},
		}},
		Services: []ServiceEntry{
			{
				Key:     "zookeeper",
				Network: "back",
				Image:   "bitnami/zookeeper:3.9",
				Env:     map[string]string{"ALLOW_ANONYMOUS_LOGIN": "yes"},
				Volumes: []string{"zookeeper_data:/bitnami/zookeeper"},
			},
			{
				Key:     "kafka",
				Network: "back",
				Image:   "bitnami/kafka:3.7",
				Env: map[string]string{
					"KAFKA_CFG_ZOOKEEPER_CONNECT": "zookeeper:2181",
					"KAFKA_PASSWORD":              AutoPasswordPlaceholder,
				},
				Ports:     []string{"9092"},
				Volumes:   []string{"kafka_data:/bitnami/kafka", "/etc/passwd:/host:ro", "../bad:/x"},
				DependsOn: []string{"zookeeper", "missing-service", "kafka"},
			},
		},
	}

	compose := BuildCompose(payload, "test-freeform")
	services := compose["services"].(M)

	kafka, ok := services["kafka"].(M)
	if !ok {
		t.Fatalf("expected kafka service, got %#v", services)
	}
	if kafka["image"] != "bitnami/kafka:3.7" {
		t.Fatalf("expected kafka image, got %#v", kafka["image"])
	}

	env := kafka["environment"].(M)
	pw, _ := env["KAFKA_PASSWORD"].(string)
	if pw == "" || pw == AutoPasswordPlaceholder {
		t.Fatalf("expected generated password, got %#v", env["KAFKA_PASSWORD"])
	}

	if !hasPortMapping(kafka, ":9092") {
		t.Fatalf("expected kafka to expose container port 9092, got %#v", kafka["ports"])
	}

	volumes, _ := kafka["volumes"].([]string)
	if len(volumes) != 1 || volumes[0] != "kafka_data:/bitnami/kafka" {
		t.Fatalf("expected bind mounts to be rejected, got %#v", volumes)
	}
	if _, ok := compose["volumes"].(M)["kafka_data"]; !ok {
		t.Fatal("expected kafka_data volume to be registered")
	}

	deps, _ := kafka["depends_on"].([]string)
	if len(deps) != 1 || deps[0] != "zookeeper" {
		t.Fatalf("expected only zookeeper dependency, got %#v", deps)
	}

	if _, ok := services["zookeeper"]; !ok {
		t.Fatal("expected zookeeper service")
	}
}

func TestBuildComposeFreeformRejectsInvalidInput(t *testing.T) {
	payload := BuildComposePayload{
		Addressing: AddressingConfig{Networks: []NetworkDef{
			{Name: "back", CIDR: "172.20.20.0/24", Gateway: "172.20.20.1"},
		}},
		Services: []ServiceEntry{
			{Key: "evil", Network: "back", Image: "evil image; rm -rf /"},
			{Key: "bad key!", Network: "back", Image: "nginx:alpine"},
			{Key: "noimage", Network: "back"},
		},
	}

	compose := BuildCompose(payload, "test-reject")
	services := compose["services"].(M)
	if len(services) != 0 {
		t.Fatalf("expected all invalid freeform services to be rejected, got %#v", services)
	}
}

func TestValidFreeformImage(t *testing.T) {
	valid := []string{
		"nginx:alpine",
		"bitnami/kafka:3.7",
		"ghcr.io/open-webui/open-webui:main",
		"itzg/minecraft-server:latest",
		"redis@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	for _, img := range valid {
		if !ValidFreeformImage(img) {
			t.Fatalf("expected image %q to be valid", img)
		}
	}
	invalid := []string{
		"",
		"Evil Image",
		"nginx:alpine; rm -rf /",
		"nginx:$(whoami)",
		"-leading/dash",
		strings.Repeat("a", 300),
	}
	for _, img := range invalid {
		if ValidFreeformImage(img) {
			t.Fatalf("expected image %q to be rejected", img)
		}
	}
}

func TestNormalizeMagicPayloadKeepsFreeformServices(t *testing.T) {
	payload := map[string]interface{}{
		"services": []interface{}{
			map[string]interface{}{
				"key":     "keycloak",
				"image":   "quay.io/keycloak/keycloak:24.0",
				"network": "back",
				"env":     map[string]interface{}{"KEYCLOAK_ADMIN": "admin", "KEYCLOAK_ADMIN_PASSWORD": AutoPasswordPlaceholder},
				"ports":   []interface{}{8080, "8443:8443"},
				"volumes": []interface{}{"keycloak_data:/opt/keycloak/data", "/var/run/docker.sock:/sock"},
				"command": []interface{}{"start-dev"},
			},
		},
	}

	repaired, warnings, valid := NormalizeMagicPayload("deploie keycloak", payload)
	if !valid {
		t.Fatal("expected freeform payload to be valid")
	}
	keys := servicesInPayload(t, repaired)
	if !keys["keycloak"] {
		t.Fatalf("expected keycloak to be kept, got %#v", keys)
	}
	if keys["nginx"] {
		t.Fatalf("expected no forced nginx for freeform stack, got %#v", keys)
	}
	if !keys["portainer"] || !keys["traefik"] {
		t.Fatalf("expected base services, got %#v", keys)
	}

	var keycloak map[string]interface{}
	for _, raw := range repaired["services"].([]map[string]interface{}) {
		if raw["key"] == "keycloak" {
			keycloak = raw
		}
	}
	ports, _ := keycloak["ports"].([]string)
	if len(ports) != 2 {
		t.Fatalf("expected normalized ports, got %#v", keycloak["ports"])
	}
	vols, _ := keycloak["volumes"].([]string)
	if len(vols) != 1 || vols[0] != "keycloak_data:/opt/keycloak/data" {
		t.Fatalf("expected docker.sock mount to be dropped, got %#v", vols)
	}

	foundWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "mode libre") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatalf("expected freeform warning, got %#v", warnings)
	}
}

func TestNormalizeMagicPayloadKeepsDebianRouter(t *testing.T) {
	payload := map[string]interface{}{
		"services": []interface{}{
			map[string]interface{}{"key": "debian-router", "network": "back"},
			map[string]interface{}{"key": "nginx", "network": "front"},
		},
	}
	repaired, _, valid := NormalizeMagicPayload("lab reseau avec routeur", payload)
	if !valid {
		t.Fatal("expected payload to be valid")
	}
	keys := servicesInPayload(t, repaired)
	if !keys["debian-router"] {
		t.Fatalf("expected debian-router to be kept, got %#v", keys)
	}
}

func TestSanitizeNamedVolume(t *testing.T) {
	cases := []struct {
		spec string
		ok   bool
	}{
		{"data:/var/lib/data", true},
		{"my-vol_1:/srv", true},
		{"/host/path:/container", false},
		{"./relative:/container", false},
		{"data:relative/path", false},
		{"data:/etc/../root", false},
		{"data", false},
		{"C:\\windows:/data", false},
	}
	for _, tc := range cases {
		_, _, ok := sanitizeNamedVolume(tc.spec)
		if ok != tc.ok {
			t.Fatalf("sanitizeNamedVolume(%q) = %v, want %v", tc.spec, ok, tc.ok)
		}
	}
}
