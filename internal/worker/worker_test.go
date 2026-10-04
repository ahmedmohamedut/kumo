package worker_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// The pool is asynchronous, so tests wait for the expected state instead of
// sleeping for a fixed time.
const (
	waitFor = 10 * time.Second
	poll    = 2 * time.Millisecond
)

// errDownloadFailed is what a handler returns to simulate a dead download.
var errDownloadFailed = errors.New("the download failed: connection reset")

func TestRunsATaskAndMarksItSucceeded(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	taskID := enqueue(t, db, "image.import", "img-00000000000000000", 0)

	started := make(chan worker.Task, 1)
	start(context.Background(), t, newPool(db, worker.Options{
		Registry: worker.Registry{
			"image.import": worker.HandlerFunc(func(_ context.Context, task worker.Task) error {
				started <- task
				return nil
			}),
		},
	}))

	select {
	case task := <-started:
		if task.Type != "image.import" {
			t.Errorf("type = %q, want image.import", task.Type)
		}
		if task.ResourceID != "img-00000000000000000" {
			t.Errorf("resource_id = %q, want the image id", task.ResourceID)
		}
		if task.Attempts != 1 {
			t.Errorf("attempts = %d, want 1", task.Attempts)
		}
	case <-time.After(waitFor):
		t.Fatal("the handler never ran")
	}

	row := waitForState(t, db, taskID, "succeeded")
	if row.Progress != 100 {
		t.Errorf("progress = %d, want 100", row.Progress)
	}
	if row.Error.Valid {
		t.Errorf("error = %q, want none", row.Error.String)
	}
}

func TestAFailingTaskIsRecordedWithItsReason(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	taskID := enqueue(t, db, "image.import", "img-00000000000000000", 0)

	start(context.Background(), t, newPool(db, worker.Options{
		Registry: worker.Registry{
			"image.import": worker.HandlerFunc(func(context.Context, worker.Task) error {
				return errDownloadFailed
			}),
		},
	}))

	row := waitForState(t, db, taskID, "failed")
	if row.Error.String != "the download failed: connection reset" {
		t.Errorf("error = %q, want the handler's message", row.Error.String)
	}
	if row.Progress != 0 {
		t.Errorf("progress = %d, want it untouched at 0", row.Progress)
	}
}

func TestATaskWithoutAHandlerFailsInsteadOfSpinning(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	taskID := enqueue(t, db, "image.import", "img-00000000000000000", 0)

	// The pool has a handler, just not for this type: a typo in a task type
	// must not be retried on every restart.
	start(context.Background(), t, newPool(db, worker.Options{
		Registry: worker.Registry{
			"image.delete": worker.HandlerFunc(func(context.Context, worker.Task) error { return nil }),
		},
	}))

	row := waitForState(t, db, taskID, "failed")
	if row.Error.String == "" {
		t.Fatal("error is empty, want a reason")
	}
}

// Restart recovery: a task left 'running' belongs to no worker, so Start must
// hand it back to the queue and it must run again.
func TestRecoverRequeuesTasksLeftRunningByAPreviousRun(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	taskID := enqueue(t, db, "image.import", "img-00000000000000000", 0)

	// Pretend a worker claimed it and the process was killed.
	claim(t, db)
	if state(t, db, taskID) != "running" {
		t.Fatalf("state = %q, want running before the restart", state(t, db, taskID))
	}

	var runs atomic.Int32
	start(context.Background(), t, newPool(db, worker.Options{
		Registry: worker.Registry{
			"image.import": worker.HandlerFunc(func(context.Context, worker.Task) error {
				runs.Add(1)
				return nil
			}),
		},
	}))

	row := waitForState(t, db, taskID, "succeeded")
	if row.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (the claim before the crash counts)", row.Attempts)
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("the handler ran %d times, want 1", got)
	}
}

func TestRecoverAloneIsEnoughToRequeue(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	taskID := enqueue(t, db, "image.import", "img-00000000000000000", 0)
	claim(t, db)

	pool := newPool(db, worker.Options{})
	if err := pool.Recover(context.Background()); err != nil {
		t.Fatalf("Recover(): %v", err)
	}

	row := waitForState(t, db, taskID, "pending")
	if row.Progress != 0 {
		t.Errorf("progress = %d, want it reset for the new attempt", row.Progress)
	}
}

