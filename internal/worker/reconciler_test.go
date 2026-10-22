package worker_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// The reconciler owns scheduling only: when a pass runs, and what a lifecycle
// event does to that schedule. What a pass means belongs to the service that
// implements it, so every test here counts calls rather than inspecting rows.

// The failures a test injects are static, so a reader can tell the error a case
// is about from the one the code under test produced.
var (
	errLibvirtDown  = errors.New("libvirt is not listening")
	errInstanceGone = errors.New("the instance is gone")
	errNoRoute      = errors.New("no route to the libvirt socket")
)

// reconcileTick is short enough to keep the tests fast and long enough that a pass is not
// racing the goroutine that started it.
const reconcileTick = 10 * time.Millisecond

// awaitCond polls cond until it holds or the deadline passes, so a test never
// sleeps for a fixed time to see an asynchronous effect.
func awaitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// started builds a reconciler, starts it and stops it when the test ends.
func started(t *testing.T, opts worker.ReconcilerOptions) *worker.Reconciler {
	t.Helper()

	if opts.Logger == nil {
		opts.Logger = logging.Discard()
	}
	if opts.Interval == 0 {
		opts.Interval = reconcileTick
	}

	r, err := worker.NewReconciler(opts)
	if err != nil {
		t.Fatalf("NewReconciler(): %v", err)
	}
	if err := r.Start(t.Context()); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := r.Stop(stopCtx); err != nil {
			t.Errorf("Stop(): %v", err)
		}
	})
	return r
}

func TestNewReconcilerNeedsAPass(t *testing.T) {
	t.Parallel()

	if _, err := worker.NewReconciler(worker.ReconcilerOptions{}); !errors.Is(err, worker.ErrNoPass) {
		t.Errorf("NewReconciler() without a pass = %v, want ErrNoPass", err)
	}
}

// TestReconcilerRunsOnItsTimer is the guarantee behind M4.5: whatever happens to
// the event stream, a pass keeps running.
func TestReconcilerRunsOnItsTimer(t *testing.T) {
	t.Parallel()

	var passes atomic.Int64
	started(t, worker.ReconcilerOptions{
		Pass: func(context.Context) error {
			passes.Add(1)
			return nil
		},
	})

	awaitCond(t, "the timer to run several passes", func() bool { return passes.Load() >= 3 })
}

// TestReconcilerDoesNotPassBeforeTheFirstTick keeps startup out of the
// reconciler's way: the worker pool has just requeued the tasks it recovered, and
// a pass that ran immediately would compete with them for the instance locks.
func TestReconcilerDoesNotPassBeforeTheFirstTick(t *testing.T) {
	t.Parallel()

	var passes atomic.Int64
	started(t, worker.ReconcilerOptions{
		Interval: time.Hour,
		Pass: func(context.Context) error {
			passes.Add(1)
			return nil
		},
	})

	if got := passes.Load(); got != 0 {
		t.Errorf("Start() ran %d passes, want 0 before the first tick", got)
	}
}

// TestReconcilerKeepsGoingAfterAFailedPass is why the loop exists. A libvirt that
// is unreachable for a while is exactly the situation a reconciler is for, so a
// failure must not end the schedule.
func TestReconcilerKeepsGoingAfterAFailedPass(t *testing.T) {
	t.Parallel()

	var passes atomic.Int64
	started(t, worker.ReconcilerOptions{
		Pass: func(context.Context) error {
			passes.Add(1)
			return errLibvirtDown
		},
	})

	awaitCond(t, "the loop to keep its schedule through failures",
		func() bool { return passes.Load() >= 3 })
}

