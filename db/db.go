package db

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"autodocker-go/config"

	_ "modernc.org/sqlite"
)

var (
	DB   *sql.DB
	once sync.Once
)

func Init() *sql.DB {
	once.Do(func() {
		var err error
		DB, err = sql.Open("sqlite", config.DBPath+"?_busy_timeout=5000&_journal_mode=WAL")
		if err != nil {
			panic(fmt.Sprintf("[DB] Impossible d'ouvrir la base: %v", err))
		}
		DB.SetMaxOpenConns(5)
		DB.SetMaxIdleConns(2)
		if err = DB.Ping(); err != nil {
			panic(fmt.Sprintf("[DB] Ping echoue: %v", err))
		}
		createTables()
		ensureAdmin()
		fmt.Println("[DB] SQLite initialisee avec succes")
	})
	return DB
}

func execMulti(db *sql.DB, stmts string) error {
	// Split by semicolons and execute each statement individually
	for _, stmt := range splitStatements(stmts) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("statement failed: %s: %w", stmt[:min(len(stmt), 80)], err)
		}
	}
	return nil
}

func splitStatements(s string) []string {
	var stmts []string
	current := ""
	depth := 0
	for _, c := range s {
		if c == '(' {
			depth++
		} else if c == ')' {
			depth--
		}
		if c == ';' && depth == 0 {
			stmts = append(stmts, current)
			current = ""
		} else {
			current += string(c)
		}
	}
	if strings.TrimSpace(current) != "" {
		stmts = append(stmts, current)
	}
	return stmts
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func createTables() {
	tables := `
	CREATE TABLE IF NOT EXISTS users (
		username TEXT PRIMARY KEY,
		password_hash TEXT NOT NULL,
		role TEXT DEFAULT 'user',
		totp_secret TEXT DEFAULT '',
		totp_enabled INTEGER DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS labs (
		project TEXT PRIMARY KEY,
		owner TEXT DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS labs_acl (
		project TEXT NOT NULL,
		username TEXT NOT NULL,
		role TEXT DEFAULT 'read',
		PRIMARY KEY (project, username)
	);
	CREATE TABLE IF NOT EXISTS groups (
		group_name TEXT PRIMARY KEY
	);
	CREATE TABLE IF NOT EXISTS user_groups (
		username TEXT NOT NULL,
		group_name TEXT NOT NULL,
		PRIMARY KEY (username, group_name)
	);
	CREATE TABLE IF NOT EXISTS groups_acl (
		group_name TEXT NOT NULL,
		project TEXT NOT NULL,
		role TEXT DEFAULT 'read',
		PRIMARY KEY (group_name, project)
	);
	CREATE TABLE IF NOT EXISTS activity_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp TEXT NOT NULL,
		username TEXT NOT NULL,
		action TEXT NOT NULL,
		target TEXT DEFAULT '',
		details TEXT DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_activity_target_ts ON activity_log(target, timestamp);
	CREATE TABLE IF NOT EXISTS catalogue_custom (
		key TEXT PRIMARY KEY,
		image TEXT NOT NULL,
		category TEXT DEFAULT 'custom',
		description TEXT DEFAULT '',
		added_by TEXT DEFAULT '',
		added_at TEXT DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS agents (
		agent_id TEXT PRIMARY KEY,
		name TEXT DEFAULT '',
		token TEXT UNIQUE,
		created_by TEXT DEFAULT '',
		created_at TEXT DEFAULT '',
		last_seen TEXT DEFAULT '',
		status TEXT DEFAULT 'offline',
		hostname TEXT DEFAULT '',
		os_info TEXT DEFAULT '',
		ip_address TEXT DEFAULT '',
		last_data TEXT DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS backup_schedules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project TEXT NOT NULL,
		volume_name TEXT NOT NULL,
		cron_expr TEXT DEFAULT '0 2 * * *',
		retention_count INTEGER DEFAULT 5,
		enabled INTEGER DEFAULT 1,
		last_run TEXT DEFAULT '',
		created_by TEXT DEFAULT '',
		created_at TEXT DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS backup_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project TEXT NOT NULL,
		volume_name TEXT NOT NULL,
		filename TEXT NOT NULL,
		size_bytes INTEGER DEFAULT 0,
		status TEXT DEFAULT 'success',
		created_at TEXT DEFAULT '',
		created_by TEXT DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS container_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp TEXT NOT NULL,
		container_id TEXT NOT NULL,
		container_name TEXT DEFAULT '',
		project TEXT DEFAULT '',
		severity TEXT DEFAULT 'info',
		message TEXT DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_logs_ts ON container_logs(timestamp);
	CREATE INDEX IF NOT EXISTS idx_logs_severity ON container_logs(severity);
	CREATE INDEX IF NOT EXISTS idx_logs_cid ON container_logs(container_id);
	CREATE TABLE IF NOT EXISTS docker_hosts (
		host_id TEXT PRIMARY KEY,
		name TEXT DEFAULT '',
		url TEXT DEFAULT '',
		auth_type TEXT DEFAULT 'none',
		tls_ca TEXT DEFAULT '',
		tls_cert TEXT DEFAULT '',
		tls_key TEXT DEFAULT '',
		status TEXT DEFAULT 'unknown',
		last_check TEXT DEFAULT '',
		added_by TEXT DEFAULT '',
		added_at TEXT DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS uptime_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp TEXT NOT NULL,
		container_id TEXT NOT NULL,
		project TEXT DEFAULT '',
		status TEXT DEFAULT '',
		duration_sec REAL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS scaling_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp TEXT NOT NULL,
		project TEXT DEFAULT '',
		service TEXT DEFAULT '',
		rule_id INTEGER DEFAULT 0,
		direction TEXT DEFAULT '',
		old_replicas INTEGER DEFAULT 0,
		new_replicas INTEGER DEFAULT 0,
		metric_value REAL DEFAULT 0,
		threshold REAL DEFAULT 0,
		details TEXT DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS metrics_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp INTEGER NOT NULL,
		source_type TEXT NOT NULL DEFAULT 'container',
		source_id TEXT NOT NULL,
		source_name TEXT,
		project TEXT DEFAULT '',
		cpu REAL DEFAULT 0,
		mem REAL DEFAULT 0,
		mem_mb REAL DEFAULT 0,
		net_rx REAL DEFAULT 0,
		net_tx REAL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_metrics_ts ON metrics_history(timestamp);
	CREATE INDEX IF NOT EXISTS idx_metrics_src ON metrics_history(source_id);
	CREATE TABLE IF NOT EXISTS secrets (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project TEXT NOT NULL,
		key_name TEXT NOT NULL,
		encrypted_value TEXT NOT NULL,
		created_by TEXT DEFAULT '',
		created_at TEXT DEFAULT '',
		UNIQUE(project, key_name)
	);
	CREATE TABLE IF NOT EXISTS scheduled_tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project TEXT NOT NULL,
		container_name TEXT DEFAULT '',
		task_type TEXT NOT NULL,
		schedule_preset TEXT DEFAULT '',
		custom_cron TEXT DEFAULT '',
		command TEXT DEFAULT '',
		enabled INTEGER DEFAULT 1,
		last_run TEXT DEFAULT '',
		next_run TEXT DEFAULT '',
		created_by TEXT DEFAULT '',
		created_at TEXT DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS metric_baselines (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		container_id TEXT NOT NULL,
		metric TEXT NOT NULL,
		avg_value REAL DEFAULT 0,
		stddev REAL DEFAULT 0,
		sample_count INTEGER DEFAULT 0,
		last_updated TEXT DEFAULT '',
		UNIQUE(container_id, metric)
	);
	`
	// Migrate: drop incompatible metrics_history if schema differs (old had container_id, new has source_id)
	migrateMetricsHistory()

	if err := execMulti(DB, tables); err != nil {
		// Retry without indexes if existing DB has incompatible schema
		fmt.Printf("[DB] Warning: %v — tentative sans index\n", err)
		tablesOnly := ""
		for _, stmt := range splitStatements(tables) {
			s := strings.TrimSpace(stmt)
			if s != "" && !strings.HasPrefix(strings.ToUpper(s), "CREATE INDEX") {
				tablesOnly += s + ";\n"
			}
		}
		if err2 := execMulti(DB, tablesOnly); err2 != nil {
			panic(fmt.Sprintf("[DB] Erreur creation tables: %v", err2))
		}
		for _, stmt := range splitStatements(tables) {
			s := strings.TrimSpace(stmt)
			if strings.HasPrefix(strings.ToUpper(s), "CREATE INDEX") {
				DB.Exec(s)
			}
		}
	}
}

func migrateMetricsHistory() {
	// Check if old schema (container_id column) exists and drop it
	var colName string
	row := DB.QueryRow("SELECT name FROM pragma_table_info('metrics_history') WHERE name='container_id'")
	if row.Scan(&colName) == nil && colName == "container_id" {
		fmt.Println("[DB] Migration: recreation de metrics_history (ancien schema detecte)")
		DB.Exec("DROP TABLE IF EXISTS metrics_history")
		DB.Exec("DROP INDEX IF EXISTS idx_metrics_cid")
	}
}

func ensureAdmin() {
	// Check if admin user exists, create if not
	var count int
	DB.QueryRow("SELECT COUNT(*) FROM users WHERE role IN ('admin','super_admin')").Scan(&count)
	if count == 0 {
		// Will be created by the auth handler on first run
		fmt.Println("[DB] Aucun admin trouve — sera cree au demarrage")
	}
}

func LogActivity(username, action, target, details string) {
	_, _ = DB.Exec(
		"INSERT INTO activity_log (timestamp, username, action, target, details) VALUES (datetime('now'), ?, ?, ?, ?)",
		username, action, target, details,
	)
}
