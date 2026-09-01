package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	// Pure-Go SQLite: no cgo, which is what keeps Gravy a single portable binary.
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB is the store. gravyd is the only writer; clients reach it through the API.
type DB struct {
	sql *sql.DB
	// writeMu serializes writers in Go rather than letting SQLite discover the conflict.
	//
	// WAL permits many concurrent readers alongside one writer, which is exactly the shape
	// Gravy wants: the TUI reads constantly while the daemon writes. What WAL does not permit
	// is two writers, and two connections from the same process racing to write produce
	// SQLITE_BUSY that busy_timeout cannot resolve, because neither side yields until its
	// transaction ends. Holding a mutex over writes keeps reads parallel and writes ordered.
	writeMu sync.Mutex
}

// Open opens (creating if needed) the database at path and applies any pending migrations.
//
// WAL is enabled so readers never block the single writer; foreign keys are on so the cascade
// deletes in the schema actually fire; busy_timeout absorbs the brief contention of a writer
// committing while readers are active.
func Open(ctx context.Context, path string) (*DB, error) {
	dsn := path + "?" + url.Values{"_pragma": {
		"journal_mode(WAL)",
		"foreign_keys(ON)",
		"busy_timeout(5000)",
		// NORMAL is the documented companion to WAL: durable across process crashes, which
		// is the failure that matters here, without an fsync per commit.
		"synchronous(NORMAL)",
	}}.Encode()
	// url.Values escapes the parentheses; SQLite's driver wants them literal.
	dsn = strings.NewReplacer("%28", "(", "%29", ")").Replace(dsn)

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	db := &DB{sql: sqlDB}
	if err := db.migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// SQL exposes the underlying handle for packages that need it. Prefer the typed methods.
func (d *DB) SQL() *sql.DB { return d.sql }

// migration is one numbered, forward-only schema step.
type migration struct {
	version int
	name    string
	sql     string
}

// migrate applies every migration not yet recorded, in order, each in its own transaction.
// Re-opening an up-to-date database applies nothing.
func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return err
	}

	all, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range all {
		if applied[m.version] {
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("read schema_migrations: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return applied, nil
}

func (d *DB) applyMigration(ctx context.Context, m migration) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", m.version, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after a successful commit is a no-op

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, unixepoch())`,
		m.version, m.name,
	); err != nil {
		return fmt.Errorf("record migration %d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", m.version, err)
	}
	return nil
}

// loadMigrations reads the embedded migrations, ordered by version. Filenames are
// NNNN_name.sql; the numeric prefix is the version.
func loadMigrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %s and %s share version %d", prev, e.Name(), version)
		}
		seen[version] = e.Name()

		body, err := migrationFS.ReadFile(filepath.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: name, sql: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func parseMigrationName(filename string) (int, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	numStr, name, ok := strings.Cut(base, "_")
	if !ok {
		return 0, "", fmt.Errorf("migration %q is not named NNNN_name.sql", filename)
	}
	var version int
	if _, err := fmt.Sscanf(numStr, "%d", &version); err != nil || version < 1 {
		return 0, "", fmt.Errorf("migration %q has no valid version prefix", filename)
	}
	return version, name, nil
}

// Tx runs fn in a write transaction, committing on success and rolling back on error or panic.
// Writers are serialized; readers are not blocked by it.
func (d *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return d.tx(ctx, fn)
}

// tx is Tx without the lock, for callers that already hold it.
func (d *DB) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// exec runs a single write statement, serialized with every other writer.
func (d *DB) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return d.sql.ExecContext(ctx, query, args...)
}
