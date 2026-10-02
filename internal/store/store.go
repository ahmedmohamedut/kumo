// Package store owns the SQLite metadata database: connection setup with the
// pragmas from docs/design/02-data-model.md, embedded goose migrations and the
// sqlc-generated queries. It is the only package that talks SQL.
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
	"strings"
	"sync"
	"time"

	"github.com/pressly/goose/v3"
	// modernc.org/sqlite is the pure Go driver: no cgo (AGENTS.md, rule 7).
	_ "modernc.org/sqlite"

	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// driverName is the database/sql driver registered by modernc.org/sqlite.
const driverName = "sqlite"

// ErrEmptyPath is returned when a store is opened without a database path.
var ErrEmptyPath = errors.New("store: database path must not be empty")

// Store is the metadata database. It embeds the sqlc queries, so services call
// store methods (or the generated ones) instead of writing SQL.
type Store struct {
	*sqlc.Queries

	db *sql.DB
}

// Open opens (and creates, if missing) the SQLite database at path, applies
// the pragmas and runs the embedded migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, ErrEmptyPath
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", dir, err)
		}
	}

	db, err := open(ctx, "file:"+path)
	if err != nil {
		return nil, err
	}

	store := New(db)
	if err := store.Migrate(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}

	return store, nil
}

// open dials SQLite with the pragmas of docs/design/02-data-model.md.
func open(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open(driverName, withPragmas(dsn))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", dsn, err)
	}

	// One writer connection keeps SQLite happy without retry loops; WAL still
	// gives readers a consistent snapshot.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(time.Minute)

	if err := db.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("store: ping %s: %w", dsn, err), db.Close())
	}

	return db, nil
}

// withPragmas appends the pragmas Kumo relies on to a SQLite DSN.
func withPragmas(dsn string) string {
	pragmas := []string{
		"journal_mode(WAL)",
		"foreign_keys(ON)",
		"busy_timeout(5000)",
		"synchronous(NORMAL)",
	}

	values := url.Values{}
	for _, pragma := range pragmas {
		values.Add("_pragma", pragma)
	}
	values.Set("_time_format", "sqlite")

	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}

	return dsn + separator + values.Encode()
}

// New wraps an already open database handle, without touching the schema.
func New(db *sql.DB) *Store {
	return &Store{Queries: sqlc.New(db), db: db}
}

// configureGoose sets goose's package-level configuration exactly once. goose
// keeps it in globals, so configuring it on every Migrate would race whenever
// two stores migrate concurrently (which the tests do).
var configureGoose = sync.OnceValue(func() error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	return goose.SetDialect(driverName)
})

// Migrate applies the embedded goose migrations.
func (s *Store) Migrate(ctx context.Context) error {
	if err := configureGoose(); err != nil {
		return fmt.Errorf("store: configure goose: %w", err)
	}

	if err := goose.UpContext(ctx, s.db, "migrations"); err != nil {
		return fmt.Errorf("store: apply migrations: %w", err)
	}
	return nil
}

// Ping reports whether the database answers.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
}

// Exec runs a parameterized statement. Services should use the sqlc queries in
// internal/store/queries; this escape hatch is for migrations, maintenance and
// tests.
func (s *Store) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, query, args...)
}

// DB exposes the underlying handle for the few places that need it
// (health probes, sqlc transactions).
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

// Tx is a store handle bound to an open transaction.
type Tx struct {
	*sqlc.Queries

	tx *sql.Tx
}

// Exec runs a parameterized statement inside the transaction, see Store.Exec.
func (tx *Tx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.tx.ExecContext(ctx, query, args...)
}

// WithTx runs fn inside a transaction, committing on success and rolling back
// on error or panic.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, tx *Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := fn(ctx, &Tx{Queries: sqlc.New(tx), tx: tx}); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	committed = true

	return nil
}

// Migrations returns the embedded migration files, newest version last. It is
// used by tests to assert the schema is complete.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic("store: embedded migrations are missing: " + err.Error())
	}
	return sub
}
