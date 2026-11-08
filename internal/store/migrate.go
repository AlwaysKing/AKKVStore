package store

import (
	"database/sql"
	"fmt"
)

func migrateProjectSchemaV1ToV2(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT table_name FROM kv_tables ORDER BY id`)
	if err != nil {
		return err
	}
	var dataTables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		dataTables = append(dataTables, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}

	for _, table := range dataTables {
		if err := migrateDataTableV1ToV2(tx, table); err != nil {
			return err
		}
	}
	if err := migrateTableMetadataV1ToV2(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`,
		currentProjectSchemaVersion); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateDataTableV1ToV2(tx *sql.Tx, table string) error {
	if !dataTableRe.MatchString(table) {
		return fmt.Errorf("invalid data table %q", table)
	}
	migratedTable := table + "__migrate_v2"
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		migratedTable).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("temporary migration table %s already exists", migratedTable)
	}
	if _, err := tx.Exec(dataTableSQL(migratedTable)); err != nil {
		return err
	}
	insertSQL := fmt.Sprintf(`INSERT INTO "%s"
		(key, value, blob_hash, content_hash, content_type, size, created_at, updated_at, deleted)
		SELECT key, value, blob_hash, content_hash, content_type, size, created_at, updated_at, deleted
		FROM "%s"`, migratedTable, table)
	if _, err := tx.Exec(insertSQL); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`DROP TABLE "%s"`, table)); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE "%s" RENAME TO "%s"`, migratedTable, table)); err != nil {
		return err
	}
	return createDataTableSchema(tx, table)
}

func migrateTableMetadataV1ToV2(tx *sql.Tx) error {
	const migratedTable = "kv_tables__migrate_v2"
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		migratedTable).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("temporary migration table %s already exists", migratedTable)
	}
	if _, err := tx.Exec(`CREATE TABLE kv_tables__migrate_v2 (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		table_name TEXT NOT NULL UNIQUE,
		key_count INTEGER NOT NULL DEFAULT 0 CHECK(key_count >= 0),
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO kv_tables__migrate_v2
		(id, name, table_name, key_count, created_at, updated_at)
		SELECT id, name, table_name, key_count, created_at, updated_at
		FROM kv_tables`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE kv_tables`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE kv_tables__migrate_v2 RENAME TO kv_tables`); err != nil {
		return err
	}
	return nil
}