// A task whose handler keeps killing the worker must not be requeued forever.
func TestATaskIsGivenUpOnAfterTooManyAttempts(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	taskID := enqueue(t, db, "image.import", "img-00000000000000000", 0)

	// Sit at the cap: the pool claims, sees the count and refuses to run.
	for i := 0; i < worker.DefaultMaxAttempts; i++ {
		claim(t, db)
		if _, err := db.RequeueRunningTasks(context.Background(),
			sqlc.RequeueRunningTasksParams{UpdatedAt: stamp()}); err != nil {
			t.Fatalf("RequeueRunningTasks() %d: %v", i, err)
		}
	}

	start(context.Background(), t, newPool(db, worker.Options{
		Registry: worker.Registry{
			"image.import": worker.HandlerFunc(func(context.Context, worker.Task) error {
				t.Error("the handler ran even though the task was over the attempt cap")
				return nil
			}),
		},
	}))

	if row := waitForState(t, db, taskID, "failed"); row.Error.String == "" {
		t.Error("error is empty, want the give-up reason")
	}
}

// Two workers must never run the same task; the guarded single UPDATE that
// claims a task is what guarantees it.
func TestTwoWorkersDoNotRunTheSameTask(t *testing.T) {
	t.Parallel()

	const total = 8
	db := storetest.New(t)
	for i := 0; i < total; i++ {
		enqueue(t, db, "image.import", fmt.Sprintf("img-%017d", i), 0)
	}

	var (
		mu    sync.Mutex
		seen  = map[string]int{}
		done  = make(chan struct{})
		count int
	)

	start(context.Background(), t, newPool(db, worker.Options{
		Workers: 4,
		Registry: worker.Registry{
			"image.import": worker.HandlerFunc(func(_ context.Context, task worker.Task) error {
				mu.Lock()
				defer mu.Unlock()
				seen[task.ID]++
				count++
				if count == total {
					close(done)
				}
				return nil
			}),
		},
	}))

	select {
	case <-done:
	case <-time.After(waitFor):
		t.Fatalf("only %d of %d tasks ran", count, total)
	}

	// Nothing may start a task again once it is done.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for taskID, times := range seen {
		if times != 1 {
			t.Errorf("task %s ran %d times, want exactly 1", taskID, times)
		}
	}
}

func TestProgressIsRateLimitedButAlwaysReachesTheEnds(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	taskID := enqueue(t, db, "image.import", "img-00000000000000000", 0)
	claim(t, db) // the test owns the task, so the workers must not

	pool := newPool(db, worker.Options{})

	ctx := context.Background()
	last := -1
	writes := 0
	for _, percent := range []int{0, 1, 2, 3, 5, 10, 40, 78, 99, 100, 250} {
		before := progress(t, db, taskID)
		if err := pool.ReportProgress(ctx, taskID, percent, &last); err != nil {
			t.Fatalf("ReportProgress(%d): %v", percent, err)
		}
		if progress(t, db, taskID) != before {
			writes++
		}
	}

	if writes < 3 {
		t.Errorf("%d writes for 11 reports, want the small steps dropped", writes)
	}
	if got := progress(t, db, taskID); got != 100 {
		t.Errorf("final progress = %d, want 100 (the end of the scale is always written)", got)
	}
}

// A shutdown must not report an interrupted task as failed: it stays 'running'
// and the next start recovers it.
func TestStopLeavesAnInterruptedTaskRunning(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	taskID := enqueue(t, db, "image.import", "img-00000000000000000", 0)

	runCtx, cancel := context.WithCancel(context.Background())
	pool := start(runCtx, t, newPool(db, worker.Options{
		Registry: worker.Registry{
			"image.import": worker.HandlerFunc(func(ctx context.Context, _ worker.Task) error {
				<-ctx.Done()
				return ctx.Err()
			}),
		},
	}))

	waitForState(t, db, taskID, "running")
	stop(t, pool)
	cancel()

	if got := state(t, db, taskID); got != "running" {
		t.Errorf("state = %q, want running so the next start recovers it", got)
	}
}

func TestStopReturnsWhenTheDeadlinePasses(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	blocked := make(chan struct{})

	pool := start(context.Background(), t, newPool(db, worker.Options{
		Registry: worker.Registry{
			// A handler stuck in an operation that ignores its context, which is
			// exactly what the deadline in Stop exists for.
			"image.import": worker.HandlerFunc(func(context.Context, worker.Task) error {
				<-blocked
				return nil
			}),
		},
	}))

	// Registered after start, so it runs first and the cleanup Stop cannot hang.
	t.Cleanup(func() { close(blocked) })

	enqueue(t, db, "image.import", "img-00000000000000000", 0)
	waitForState(t, db, taskIDOnly(t, db), "running")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := pool.Stop(ctx); err == nil {
		t.Error("Stop() error = nil, want the deadline error while a task is still running")
	}
}