// TestReconcilerActsOnALifecycleEvent is the hint half of
// docs/design/04-libvirt-integration.md §7: an event names a domain, so the
// reconciler acts on that one instead of waiting for the next tick.
func TestReconcilerActsOnALifecycleEvent(t *testing.T) {
	t.Parallel()

	events := make(chan hypervisor.DomainEvent, 1)

	var mu sync.Mutex
	var hinted []string

	started(t, worker.ReconcilerOptions{
		// A tick that never comes, so the only thing that can act is the event.
		Interval: time.Hour,
		Pass:     func(context.Context) error { return nil },
		Hint: func(_ context.Context, resourceID string) error {
			mu.Lock()
			defer mu.Unlock()
			hinted = append(hinted, resourceID)
			return nil
		},
		Events: func(context.Context) (<-chan hypervisor.DomainEvent, error) {
			return events, nil
		},
	})

	events <- hypervisor.DomainEvent{
		ID:     "i-0a1b2c3d4e5f60718",
		Action: hypervisor.ActionStopped,
		State:  hypervisor.DomainStateShutoff,
	}

	awaitCond(t, "the event to reach the hint", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(hinted) == 1 && hinted[0] == "i-0a1b2c3d4e5f60718"
	})
}

// TestReconcilerFallsBackToAFullPassWithoutAHint covers wiring events without a
// per-resource path: the hint still shortens the wait, it just costs a full pass.
func TestReconcilerFallsBackToAFullPassWithoutAHint(t *testing.T) {
	t.Parallel()

	events := make(chan hypervisor.DomainEvent, 1)

	var passes atomic.Int64
	started(t, worker.ReconcilerOptions{
		Interval: time.Hour,
		Pass: func(context.Context) error {
			passes.Add(1)
			return nil
		},
		Events: func(context.Context) (<-chan hypervisor.DomainEvent, error) {
			return events, nil
		},
	})

	events <- hypervisor.DomainEvent{ID: "i-0a1b2c3d4e5f60718", Action: hypervisor.ActionStopped}

	awaitCond(t, "the event to drive a full pass", func() bool { return passes.Load() == 1 })
}

// TestReconcilerIgnoresAnEventWithoutAnID keeps a malformed event from becoming a
// hint for the empty resource id.
func TestReconcilerIgnoresAnEventWithoutAnID(t *testing.T) {
	t.Parallel()

	events := make(chan hypervisor.DomainEvent, 2)

	var mu sync.Mutex
	var hinted []string

	started(t, worker.ReconcilerOptions{
		Interval: time.Hour,
		Pass:     func(context.Context) error { return nil },
		Hint: func(_ context.Context, resourceID string) error {
			mu.Lock()
			defer mu.Unlock()
			hinted = append(hinted, resourceID)
			return nil
		},
		Events: func(context.Context) (<-chan hypervisor.DomainEvent, error) {
			return events, nil
		},
	})

	events <- hypervisor.DomainEvent{Action: hypervisor.ActionStopped}
	events <- hypervisor.DomainEvent{ID: "i-0a1b2c3d4e5f60718", Action: hypervisor.ActionStopped}

	awaitCond(t, "the second event to be hinted", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(hinted) == 1
	})
	mu.Lock()
	defer mu.Unlock()
	if hinted[0] != "i-0a1b2c3d4e5f60718" {
		t.Errorf("hinted %q, want the event that carried an id", hinted[0])
	}
}

// TestReconcilerSurvivesAFailingHint keeps one bad event from ending the loop.
// A hint that fails is covered by the next tick like everything else.
func TestReconcilerSurvivesAFailingHint(t *testing.T) {
	t.Parallel()

	events := make(chan hypervisor.DomainEvent, 2)

	var hints atomic.Int64
	started(t, worker.ReconcilerOptions{
		Pass: func(context.Context) error { return nil },
		Hint: func(context.Context, string) error {
			hints.Add(1)
			return errInstanceGone
		},
		Events: func(context.Context) (<-chan hypervisor.DomainEvent, error) {
			return events, nil
		},
	})

	events <- hypervisor.DomainEvent{ID: "i-0a1b2c3d4e5f60718"}
	events <- hypervisor.DomainEvent{ID: "i-1b2c3d4e5f6071829"}

	awaitCond(t, "both events to be hinted", func() bool { return hints.Load() >= 2 })
}

