// Package locks serializes the operations of one resource at a time inside one
// process.
//
// Two actions on the same instance have to be ordered, not run side by side: a
// terminate that cleans the volumes while a start is booting the guest leaves
// both half done. Ordering them in the database alone is not enough, because the
// work itself happens in the worker and not in the transaction that recorded it,
// and a second process reading the same SQLite file would see the same pending
// task without sharing a mutex.
//
// So the rule of docs/design/00-architecture.md §4 is implemented here rather
// than in each workflow: a keyed lock, held for as long as an operation works on
// its resource. A request that cannot take the lock is refused with a conflict
// instead of queueing behind a workflow it knows nothing about, and a worker
// waits, because two tasks for one resource have to run in some order anyway.
package locks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrHeld means another operation holds the lock for a key.
var ErrHeld = errors.New("worker/locks: the resource is held by another operation")

// retryInterval is how often a waiting worker looks at the lock again. It is
// short because the wait is for a workflow that is already running, and long
// enough that a queue of waiting workers does not spin.
const retryInterval = 20 * time.Millisecond

// Set is a set of per-key locks. It is safe for concurrent use, and the zero value
// is not usable: build one with New.
type Set struct {
	mu   sync.Mutex
	held map[string]string
}

// New builds an empty set.
func New() *Set { return &Set{held: make(map[string]string)} }

// TryAcquire takes the lock for key on behalf of `by` and returns the function
// that releases it. It never waits: a caller that cannot have the key right now
// gets ErrHeld, which the api layer turns into the 409 the contract promises for
// a conflicting action (docs/design/01-api.md §Instances).
//
// The release function is safe to call more than once, and releases only the hold
// of the caller: a double release must not free a lock somebody else has taken in
// the meantime.
func (s *Set) TryAcquire(key, by string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if holder, ok := s.held[key]; ok {
		return nil, fmt.Errorf("%w: %q is held by %s", ErrHeld, key, holder)
	}
	s.held[key] = by

	return s.releaser(key), nil
}

// Acquire waits for the lock of key until it is free or ctx is done. Workers use
// it so that two tasks for one resource are ordered rather than refused: the
// second task is already queued and will run, so failing it would report a
// contention an operator did not cause as an error they have to act on.
//
// The context is what keeps this from waiting forever. A workflow that outlives
// its shutdown hands the lock back and the task is requeued (worker.Pool.Recover).
func (s *Set) Acquire(ctx context.Context, key, by string) (func(), error) {
	for {
		release, err := s.TryAcquire(key, by)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, ErrHeld) {
			return nil, err
		}

		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("wait for the lock on %q: %w", key, ctx.Err())
		case <-timer.C:
		}
	}
}

// Holder reports which operation holds a key, and whether it is held at all. A
// caller turning ErrHeld into a message for a user needs the name of the
// operation, not just the fact that something is busy.
func (s *Set) Holder(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	holder, ok := s.held[key]
	return holder, ok
}

// releaser returns the release function for a hold of key.
func (s *Set) releaser(key string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.held, key)
		})
	}
}
