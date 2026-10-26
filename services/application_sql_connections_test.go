package services

import "testing"

func TestManagedSQLConnectionsUseFinalDatabaseCredentials(t *testing.T) {
	p := BuildComposePayload{Services: []ServiceEntry{
		{Key: "wordpress", Env: map[string]string{"WORDPRESS_DB_PASSWORD": "different-model-value"}},
		{Key: "nextcloud", Env: map[string]string{"MYSQL_USER": "different-model-user"}},
		{Key: "mariadb", Env: map[string]string{"MARIADB_DATABASE": "team", "MARIADB_USER": "team_user", "MARIADB_PASSWORD": "synthetic-test-secret"}},
		{Key: "gitea", Env: map[string]string{"GITEA__database__PASSWD": "another-model-value"}},
		{Key: "postgres", Env: map[string]string{"POSTGRES_PASSWORD": "synthetic-pg-secret"}},
	}}
	sv := BuildCompose(p, "sql-wiring-test")["services"].(M)
	wp := sv["wordpress"].(M)["environment"].(M)
	nc := sv["nextcloud"].(M)["environment"].(M)
	pg := sv["postgres"].(M)["environment"].(M)
	gt := sv["gitea"].(M)["environment"].(M)
	if wp["WORDPRESS_DB_PASSWORD"] != "synthetic-test-secret" || nc["MYSQL_USER"] != "team_user" || wp["WORDPRESS_DB_NAME"] != "team" || nc["MYSQL_DATABASE"] != "team" || gt["GITEA__database__PASSWD"] != pg["POSTGRES_PASSWORD"] {
		t.Fatal("application and actual database credentials diverged")
	}
	p.Services[0].Env["WORDPRESS_DB_HOST"] = "external.example:3306"
	sv = BuildCompose(p, "external-db-test")["services"].(M)
	if sv["wordpress"].(M)["environment"].(M)["WORDPRESS_DB_PASSWORD"] != "different-model-value" {
		t.Fatal("an explicit external database was overwritten")
	}
}