// TestReconcilerRunsWithoutAnEventStream is the libvirt backend of this release
// and every backend that never grows one: ErrNotImplemented degrades to
// timer-only reconciles instead of failing startup.
func TestReconcilerRunsWithoutAnEventStream(t *testing.T) {
	t.Parallel()

	var subscribes, passes atomic.Int64
	started(t, worker.ReconcilerOptions{
		Pass: func(context.Context) error {
			passes.Add(1)
			return nil
		},
		Events: func(context.Context) (<-chan hypervisor.DomainEvent, error) {
			subscribes.Add(1)
			return nil, hypervisor.ErrNotImplemented
		},
	})

	awaitCond(t, "the timer to keep running without events",
		func() bool { return passes.Load() >= 3 })

	// ErrNotImplemented is permanent: retrying it would log the same line every
	// interval for the life of the process.
	if got := subscribes.Load(); got != 1 {
		t.Errorf("subscribed %d times, want 1: a backend with no events never grows one", got)
	}
}

// TestReconcilerResubscribesAfterTheStreamDrops is the reconnect case. go-libvirt
// closes the event channel when the socket goes, and a stream that simply ended
// would downgrade Kumo to timer-only for the rest of the process's life.
func TestReconcilerResubscribesAfterTheStreamDrops(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	streams := make([]chan hypervisor.DomainEvent, 0, 2)

	var hints atomic.Int64
	started(t, worker.ReconcilerOptions{
		Pass: func(context.Context) error { return nil },
		Hint: func(context.Context, string) error {
			hints.Add(1)
			return nil
		},
		Events: func(context.Context) (<-chan hypervisor.DomainEvent, error) {
			mu.Lock()
			defer mu.Unlock()
			ch := make(chan hypervisor.DomainEvent, 1)
			streams = append(streams, ch)
			return ch, nil
		},
	})

	awaitCond(t, "the first subscription", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(streams) == 1
	})

	// The connection goes, which go-libvirt reports by closing the channel.
	mu.Lock()
	close(streams[0])
	mu.Unlock()

	awaitCond(t, "a second subscription", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(streams) >= 2
	})

	// The new stream has to work, not merely exist.
	mu.Lock()
	second := streams[1]
	mu.Unlock()
	second <- hypervisor.DomainEvent{ID: "i-0a1b2c3d4e5f60718"}

	awaitCond(t, "the re-registered stream to deliver", func() bool { return hints.Load() >= 1 })
}

// TestReconcilerRetriesAFailedSubscription covers a libvirt that is down at
// startup: the stream is worth retrying, unlike ErrNotImplemented.
func TestReconcilerRetriesAFailedSubscription(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int64
	started(t, worker.ReconcilerOptions{
		Pass: func(context.Context) error { return nil },
		Events: func(context.Context) (<-chan hypervisor.DomainEvent, error) {
			attempts.Add(1)
			return nil, errNoRoute
		},
	})

	awaitCond(t, "the subscription to be retried", func() bool { return attempts.Load() >= 2 })
}

// TestReconcilerStopEndsTheLoop makes shutdown deterministic: nothing runs a pass
// after Stop returns.
func TestReconcilerStopEndsTheLoop(t *testing.T) {
	t.Parallel()

	var passes atomic.Int64
	r, err := worker.NewReconciler(worker.ReconcilerOptions{
		Logger:   logging.Discard(),
		Interval: reconcileTick,
		Pass: func(context.Context) error {
			passes.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewReconciler(): %v", err)
	}
	if err := r.Start(t.Context()); err != nil {
		t.Fatalf("Start(): %v", err)
	}

	awaitCond(t, "the loop to get going", func() bool { return passes.Load() >= 1 })

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Stop(stopCtx); err != nil {
		t.Fatalf("Stop(): %v", err)
	}

	after := passes.Load()
	time.Sleep(5 * reconcileTick)
	if got := passes.Load(); got != after {
		t.Errorf("the loop ran %d more passes after Stop returned", got-after)
	}
}

// TestReconcilerStopIsIdempotent keeps a double shutdown from panicking on a
// closed channel, which is what a deferred Stop beside an explicit one does.
func TestReconcilerStopIsIdempotent(t *testing.T) {
	t.Parallel()

	r := started(t, worker.ReconcilerOptions{
		Pass: func(context.Context) error { return nil },
	})

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Stop(stopCtx); err != nil {
		t.Fatalf("the first Stop(): %v", err)
	}
	if err := r.Stop(stopCtx); err != nil {
		t.Errorf("the second Stop(): %v", err)
	}
}