func TestTaskDecode(t *testing.T) {
	t.Parallel()

	t.Run("empty payload", func(t *testing.T) {
		t.Parallel()

		var payload struct{}
		if err := (worker.Task{ID: "task-1"}).Decode(&payload); !errors.Is(err, worker.ErrEmptyPayload) {
			t.Errorf("Decode() error = %v, want ErrEmptyPayload", err)
		}
	})

	t.Run("malformed payload", func(t *testing.T) {
		t.Parallel()

		var payload struct{}
		if err := (worker.Task{ID: "task-1", Payload: []byte("{oops")}).Decode(&payload); err == nil {
			t.Fatal("Decode() error = nil, want a parse error")
		}
	})

	t.Run("valid payload", func(t *testing.T) {
		t.Parallel()

		var payload struct {
			ImageID string `json:"image_id"`
		}
		if err := (worker.Task{ID: "task-1", Payload: []byte(`{"image_id":"img-1"}`)}).Decode(&payload); err != nil {
			t.Fatalf("Decode(): %v", err)
		}
		if payload.ImageID != "img-1" {
			t.Errorf("image_id = %q, want img-1", payload.ImageID)
		}
	})
}

// newPool builds a pool tuned for tests: many short polls.
func newPool(db *store.Store, opts worker.Options) *worker.Pool {
	opts.Logger = logging.Discard()
	opts.Store = db
	if opts.Workers == 0 {
		opts.Workers = 2
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = time.Millisecond
	}
	return worker.New(opts)
}

// start brings a pool up and stops it when the test ends.
func start(ctx context.Context, t *testing.T, pool *worker.Pool) *worker.Pool {
	t.Helper()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	return pool
}

func stop(t *testing.T, pool *worker.Pool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	if err := pool.Stop(ctx); err != nil {
		t.Errorf("Stop(): %v", err)
	}
}

// enqueue inserts a pending task and returns its id.
func enqueue(t *testing.T, db *store.Store, taskType, resourceID string, progress int64) string {
	t.Helper()

	taskID := id.Resource(id.PrefixTask)
	if _, err := db.CreateTask(context.Background(), sqlc.CreateTaskParams{
		ID:         taskID,
		Type:       taskType,
		ResourceID: sql.NullString{String: resourceID, Valid: resourceID != ""},
		State:      "pending",
		Progress:   progress,
		Payload:    `{}`,
		Attempts:   0,
		CreatedAt:  stamp(),
		UpdatedAt:  stamp(),
	}); err != nil {
		t.Fatalf("CreateTask(%s): %v", taskType, err)
	}
	return taskID
}

// claim takes the oldest pending task, as a worker would.
func claim(t *testing.T, db *store.Store) sqlc.Task {
	t.Helper()

	task, err := db.ClaimPendingTask(context.Background(),
		sqlc.ClaimPendingTaskParams{UpdatedAt: stamp()})
	if err != nil {
		t.Fatalf("ClaimPendingTask(): %v", err)
	}
	return task
}

// taskIDOnly returns the id of the newest task, which is the only one in these
// single-task tests.
func taskIDOnly(t *testing.T, db *store.Store) string {
	t.Helper()

	tasks, err := db.ListTasksDesc(context.Background(), sqlc.ListTasksDescParams{
		CursorCreated: "",
		CursorID:      "",
		PageLimit:     1,
	})
	if err != nil {
		t.Fatalf("ListTasksDesc(): %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("no task in the database")
	}
	return tasks[0].ID
}

func state(t *testing.T, db *store.Store, taskID string) string {
	t.Helper()

	row, err := db.GetTask(context.Background(), sqlc.GetTaskParams{ID: taskID})
	if err != nil {
		t.Fatalf("GetTask(%s): %v", taskID, err)
	}
	return row.State
}

func progress(t *testing.T, db *store.Store, taskID string) int64 {
	t.Helper()

	row, err := db.GetTask(context.Background(), sqlc.GetTaskParams{ID: taskID})
	if err != nil {
		t.Fatalf("GetTask(%s): %v", taskID, err)
	}
	return row.Progress
}

// waitForState polls until the task reaches the wanted state.
func waitForState(t *testing.T, db *store.Store, taskID, want string) sqlc.Task {
	t.Helper()

	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		row, err := db.GetTask(context.Background(), sqlc.GetTaskParams{ID: taskID})
		if err != nil {
			t.Fatalf("GetTask(%s): %v", taskID, err)
		}
		if row.State == want {
			return row
		}
		time.Sleep(poll)
	}

	t.Fatalf("task %s is %q after %s, want %q", taskID, state(t, db, taskID), waitFor, want)
	return sqlc.Task{}
}

func stamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
