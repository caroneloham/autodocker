package db

import "database/sql"

// Additive migration: preserve existing usernames, hashes, roles and MFA.
func EnsureAccountColumns(conn *sql.DB) error {
	rows, err := conn.Query("PRAGMA table_info(users)")
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var id, required, primary int
		var name, kind string
		var initial interface{}
		if err := rows.Scan(&id, &name, &kind, &required, &initial, &primary); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, field := range []struct{ name, statement string }{
		{"email", "ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT ''"},
		{"disabled", "ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0"},
		{"auth_version", "ALTER TABLE users ADD COLUMN auth_version INTEGER NOT NULL DEFAULT 0"},
	} {
		if !columns[field.name] {
			if _, err := conn.Exec(field.statement); err != nil {
				return err
			}
		}
	}
	return nil
}
