package store_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
)

func TestOpenAppliesMigrationsOnAnEmptyDatabase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "kumo.db")

	db, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck // closing cannot fail here

	for _, table := range []string{
		"users", "refresh_tokens", "key_pairs", "images", "instance_types",
		"instances", "tags", "tasks", "settings", "goose_db_version",
		"vpcs", "subnets", "network_interfaces",
	} {
		var name string
		err := db.DB().QueryRowContext(t.Context(),
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %q is missing after migrating: %v", table, err)
		}
	}

	// goose seeds version 0 when it creates its bookkeeping table, so count the
	// migrations that actually carry a version.
	var count int
	err = db.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM goose_db_version WHERE is_applied = 1 AND version_id > 0`).Scan(&count)
	if err != nil {
		t.Fatalf("query goose_db_version: %v", err)
	}
	if count != 2 {
		t.Errorf("applied migrations = %d, want 2", count)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "kumo.db")

	first, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	second, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	t.Cleanup(func() {
		first.Close()  //nolint:errcheck // closing cannot fail here
		second.Close() //nolint:errcheck // closing cannot fail here
	})

	var applied int
	if err := second.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM goose_db_version WHERE is_applied = 1 AND version_id > 0`).Scan(&applied); err != nil {
		t.Fatalf("query goose_db_version: %v", err)
	}
	if applied != 2 {
		t.Errorf("applied migrations after re-opening = %d, want 2", applied)
	}
}

func TestOpenAppliesPragmas(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	ctx := t.Context()

	tests := []struct {
		pragma string
		want   string
	}{
		{pragma: "journal_mode", want: "wal"},
		{pragma: "foreign_keys", want: "1"},
		{pragma: "busy_timeout", want: "5000"},
		{pragma: "synchronous", want: "1"},
	}

	for _, tt := range tests {
		t.Run(tt.pragma, func(t *testing.T) {
			var got string
			query := "PRAGMA " + tt.pragma
			if err := db.DB().QueryRowContext(ctx, query).Scan(&got); err != nil {
				t.Fatalf("%s: %v", query, err)
			}
			if !strings.EqualFold(got, tt.want) {
				t.Errorf("%s = %q, want %q", query, got, tt.want)
			}
		})
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)

	_, err := db.Exec(t.Context(),
		`INSERT INTO instances (id, name, state, image_id, instance_type, vcpus, memory_mib,
			root_disk_gb, launched_at, updated_at)
		 VALUES ('i-1', 'web', 'running', 'img-missing', 'kumo.small', 1, 1024, 10, 'now', 'now')`)
	if err == nil {
		t.Fatal("inserting an instance with an unknown image_id succeeded, want a foreign key error")
	}
}

func TestOpenCreatesMissingDirectories(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "deeper", "kumo.db")

	db, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck // closing cannot fail here

	if err := db.Ping(t.Context()); err != nil {
		t.Errorf("Ping() error = %v", err)
	}
}

func TestOpenRejectsAnEmptyPath(t *testing.T) {
	t.Parallel()

	if _, err := store.Open(t.Context(), ""); err == nil {
		t.Fatal("store.Open(\"\") error = nil, want an error")
	}
}

func TestMigrateFailureClosesTheDatabase(t *testing.T) {
	t.Parallel()

	// A directory in place of the database file makes both the ping and the
	// migrations fail; the returned store must not leak the handle.
	path := filepath.Join(t.TempDir(), "kumo.db")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatalf("create directory: %v", err)
	}

	db, err := store.Open(t.Context(), path)
	if err == nil {
		db.Close() //nolint:errcheck // the test ends here
		t.Fatal("store.Open() error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "store:") {
		t.Errorf("error %q is not wrapped by the store package", err)
	}
}

func TestCountUsers(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	ctx := t.Context()

	count, err := db.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers() error = %v", err)
	}
	if count != 0 {
		t.Errorf("CountUsers() = %d on an empty database, want 0", count)
	}

	insertUser(t, db, "u-1", "admin")
	insertUser(t, db, "u-2", "viewer")

	count, err = db.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers() error = %v", err)
	}
	if count != 2 {
		t.Errorf("CountUsers() = %d, want 2", count)
	}
}

func TestPingFailsOnAClosedDatabase(t *testing.T) {
	t.Parallel()

	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "kumo.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if err := db.Ping(t.Context()); err == nil {
		t.Fatal("Ping() on a closed database = nil, want an error")
	}
	// database/sql documents Close as idempotent, so a second call is fine.
	if err := db.Close(); err != nil {
		t.Errorf("second Close() error = %v", err)
	}
}

// errRollback stands in for a workflow failure inside a transaction.
var errRollback = errors.New("rollback me")

