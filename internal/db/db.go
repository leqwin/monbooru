package db

import (
	"database/sql"
	_ "embed"
	"fmt"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// DB holds read and write connection pools for the SQLite database.
// SQLite in WAL mode supports concurrent readers but serializes writers.
// Read pool: multiple connections for concurrent reads.
// Write pool: single connection for all writes and migrations.
type DB struct {
	Read  *sql.DB
	Write *sql.DB
}

// Open opens both connection pools pointing at the same SQLite file.
func Open(path string) (*DB, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=journal_mode(wal)" +
		"&_pragma=synchronous(normal)" +
		"&_pragma=cache_size(-65536)" +
		"&_pragma=temp_store(memory)" +
		"&_pragma=mmap_size(268435456)"

	rd, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening read pool: %w", err)
	}
	rd.SetMaxOpenConns(8)
	rd.SetMaxIdleConns(8)

	wr, err := sql.Open("sqlite", dsn)
	if err != nil {
		rd.Close()
		return nil, fmt.Errorf("opening write pool: %w", err)
	}
	wr.SetMaxOpenConns(1)
	wr.SetMaxIdleConns(1)

	db := &DB{Read: rd, Write: wr}

	// Verify connectivity
	if err := rd.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging read pool: %w", err)
	}
	if err := wr.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging write pool: %w", err)
	}

	return db, nil
}

// Bootstrap runs the embedded schema.sql on the write pool, then applies
// the small set of idempotent column-add migrations needed for DBs that
// predate a new column. SQLite does not support ADD COLUMN IF NOT EXISTS,
// so each migration gates itself on pragma_table_info.
func Bootstrap(db *DB) error {
	if _, err := db.Write.Exec(schemaSQL); err != nil {
		return fmt.Errorf("bootstrapping schema: %w", err)
	}
	if err := ensureColumn(db, "images", "origin", `ALTER TABLE images ADD COLUMN origin TEXT NOT NULL DEFAULT 'ingest'`); err != nil {
		return err
	}
	return nil
}

// ensureColumn adds a column on the named table when it is absent. The
// caller supplies the full ALTER TABLE statement so the default and type
// stay adjacent to the original schema definition.
func ensureColumn(db *DB, table, column, alterSQL string) error {
	var count int
	if err := db.Write.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&count); err != nil {
		return fmt.Errorf("inspect %s.%s: %w", table, column, err)
	}
	if count > 0 {
		return nil
	}
	if _, err := db.Write.Exec(alterSQL); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

// Close closes both connection pools.
func (db *DB) Close() error {
	var firstErr error
	if err := db.Read.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := db.Write.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
