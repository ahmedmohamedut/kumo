package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
)

// DefaultReconcileInterval is how often the full pass runs. Ten seconds is the
// figure docs/design/00-architecture.md §4 commits to and the one M4.5's
// acceptance criterion measures: a guest destroyed out of band is reflected in
// the console within about that long.
const DefaultReconcileInterval = 10 * time.Second

// reconcileHintBuffer is how many event-driven hints may queue before the
// reconciler drops them. A dropped hint costs nothing but latency: the periodic
// pass is the safety net, and the whole point of §7 is that events are an
// optimisation.
const reconcileHintBuffer = 64

// Pass is one full reconcile. The loop owns when it runs; what it means lives in
// the service that implements it, so this package stays scheduling only.
type Pass func(ctx context.Context) error

// Hint reconciles a single resource, which is what a lifecycle event names.
type Hint func(ctx context.Context, resourceID string) error

// EventSource opens a stream of hypervisor lifecycle events. A backend that does
// not have one returns hypervisor.ErrNotImplemented and the reconciler runs on
// its timer alone.
type EventSource func(ctx context.Context) (<-chan hypervisor.DomainEvent, error)

// ReconcilerOptions configures the loop.
type ReconcilerOptions struct {
	// Logger records every pass that changed something and every failure.
	Logger *slog.Logger
	// Pass runs one full reconcile. Required.
	Pass Pass
	// Hint reconciles the one resource an event named. Optional: without it,
	// an event triggers a full pass instead.
	Hint Hint
	// Events subscribes to hypervisor lifecycle events. Optional.
	Events EventSource
	// Interval is how often Pass runs. Zero means DefaultReconcileInterval.
	Interval time.Duration
}

// Reconciler keeps the database in step with the hypervisor.
//
// Two things drive it: a ticker, which is the guarantee, and the hypervisor's
// own lifecycle events, which are a hint that shortens the wait
// (docs/design/04-libvirt-integration.md §7). Everything the stream misses — a
// dropped connection, a backend with no events at all, an event for a resource
// whose row was being written at the time — is picked up by the next tick, which
// is why losing events is never an error here.
type Reconciler struct {
	logger   *slog.Logger
	pass     Pass
	hint     Hint
	events   EventSource
	interval time.Duration

	// hints carries resource ids from the event stream to the loop, so a burst
	// of events cannot run passes concurrently.
	hints chan string

	wg     sync.WaitGroup
	cancel context.CancelFunc
	once   sync.Once
}

// ErrNoPass means ReconcilerOptions.Pass was nil, which is a wiring mistake
// rather than a runtime condition.
var ErrNoPass = errors.New("worker: the reconciler needs a pass to run")

// NewReconciler builds the loop. Start brings it up.
func NewReconciler(opts ReconcilerOptions) (*Reconciler, error) {
	if opts.Pass == nil {
		return nil, ErrNoPass
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultReconcileInterval
	}

	return &Reconciler{
		logger:   opts.Logger,
		pass:     opts.Pass,
		hint:     opts.Hint,
		events:   opts.Events,
		interval: opts.Interval,
		hints:    make(chan string, reconcileHintBuffer),
	}, nil
}

// Start runs the loop in the background and returns once it is going.
//
// It does not run a pass before returning. Startup has just ensured the infra
// and recovered the tasks, and a reconcile that raced the worker's first claims
// would fight them for locks; the first tick is one interval away.
func (r *Reconciler) Start(ctx context.Context) error {
	ctx, r.cancel = context.WithCancel(ctx)

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.loop(ctx)
	}()

	if r.events != nil {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.watch(ctx)
		}()
	}

	r.logger.InfoContext(ctx, "reconciler started", "interval", r.interval,
		"events", r.events != nil)
	return nil
}

// Stop cancels the loop and waits for it, returning ctx.Err() if it is still
// going when ctx is done so a shutdown can proceed anyway.
func (r *Reconciler) Stop(ctx context.Context) error {
	r.once.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
	})

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		r.logger.InfoContext(context.WithoutCancel(ctx), "reconciler stopped")
		return nil
	case <-ctx.Done():
		return fmt.Errorf("worker: stop the reconciler: %w", ctx.Err())
	}
}

// loop runs a pass on every tick and on every hint, one at a time.
//
// A failing pass is logged and the loop keeps its schedule: libvirt being
// unreachable for a while is exactly the situation a reconciler exists for, and
// giving up would leave the database to drift until a restart.
func (r *Reconciler) loop(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.pass(ctx); err != nil && ctx.Err() == nil {
				r.logger.ErrorContext(ctx, "reconciler: the pass failed", "error", err)
			}
		case resourceID := <-r.hints:
			r.reconcileHinted(ctx, resourceID)
		}
	}
}

// reconcileHinted acts on one event. Without a Hint it falls back to a full
// pass, so wiring events without a per-resource path still shortens the wait.
func (r *Reconciler) reconcileHinted(ctx context.Context, resourceID string) {
	if r.hint == nil {
		if err := r.pass(ctx); err != nil && ctx.Err() == nil {
			r.logger.ErrorContext(ctx, "reconciler: the pass failed", "error", err)
		}
		return
	}

	if err := r.hint(ctx, resourceID); err != nil && ctx.Err() == nil {
		// A hint that fails is not worth a retry of its own: the resource is
		// covered by the next tick like everything else.
		r.logger.WarnContext(ctx, "reconciler: could not act on a lifecycle event",
			"resource_id", resourceID, "error", err)
	}
}

// watch forwards hypervisor lifecycle events to the loop, re-subscribing when
// the stream ends.
//
// A backend without events is reported once and the watcher stops: there is
// nothing to retry, and the ticker already covers what the stream would have
// told us. Any other failure to subscribe is retried, because a libvirt that is
// down now is expected to come back.
func (r *Reconciler) watch(ctx context.Context) {
	backoff := r.interval

	for ctx.Err() == nil {
		stream, err := r.events(ctx)
		switch {
		case errors.Is(err, hypervisor.ErrNotImplemented):
			r.logger.InfoContext(ctx,
				"reconciler: the hypervisor has no event stream, reconciling on the timer alone")
			return
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			r.logger.WarnContext(ctx, "reconciler: could not subscribe to lifecycle events",
				"error", err, "retry_in", backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			continue
		}

		r.drain(ctx, stream)
	}
}

// drain forwards one subscription's events until it closes. A hint that does not
// fit the buffer is dropped rather than blocking the stream, which would make a
// busy host stall the very events it is reporting.
func (r *Reconciler) drain(ctx context.Context, stream <-chan hypervisor.DomainEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-stream:
			if !ok {
				return
			}
			if event.ID == "" {
				continue
			}
			select {
			case r.hints <- event.ID:
			default:
				r.logger.DebugContext(ctx, "reconciler: dropped a lifecycle event hint",
					"resource_id", event.ID, "action", event.Action)
			}
		}
	}
}

// sleepCtx waits for d and reports whether the wait completed rather than ctx
// being canceled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
