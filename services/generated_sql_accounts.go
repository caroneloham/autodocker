package services

import "fmt"

// Provision separate databases and least-privilege accounts on fresh volumes.
// External endpoints and user-managed initialization remain authoritative.
func provisionGeneratedSQLAccounts(compose M) {
	svcs := compose["services"].(M)
	for _, backend := range []struct{ key, config string }{
		{"mariadb", "autodocker_mariadb_apps"}, {"nextcloud-db", "autodocker_nextcloud_apps"}, {"postgres", "autodocker_postgres_apps"},
	} {
		db, ok := svcs[backend.key].(M)
		if !ok || applicationUsesNetworkMode(db) || applicationMountTouches(db, "/docker-entrypoint-initdb.d") {
			continue
		}
		sql := ""
		for _, binding := range []struct{ app, host, database, user, password string }{
			{"wordpress", "WORDPRESS_DB_HOST", "WORDPRESS_DB_NAME", "WORDPRESS_DB_USER", "WORDPRESS_DB_PASSWORD"},
			{"nextcloud", "MYSQL_HOST", "MYSQL_DATABASE", "MYSQL_USER", "MYSQL_PASSWORD"},
			{"gitea", "GITEA__database__HOST", "GITEA__database__NAME", "GITEA__database__USER", "GITEA__database__PASSWD"},
		} {
			app, ok := svcs[binding.app].(M)
			if !ok || applicationUsesNetworkMode(app) {
				continue
			}
			env, _ := app["environment"].(M)
			host, _ := env[binding.host].(string)
			if host != backend.key && host != backend.key+":3306" && host != backend.key+":5432" {
				continue
			}
			if backend.key == "postgres" && binding.app != "gitea" || backend.key != "postgres" && binding.app == "gitea" {
				continue
			}
			if !connectApplicationNetwork(compose, app, db) {
				continue
			}
			name, user, secret := binding.app, binding.app+"_app", generatePassword(32)
			env[binding.database], env[binding.user], env[binding.password] = name, user, secret
			if backend.key == "postgres" {
				sql += fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE;\nCREATE DATABASE %s OWNER %s;\nREVOKE ALL ON DATABASE %s FROM PUBLIC;\n", user, secret, name, user, name)
				if db["healthcheck"] == nil {
					db["healthcheck"] = M{"test": []string{"CMD-SHELL", "pg_isready -h 127.0.0.1 -U \"$$POSTGRES_USER\" -d \"$$POSTGRES_DB\""}, "interval": "5s", "timeout": "5s", "retries": 30, "start_period": "20s"}
				}
			} else {
				sql += fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;\nCREATE USER '%s'@'%%' IDENTIFIED BY '%s';\nGRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%%';\n", name, user, secret, name, user)
				if db["healthcheck"] == nil {
					db["healthcheck"] = M{"test": []string{"CMD", "healthcheck.sh", "--connect", "--innodb_initialized"}, "interval": "5s", "timeout": "5s", "retries": 30, "start_period": "20s"}
				}
			}
			// All templates in this closed set expose official health commands.
			dependencies := M{}
			if old, ok := app["depends_on"].(M); ok {
				dependencies = old
			} else {
				for _, key := range toStringList(app["depends_on"]) {
					dependencies[key] = M{"condition": "service_started"}
				}
			}
			dependencies[backend.key] = M{"condition": "service_healthy"}
			app["depends_on"] = dependencies
		}
		if sql == "" {
			continue
		}
		configs, _ := compose["configs"].(M)
		if configs == nil {
			configs = M{}
			compose["configs"] = configs
		}
		configs[backend.config] = M{"content": sql}
		db["configs"] = appendGeneratedConfig(db["configs"], M{"source": backend.config, "target": "/docker-entrypoint-initdb.d/20-autodocker-apps.sql", "mode": 0444})
		// MariaDB root initializes the per-app accounts above. Do not also create
		// a shared application account through entrypoint environment variables.
		if backend.key != "postgres" {
			if env, ok := db["environment"].(M); ok {
				for _, key := range []string{"MARIADB_USER", "MARIADB_PASSWORD", "MARIADB_DATABASE", "MYSQL_USER", "MYSQL_PASSWORD", "MYSQL_DATABASE"} {
					delete(env, key)
				}
			}
		}
	}
}

func appendGeneratedConfig(existing interface{}, mount M) []interface{} {
	result := []interface{}{}
	switch entries := existing.(type) {
	case []interface{}:
		result = append(result, entries...)
	case []M:
		for _, entry := range entries {
			result = append(result, entry)
		}
	case []string:
		for _, entry := range entries {
			result = append(result, entry)
		}
	}
	return append(result, mount)
}
