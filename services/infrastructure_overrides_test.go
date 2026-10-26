package services

import (
	"strings"
	"testing"
)

func TestInfrastructureBuiltinSettingsReachCompose(t *testing.T) {
	payload := BuildComposePayload{Services: []ServiceEntry{
		{Key: "wordpress", Network: "app", Image: "wordpress:6.8", Env: map[string]string{"WORDPRESS_DB_PASSWORD": AutoPasswordPlaceholder, "WORDPRESS_CONFIG_EXTRA": "define('WP_DEBUG', false);"}, Ports: []string{"43879:80"}, Volumes: []string{"wp_extra:/opt/extra"}},
		{Key: "mariadb", Network: "app", Env: map[string]string{"MYSQL_PASSWORD": AutoPasswordPlaceholder}},
	}, Addressing: AddressingConfig{Networks: []NetworkDef{{Name: "app", CIDR: "172.28.0.0/24", Gateway: "172.28.0.1"}}}}
	compose := BuildCompose(payload, "override-test")
	services := compose["services"].(M)
	wp := services["wordpress"].(M)
	db := services["mariadb"].(M)
	if wp["image"] != "wordpress:6.8" {
		t.Fatal("explicit image version was ignored")
	}
	if len(toStringList(wp["ports"])) != 1 || !strings.HasSuffix(toStringList(wp["ports"])[0], ":80") {
		t.Fatal("explicit port was ignored")
	}
	if wp["environment"].(M)["WORDPRESS_CONFIG_EXTRA"] != "define('WP_DEBUG', false);" {
		t.Fatal("configuration was ignored")
	}
	if wp["environment"].(M)["WORDPRESS_DB_PASSWORD"] != db["environment"].(M)["MYSQL_PASSWORD"] {
		t.Fatal("automatic credentials broke app/database connection")
	}
	if !containsInfrastructureString(toStringList(wp["volumes"]), "wordpress_data:/var/www/html") || !containsInfrastructureString(toStringList(wp["volumes"]), "wp_extra:/opt/extra") {
		t.Fatal("extra volume removed the application's persistent data")
	}
	if compose["volumes"].(M)["wp_extra"] == nil {
		t.Fatal("extra named volume was not declared")
	}
}

func TestInfrastructureFreeformAppSharesBuiltinDatabaseCredential(t *testing.T) {
	payload := BuildComposePayload{Services: []ServiceEntry{{Key: "postgres", Network: "app"}, {Key: "worker", Image: "alpine:3.21", Network: "app", Env: map[string]string{"DATABASE_PASSWORD": AutoPasswordPlaceholder}}}}
	services := BuildCompose(payload, "credential-test")["services"].(M)
	if services["worker"].(M)["environment"].(M)["DATABASE_PASSWORD"] != services["postgres"].(M)["environment"].(M)["POSTGRES_PASSWORD"] {
		t.Fatal("freeform app received the wrong database password")
	}
}
