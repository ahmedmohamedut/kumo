package locks_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/worker/locks"
)

func TestTryAcquireIsExclusive(t *testing.T) {
	t.Parallel()

	set := locks.New()
	release, err := set.TryAcquire("instance:i-1", "instance.start")
	if err != nil {
		t.Fatalf("TryAcquire(): %v", err)
	}

	// A second operation on the same key has to be told, not left waiting: the
	// caller turns this into the 409 the contract promises.
	if _, err := set.TryAcquire("instance:i-1", "instance.terminate"); !errors.Is(err, locks.ErrHeld) {
		t.Fatalf("TryAcquire() on a held key = %v, want ErrHeld", err)
	}

	// A different key is unaffected: the lock is per resource, not global.
	other, err := set.TryAcquire("instance:i-2", "instance.start")
	if err != nil {
		t.Fatalf("TryAcquire() on a free key: %v", err)
	}
	other()
	release()

	// Releasing frees the key for the next operation.
	next, err := set.TryAcquire("instance:i-1", "instance.stop")
	if err != nil {
		t.Fatalf("TryAcquire() after release: %v", err)
	}
	next()
}

func TestHolderNamesTheOperationThatHolds(t *testing.T) {
	t.Parallel()

	set := locks.New()
	if holder, ok := set.Holder("instance:i-1"); ok || holder != "" {
		t.Fatalf("Holder() on a free key = %q, %t, want \"\", false", holder, ok)
	}

	release, err := set.TryAcquire("instance:i-1", "instance.terminate")
	if err != nil {
		t.Fatalf("TryAcquire(): %v", err)
	}
	defer release()

	holder, ok := set.Holder("instance:i-1")
	if !ok || holder != "instance.terminate" {
		t.Errorf("Holder() = %q, %t, want instance.terminate, true", holder, ok)
	}

	_, busy := set.TryAcquire("instance:i-1", "instance.start")
	if busy == nil {
		t.Fatal("TryAcquire() on a held key succeeded")
	}
	// The message names the holder, so the console can say what to wait for.
	if !strings.Contains(busy.Error(), "instance.terminate") {
		t.Errorf("error = %q, want it to name instance.terminate", busy)
	}
}

func TestReleaseOnlyFreesTheHoldItOwns(t *testing.T) {
	t.Parallel()

	set := locks.New()
	release, err := set.TryAcquire("instance:i-1", "instance.start")
	if err != nil {
		t.Fatalf("TryAcquire(): %v", err)
	}
	release()
	release() // a second release must not free somebody else's hold below

	held, err := set.TryAcquire("instance:i-1", "instance.stop")
	if err != nil {
		t.Fatalf("TryAcquire(): %v", err)
	}
	defer held()

	release()

	if _, ok := set.Holder("instance:i-1"); !ok {
		t.Error("a repeated release of an old hold freed a newer one")
	}
}

func TestAcquireWaitsForTheRelease(t *testing.T) {
	t.Parallel()

	set := locks.New()
	release, err := set.TryAcquire("instance:i-1", "instance.launch")
	if err != nil {
		t.Fatalf("TryAcquire(): %v", err)
	}

	var (
		wg       sync.WaitGroup
		acquired = make(chan struct{})
		errs     = make(chan error, 1)
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		handed, err := set.Acquire(t.Context(), "instance:i-1", "instance.terminate")
		if err != nil {
			errs <- err
			return
		}
		close(acquired)
		handed()
	}()

	select {
	case err := <-errs:
		t.Fatalf("Acquire(): %v", err)
	case <-acquired:
		t.Fatal("Acquire() returned while the key was still held")
	case <-time.After(50 * time.Millisecond):
	}

	release()

	select {
	case err := <-errs:
		t.Fatalf("Acquire(): %v", err)
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire() did not return after the key was released")
	}
	wg.Wait()
}

func TestAcquireHonorsTheContext(t *testing.T) {
	t.Parallel()

	set := locks.New()
	release, err := set.TryAcquire("instance:i-1", "instance.launch")
	if err != nil {
		t.Fatalf("TryAcquire(): %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// A worker that is shutting down must not sit in a lock it cannot have: the
	// task is requeued instead, and the next run adopts what this one made.
	if _, err := set.Acquire(ctx, "instance:i-1", "instance.terminate"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire() with a done context = %v, want context.Canceled", err)
	}
}
