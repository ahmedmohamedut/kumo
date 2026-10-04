// Package worker runs the long operations of the control plane. Handlers are
// registered per task type; the pool claims pending rows from the database,
// runs the handler and records the outcome. The rules come from
// docs/design/00-architecture.md ("long operations are async") and
// docs/design/02-data-model.md (the tasks table).
//
// A crash mid-task leaves a row in 'running' with no worker behind it, so Start
// hands those rows back to the queue. Every workflow must therefore be safe to
// run twice.
package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Defaults for the pool.
const (
	defaultPollInterval = 250 * time.Millisecond
	defaultWorkers      = 2

	// DefaultMaxAttempts caps how often a task may be claimed. A handler that
	// kills its worker (a panic that takes the process with it, an OOM kill)
	// would otherwise be requeued on every restart and block the queue behind
	// itself forever.
	DefaultMaxAttempts = 3

	// progressFloor is the smallest change in percent worth a database write.
	// A download reports progress per chunk, which is far more often than a
	// console polls.
	progressFloor = 2
)

// ErrNoHandler means a task reached the pool without a registered workflow.
var ErrNoHandler = errors.New("worker: no handler is registered for this task type")

// ErrTooManyAttempts means a task has been claimed DefaultMaxAttempts times.
var ErrTooManyAttempts = errors.New("worker: too many attempts")

// ErrEmptyPayload means a task row carries no JSON, which a creator in this
// codebase cannot produce; it marks a bug rather than a runtime condition.
var ErrEmptyPayload = errors.New("worker: the task has an empty payload")

// Task is one claimed unit of work. The payload is the JSON the creator stored
// on the row; a workflow reads its inputs from the database, so the payload
// only has to name what it works on.
type Task struct {
	ID         string
	Type       string
	ResourceID string
	Attempts   int64
	Payload    json.RawMessage
}

// Decode unmarshals the payload into v.
func (t Task) Decode(v any) error {
	if len(t.Payload) == 0 {
		return fmt.Errorf("%w: task %s", ErrEmptyPayload, t.ID)
	}
	if err := json.Unmarshal(t.Payload, v); err != nil {
		return fmt.Errorf("task %s: decode the payload: %w", t.ID, err)
	}
	return nil
}

// Handler runs one task type. Returning an error marks the task failed with
// that message; the workflow is responsible for having cleaned up after itself.
type Handler interface {
	Handle(ctx context.Context, task Task) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, task Task) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, task Task) error { return f(ctx, task) }

// Registry maps a task type to its handler.
type Registry map[string]Handler

