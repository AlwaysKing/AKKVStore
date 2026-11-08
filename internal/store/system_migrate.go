package store

import (
	"database/sql"
	"fmt"
)

func createSystemSchemaV6(execer sqlExecer) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS projects (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS project_tokens (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			token TEXT NOT NULL DEFAULT '',
			token_hash TEXT NOT NULL UNIQUE,
			request_count INTEGER NOT NULL DEFAULT 0,
			read_count INTEGER NOT NULL DEFAULT 0,
			write_count INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			last_used_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_project_tokens_project_id ON project_tokens(project_id)`,
		`CREATE TABLE IF NOT EXISTS project_token_access (
			project_id INTEGER NOT NULL,
			token_id INTEGER NOT NULL,
			second_start INTEGER NOT NULL,
			request_count INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(project_id, token_id, second_start)
		) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS idx_project_token_access_second ON project_token_access(second_start)`,
		`CREATE TABLE IF NOT EXISTS project_usage (
			project_id INTEGER PRIMARY KEY,
			request_count INTEGER NOT NULL DEFAULT 0
		) WITHOUT ROWID`,
	} {
		if _, err := execer.Exec(q); err != nil {
			return fmt.Errorf("init system schema: %w", err)
		}
	}
	return nil
}

func ensureSystemSchemaV6(db *sql.DB) error {
	if err := createSystemSchemaV6(db); err != nil {
		return err
	}
	rows, err := db.Query(`PRAGMA table_info("project_tokens")`)
	if err != nil {
		return err
	}
	columns := make(map[string]struct{})
	for rows.Next() {
		var (
			cid          int
			name         string
			columnType   string
			notNull      int
			defaultValue sql.NullString
			primaryKey   int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		columns[name] = struct{}{}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if _, ok := columns["token"]; !ok {
		if _, err := db.Exec(`ALTER TABLE project_tokens
			ADD COLUMN token TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("repair project_tokens token column: %w", err)
		}
		columns["token"] = struct{}{}
	}
	for _, required := range []string{
		"project_id", "name", "token", "token_hash", "request_count",
		"read_count", "write_count", "created_at", "last_used_at",
	} {
		if _, ok := columns[required]; !ok {
			return fmt.Errorf("project_tokens is missing required column %s", required)
		}
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO project_usage(project_id, request_count)
		SELECT project_id, COALESCE(SUM(request_count), 0)
		FROM project_tokens
		GROUP BY project_id`); err != nil {
		return fmt.Errorf("initialize project usage: %w", err)
	}
	rows, err = db.Query(`PRAGMA table_info("project_token_access")`)
	if err != nil {
		return err
	}
	accessColumns := make(map[string]struct{})
	for rows.Next() {
		var (
			cid          int
			name         string
			columnType   string
			notNull      int
			defaultValue sql.NullString
			primaryKey   int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		accessColumns[name] = struct{}{}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if _, ok := accessColumns["second_start"]; !ok {
		if _, err := db.Exec(`DROP TABLE project_token_access`); err != nil {
			return fmt.Errorf("repair project_token_access table: %w", err)
		}
		for _, q := range []string{
			`CREATE TABLE project_token_access (
				project_id INTEGER NOT NULL,
				token_id INTEGER NOT NULL,
				second_start INTEGER NOT NULL,
				request_count INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY(project_id, token_id, second_start)
			) WITHOUT ROWID`,
			`CREATE INDEX idx_project_token_access_second ON project_token_access(second_start)`,
		} {
			if _, err := db.Exec(q); err != nil {
				return fmt.Errorf("repair project_token_access table: %w", err)
			}
		}
		accessColumns = map[string]struct{}{
			"project_id": {}, "token_id": {}, "second_start": {}, "request_count": {},
		}
	}
	for _, required := range []string{"project_id", "token_id", "second_start", "request_count"} {
		if _, ok := accessColumns[required]; !ok {
			return fmt.Errorf("project_token_access is missing required column %s", required)
		}
	}
	return nil
}

func migrateSystemSchemaV1ToV2(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`CREATE TABLE projects__migrate_v2 (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO projects__migrate_v2(id, name, created_at, updated_at)
		SELECT id, name, created_at, updated_at FROM projects`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE project_tokens (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id INTEGER NOT NULL,
		name TEXT NOT NULL,
		token_hash TEXT NOT NULL UNIQUE,
		request_count INTEGER NOT NULL DEFAULT 0,
		read_count INTEGER NOT NULL DEFAULT 0,
		write_count INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO project_tokens(project_id, name, token_hash, created_at)
		SELECT id, '默认 Token', token_hash, created_at
		FROM projects
		WHERE token_hash <> ''`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE projects`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE projects__migrate_v2 RENAME TO projects`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX idx_project_tokens_project_id ON project_tokens(project_id)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE meta SET value = 2 WHERE key = 'schema_version'`); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateSystemSchemaV2ToV3(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`ALTER TABLE project_tokens
		ADD COLUMN token TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`,
		systemSchemaV3); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateSystemSchemaV3ToV4(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS project_token_access (
			project_id INTEGER NOT NULL,
			token_id INTEGER NOT NULL,
			minute_start INTEGER NOT NULL,
			request_count INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(project_id, token_id, minute_start)
		) WITHOUT ROWID`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`,
		systemSchemaV4); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateSystemSchemaV4ToV5(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS project_usage (
		project_id INTEGER PRIMARY KEY,
		request_count INTEGER NOT NULL DEFAULT 0
	) WITHOUT ROWID`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO project_usage(project_id, request_count)
		SELECT project_id, COALESCE(SUM(request_count), 0)
		FROM project_tokens
		WHERE true
		GROUP BY project_id
		ON CONFLICT(project_id) DO UPDATE SET
			request_count = MAX(project_usage.request_count, excluded.request_count)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`,
		systemSchemaV5); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateSystemSchemaV5ToV6(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DROP TABLE IF EXISTS project_token_access`); err != nil {
		return err
	}
	for _, q := range []string{
		`CREATE TABLE project_token_access (
			project_id INTEGER NOT NULL,
			token_id INTEGER NOT NULL,
			second_start INTEGER NOT NULL,
			request_count INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(project_id, token_id, second_start)
		) WITHOUT ROWID`,
		`CREATE INDEX idx_project_token_access_second ON project_token_access(second_start)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`,
		currentSystemSchemaVersion); err != nil {
		return err
	}
	return tx.Commit()
}
