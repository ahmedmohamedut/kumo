// Package storetest provides a migrated, throwaway SQLite database for tests.
package storetest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/store"
)

// New opens a store backed by a temporary database file that has the full
// schema applied. The file is removed when the test finishes.
func New(t *testing.T) *store.Store {
	t.Helper()

	return NewAt(t, filepath.Join(t.TempDir(), "kumo.db"))
}

// NewAt opens a store at an explicit path with the full schema applied.
func NewAt(t *testing.T, path string) *store.Store {
	t.Helper()

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("store.Close(): %v", err)
		}
	})

	return db
}
