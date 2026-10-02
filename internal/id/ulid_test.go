package id_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/id"
)

func TestULIDShape(t *testing.T) {
	t.Parallel()

	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

	value := id.ULID()
	if len(value) != id.ULIDSize {
		t.Fatalf("len(ULID()) = %d, want %d", len(value), id.ULIDSize)
	}
	for i, r := range value {
		if !strings.ContainsRune(alphabet, r) {
			t.Errorf("ULID()[%d] = %q, want a Crockford Base32 character", i, r)
		}
	}
}

func TestULIDIsUnique(t *testing.T) {
	t.Parallel()

	const count = 10_000

	seen := make(map[string]struct{}, count)
	for range count {
		value := id.ULID()
		if _, dup := seen[value]; dup {
			t.Fatalf("ULID() returned a duplicate: %s", value)
		}
		seen[value] = struct{}{}
	}
}

func TestULIDIsSortable(t *testing.T) {
	t.Parallel()

	older := id.ULID()
	time.Sleep(2 * time.Millisecond)
	newer := id.ULID()

	if older >= newer {
		t.Errorf("ULID() = %q then %q, want increasing values", older, newer)
	}
}

func TestULIDConcurrent(t *testing.T) {
	t.Parallel()

	const goroutines = 8
	const each = 500

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		all []string
	)

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			local := make([]string, 0, each)
			for range each {
				local = append(local, id.ULID())
			}
			mu.Lock()
			all = append(all, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	seen := make(map[string]struct{}, len(all))
	for _, value := range all {
		if len(value) != id.ULIDSize {
			t.Fatalf("len(ULID()) = %d, want %d", len(value), id.ULIDSize)
		}
		if _, dup := seen[value]; dup {
			t.Fatalf("ULID() returned a duplicate: %s", value)
		}
		seen[value] = struct{}{}
	}
}