// Options configures the pool.
type Options struct {
	// Logger records every claim, completion and failure.
	Logger *slog.Logger
	// Store is the metadata database holding the tasks table.
	Store *store.Store
	// Registry maps task types to handlers.
	Registry Registry
	// Workers is the number of concurrent workers. Zero means 2.
	Workers int
	// PollInterval is how long a worker waits after an empty queue. Zero means
	// 250ms.
	PollInterval time.Duration
	// MaxAttempts caps claims per task. Zero means DefaultMaxAttempts.
	MaxAttempts int
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Pool is a set of workers draining the tasks table.
type Pool struct {
	logger      *slog.Logger
	store       *store.Store
	registry    Registry
	workers     int
	poll        time.Duration
	maxAttempts int
	now         func() time.Time

	// writeMu serializes progress writes so a burst from several workers does
	// not pile up on the single SQLite connection.
	writeMu sync.Mutex
	wg      sync.WaitGroup

	cancel context.CancelFunc
	once   sync.Once
}

// New builds a pool. Start brings it up.
func New(opts Options) *Pool {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Workers <= 0 {
		opts.Workers = defaultWorkers
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Pool{
		logger:      opts.Logger,
		store:       opts.Store,
		registry:    opts.Registry,
		workers:     opts.Workers,
		poll:        opts.PollInterval,
		maxAttempts: opts.MaxAttempts,
		now:         opts.Now,
	}
}

// Start requeues tasks abandoned by a previous run and then starts the workers.
// It returns once they are running; work happens in the background.
func (p *Pool) Start(ctx context.Context) error {
	if err := p.Recover(ctx); err != nil {
		return err
	}

	ctx, p.cancel = context.WithCancel(ctx)

	p.wg.Add(p.workers)
	for i := 0; i < p.workers; i++ {
		go func(n int) {
			defer p.wg.Done()
			p.loop(ctx, n)
		}(i)
	}

	p.logger.InfoContext(ctx, "worker pool started",
		"workers", p.workers, "handlers", len(p.registry), "poll_interval", p.poll)

	return nil
}

// Recover hands tasks left 'running' by a previous run back to the queue. It is
// called by Start and is exported so a test can assert the recovery on its own.
func (p *Pool) Recover(ctx context.Context) error {
	recovered, err := p.store.RequeueRunningTasks(ctx,
		sqlc.RequeueRunningTasksParams{UpdatedAt: p.stamp()})
	if err != nil {
		return fmt.Errorf("worker: requeue tasks abandoned by a previous run: %w", err)
	}

	for _, task := range recovered {
		p.logger.WarnContext(ctx, "worker: requeued a task that was still running",
			"task_id", task.ID, "task_type", task.Type, "resource_id", task.ResourceID.String,
			"attempts", task.Attempts)
	}

	return nil
}

// Stop cancels the workers and waits for them. It returns ctx.Err() if they
// are still busy when ctx is done, so a shutdown can proceed anyway.
func (p *Pool) Stop(ctx context.Context) error {
	p.once.Do(func() {
		if p.cancel != nil {
			p.cancel()
		}
	})

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.logger.InfoContext(context.WithoutCancel(ctx), "worker pool stopped")
		return nil
	case <-ctx.Done():
		return fmt.Errorf("worker: stop: %w", ctx.Err())
	}
}

// loop claims and runs tasks until ctx is canceled. It claims before waiting,
// so a queue that already holds work does not sit idle for a poll interval.
func (p *Pool) loop(ctx context.Context, workerID int) {
	for {
		ran, err := p.claimAndRun(ctx, workerID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.logger.ErrorContext(ctx, "worker: claim failed", "worker", workerID, "error", err)
		}

		if ran {
			// Go straight back for more work.
			continue
		}

		timer := time.NewTimer(p.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// claimAndRun takes the oldest pending task and runs it. It reports whether a
// task was claimed; an empty queue is not an error.
func (p *Pool) claimAndRun(ctx context.Context, workerID int) (bool, error) {
	task, err := p.store.ClaimPendingTask(ctx, sqlc.ClaimPendingTaskParams{UpdatedAt: p.stamp()})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("claim a pending task: %w", err)
	}

	if task.Attempts > int64(p.maxAttempts) {
		// Recovery keeps requeueing a task whose worker dies mid-flight; give
		// up so it cannot hold the queue hostage.
		p.logger.ErrorContext(ctx, "worker: giving up on a task",
			"task_id", task.ID, "task_type", task.Type,
			"attempts", task.Attempts, "max_attempts", p.maxAttempts)
		p.finish(ctx, task, fmt.Errorf("%w: claimed %d times", ErrTooManyAttempts, task.Attempts))
		return true, nil
	}

	handler, ok := p.registry[task.Type]
	if !ok {
		// A typo in a task type would otherwise be retried on every restart.
		p.logger.ErrorContext(ctx, "worker: no handler for this task type",
			"task_id", task.ID, "task_type", task.Type)
		p.finish(ctx, task, fmt.Errorf("%w: %q", ErrNoHandler, task.Type))
		return true, nil
	}

	p.logger.InfoContext(ctx, "worker: task started",
		"task_id", task.ID, "task_type", task.Type,
		"resource_id", task.ResourceID.String, "worker", workerID, "attempts", task.Attempts)

	if err := handler.Handle(ctx, Task{
		ID:         task.ID,
		Type:       task.Type,
		ResourceID: task.ResourceID.String,
		Attempts:   task.Attempts,
		Payload:    json.RawMessage(task.Payload),
	}); err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			// The process is shutting down. The task stays 'running' and the
			// next Start requeues it; failing it here would report a shutdown
			// as a failure the user has to act on.
			p.logger.WarnContext(ctx, "worker: task interrupted by shutdown",
				"task_id", task.ID, "task_type", task.Type)
			return true, nil
		}

		p.logger.ErrorContext(ctx, "worker: task failed",
			"task_id", task.ID, "task_type", task.Type, "resource_id", task.ResourceID.String, "error", err)
		p.finish(ctx, task, err)
		return true, nil
	}

	p.finish(ctx, task, nil)
	return true, nil
}

// finish records the outcome of a task. It is deliberately forgiving: a failure
// to write the outcome must not kill the worker, because the task is then left
// 'running' and the next restart recovers it.
func (p *Pool) finish(ctx context.Context, task sqlc.Task, cause error) {
	params := sqlc.UpdateTaskParams{
		ID:        task.ID,
		State:     "succeeded",
		Progress:  100,
		Attempts:  task.Attempts,
		UpdatedAt: p.stamp(),
	}
	if cause != nil {
		params.State = "failed"
		params.Progress = task.Progress
		params.Error = sql.NullString{String: cause.Error(), Valid: true}
	}

	ctx = context.WithoutCancel(ctx) // the outcome must survive a shutdown
	if _, err := p.store.UpdateTask(ctx, params); err != nil {
		p.logger.ErrorContext(ctx, "worker: could not record the task outcome",
			"task_id", task.ID, "task_type", task.Type, "error", err)
	}
}

// ReportProgress persists the progress of a running task. Writes below
// progressFloor percent of change are dropped, so a handler may call it on
// every chunk it reads without hammering the database.
func (p *Pool) ReportProgress(ctx context.Context, taskID string, percent int, last *int) error {
	percent = clampPercent(percent)

	if last != nil && *last >= 0 && percent-*last < progressFloor && percent < 100 {
		return nil
	}

	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	if _, err := p.store.UpdateTaskProgress(ctx, sqlc.UpdateTaskProgressParams{
		ID:        taskID,
		Progress:  int64(percent),
		UpdatedAt: p.stamp(),
	}); err != nil {
		return fmt.Errorf("worker: report progress for task %s: %w", taskID, err)
	}
	if last != nil {
		*last = percent
	}

	return nil
}

// NewTaskID returns an id for a task row, so services can name a task they are
// about to enqueue.
func NewTaskID() string { return id.Resource(id.PrefixTask) }

// stamp is the current time in the format the schema stores.
func (p *Pool) stamp() string { return p.now().UTC().Format(time.RFC3339Nano) }

// clampPercent keeps a progress value inside the 0-100 the schema allows.
func clampPercent(percent int) int {
	switch {
	case percent < 0:
		return 0
	case percent > 100:
		return 100
	default:
		return percent
	}
}