func TestWithTx(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	ctx := t.Context()

	t.Run("commits", func(t *testing.T) {
		err := db.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO users (id, username, password_hash, role, created_at, updated_at)
				 VALUES ('u-1', 'admin', 'hash', 'admin', 'now', 'now')`)
			return err
		})
		if err != nil {
			t.Fatalf("WithTx() error = %v", err)
		}

		count, err := db.CountUsers(ctx)
		if err != nil {
			t.Fatalf("CountUsers() error = %v", err)
		}
		if count != 1 {
			t.Errorf("CountUsers() = %d, want 1", count)
		}
	})

	t.Run("rolls back on error", func(t *testing.T) {
		err := db.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
			if _, err := tx.Exec(ctx,
				`INSERT INTO users (id, username, password_hash, role, created_at, updated_at)
				 VALUES ('u-2', 'viewer', 'hash', 'viewer', 'now', 'now')`); err != nil {
				return err
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatalf("WithTx() error = %v, want %v", err, errRollback)
		}

		var name string
		err = db.DB().QueryRowContext(ctx, `SELECT username FROM users WHERE id = 'u-2'`).Scan(&name)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("the rolled back row is still there (err = %v)", err)
		}
	})

	t.Run("rolls back on panic", func(t *testing.T) {
		func() {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Error("the panic did not escape WithTx")
				}
			}()
			_ = db.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
				if _, err := tx.Exec(ctx,
					`INSERT INTO users (id, username, password_hash, role, created_at, updated_at)
					 VALUES ('u-3', 'panic', 'hash', 'viewer', 'now', 'now')`); err != nil {
					return err
				}
				panic("boom")
			})
		}()

		var name string
		err := db.DB().QueryRowContext(ctx, `SELECT username FROM users WHERE id = 'u-3'`).Scan(&name)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("the rolled back row is still there (err = %v)", err)
		}
	})
}

func TestMigrationsAreEmbedded(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(store.Migrations(), ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}

	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 2 || names[0] != "0001_init.sql" || names[1] != "0002_network.sql" {
		t.Errorf("embedded migrations = %v, want [0001_init.sql 0002_network.sql]", names)
	}
}

func insertUser(t *testing.T, db *store.Store, id, role string) {
	t.Helper()

	_, err := db.Exec(t.Context(),
		`INSERT INTO users (id, username, password_hash, role, created_at, updated_at)
		 VALUES (?, ?, 'hash', ?, 'now', 'now')`, id, id, role)
	if err != nil {
		t.Fatalf("insert user %q: %v", id, err)
	}
}

func TestAllocatedCapacity(t *testing.T) {
	t.Parallel()

	// A row an earlier release left in `terminated` holds nothing, so it must
	// not keep consuming the node's capacity.
	tests := []struct {
		name      string
		instances []allocatedInstance
		want      hypervisor.Capacity
	}{
		{
			name: "an empty database allocates nothing",
		},
		{
			name: "live instances add up",
			instances: []allocatedInstance{
				{id: "i-aaa", state: "running", vcpus: 2, memoryMiB: 4096, diskGB: 20},
				{id: "i-bbb", state: "stopped", vcpus: 1, memoryMiB: 1024, diskGB: 10},
				{id: "i-ccc", state: "pending", vcpus: 4, memoryMiB: 8192, diskGB: 40},
			},
			want: hypervisor.Capacity{VCPUs: 7, MemoryBytes: 13 << 30, DiskBytes: 70 << 30},
		},
		{
			name: "terminated instances are left out",
			instances: []allocatedInstance{
				{id: "i-aaa", state: "running", vcpus: 2, memoryMiB: 4096, diskGB: 20},
				{id: "i-bbb", state: "terminated", vcpus: 8, memoryMiB: 32768, diskGB: 100},
			},
			want: hypervisor.Capacity{VCPUs: 2, MemoryBytes: 4 << 30, DiskBytes: 20 << 30},
		},
		{
			name: "a node whose instances were all terminated is free again",
			instances: []allocatedInstance{
				{id: "i-aaa", state: "terminated", vcpus: 8, memoryMiB: 32768, diskGB: 100},
			},
			want: hypervisor.Capacity{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db := storetest.New(t)
			ctx := t.Context()
			for _, in := range tt.instances {
				insertAllocatedInstance(t, db, in)
			}

			got, err := db.AllocatedCapacity(ctx)
			if err != nil {
				t.Fatalf("AllocatedCapacity() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("AllocatedCapacity() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// allocatedInstance is one row of the instances table that the capacity sum
// reads. It carries only the columns the sum touches.
type allocatedInstance struct {
	id        string
	state     string
	vcpus     int64
	memoryMiB int64
	diskGB    int64
}

func insertAllocatedInstance(t *testing.T, db *store.Store, in allocatedInstance) {
	t.Helper()

	ctx := t.Context()
	if _, err := db.Exec(ctx,
		`INSERT OR IGNORE INTO instance_types (id, vcpus, memory_mib) VALUES ('kumo.small', 1, 2048)`); err != nil {
		t.Fatalf("insert instance type: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT OR IGNORE INTO images (id, name, state, created_at, updated_at)
		 VALUES ('img-00000000000000000', 'img-00000000000000000', 'available', 'now', 'now')`); err != nil {
		t.Fatalf("insert image: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO instances (id, name, state, image_id, instance_type, vcpus, memory_mib,
		   root_disk_gb, launched_at, updated_at)
		 VALUES (?, ?, ?, 'img-00000000000000000', 'kumo.small', ?, ?, ?, 'now', 'now')`,
		in.id, in.id, in.state, in.vcpus, in.memoryMiB, in.diskGB); err != nil {
		t.Fatalf("insert instance %q: %v", in.id, err)
	}
}
