// Package store is forge's persistence layer: a SQLite database holding
// pipelines, runs, jobs, attempts, log metadata, artifact metadata and the run
// event timeline.
//
// The driver is modernc.org/sqlite, a pure-Go implementation, so forge builds and
// runs with CGO disabled and needs no C toolchain to clone-and-run.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a lookup by ID or name matches nothing.
var ErrNotFound = errors.New("not found")

// DB wraps the SQLite connection pool.
//
// The pool is limited to a single connection. SQLite allows one writer at a
// time, and serialising every statement removes an entire class of "database is
// locked" failures at a cost that does not matter at the scale forge runs at:
// queries here are sub-millisecond and no transaction is held open across I/O.
type DB struct {
	sql  *sql.DB
	path string
}

// Options configures Open.
type Options struct {
	// Path is the SQLite file. Parent directories are created if needed.
	Path string
	// BusyTimeout bounds how long a statement waits on a locked database.
	BusyTimeout time.Duration
}

// Open connects to the database and applies any outstanding migrations.
func Open(ctx context.Context, opts Options) (*DB, error) {
	if opts.Path == "" {
		return nil, errors.New("store: database path is required")
	}
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = 5 * time.Second
	}
	if dir := filepath.Dir(opts.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("store: create database directory: %w", err)
		}
	}

	handle, err := sql.Open("sqlite", dsn(opts))
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	handle.SetMaxOpenConns(1)
	handle.SetMaxIdleConns(1)
	handle.SetConnMaxLifetime(0)

	if err := handle.PingContext(ctx); err != nil {
		_ = handle.Close()
		return nil, fmt.Errorf("store: connect to %s: %w", opts.Path, err)
	}

	db := &DB{sql: handle, path: opts.Path}
	if err := db.Migrate(ctx); err != nil {
		_ = handle.Close()
		return nil, err
	}
	return db, nil
}

// dsn builds the connection string. WAL keeps readers from blocking the writer,
// foreign keys are enabled so cascade deletes actually cascade, and `immediate`
// transactions take the write lock upfront rather than failing on upgrade.
func dsn(opts Options) string {
	params := url.Values{}
	params.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", opts.BusyTimeout.Milliseconds()))
	params.Add("_pragma", "journal_mode(WAL)")
	params.Add("_pragma", "foreign_keys(1)")
	params.Add("_pragma", "synchronous(NORMAL)")
	params.Add("_txlock", "immediate")
	return "file:" + opts.Path + "?" + params.Encode()
}

// Close releases the connection pool.
func (db *DB) Close() error {
	if db == nil || db.sql == nil {
		return nil
	}
	return db.sql.Close()
}

// Path is the database file location.
func (db *DB) Path() string { return db.path }

// SQL exposes the underlying handle for tests and ad-hoc queries.
func (db *DB) SQL() *sql.DB { return db.sql }

// Migrate applies every embedded migration that has not run yet.
//
// Each migration runs inside its own transaction together with the row recording
// it, so a failure leaves the database at the last fully-applied version rather
// than half-way through. Re-running is a no-op, which is what makes restarts safe.
func (db *DB) Migrate(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied, err := db.appliedMigrations(ctx)
	if err != nil {
		return err
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := db.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

type migration struct {
	version string
	body    string
}

// loadMigrations reads the embedded .sql files in lexical order, which is why
// they are named with a zero-padded numeric prefix.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", e.Name(), err)
		}
		out = append(out, migration{
			version: strings.TrimSuffix(e.Name(), ".sql"),
			body:    string(body),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func (db *DB) appliedMigrations(ctx context.Context) (map[string]bool, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: read applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: scan migration version: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate migrations: %w", err)
	}
	return applied, nil
}

func (db *DB) applyMigration(ctx context.Context, m migration) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %s: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return fmt.Errorf("store: apply migration %s: %w", m.version, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.version, nowMillis()); err != nil {
		return fmt.Errorf("store: record migration %s: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", m.version, err)
	}
	return nil
}

// AppliedVersions lists the migrations that have run, oldest first.
func (db *DB) AppliedVersions(ctx context.Context) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("store: list migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// --- time helpers -----------------------------------------------------------
//
// Timestamps cross the boundary as Unix milliseconds. Nullable columns use a
// *time.Time on the Go side and NULL in SQLite.

func nowMillis() int64 { return time.Now().UnixMilli() }

func toMillis(t time.Time) int64 { return t.UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func toNullMillis(t *time.Time) sql.NullInt64 {
	if t == nil || t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func fromNullMillis(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

func toNullInt(v *int) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}

func fromNullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}
