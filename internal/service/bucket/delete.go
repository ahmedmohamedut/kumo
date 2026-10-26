package bucket

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// deletePayload is the input of a bucket.delete task.
type deletePayload struct {
	Name string `json:"name"`
}

// Delete removes an empty bucket at once. A bucket that still holds objects,
// old versions or delete markers is a conflict: the caller has to ask for a
// forced delete, which is ForceDelete.
func (s *Service) Delete(ctx context.Context, name string) error {
	current, err := s.get(ctx, name)
	if err != nil {
		return err
	}
	if current.State == StateDeleting {
		return fmt.Errorf("%w: bucket %q is already being deleted", service.ErrConflict, name)
	}

	if err := s.objects.DeleteBucket(ctx, name); err != nil {
		if errors.Is(err, objectstore.ErrBucketNotEmpty) {
			return fmt.Errorf("%w: bucket %q still holds objects or old versions; "+
				"delete them first, or delete the bucket with force", service.ErrConflict, name)
		}
		return mapErr(err)
	}
	s.forget(ctx, name, "deleted")
	s.logger.InfoContext(ctx, "bucket: deleted", "bucket", name)
	return nil
}

// ForceDelete marks the bucket deleting and queues a task that empties and
// deletes it, because emptying a large bucket takes longer than a request.
// Asking again while it is deleting queues another task, which is how a
// failed one is retried; the handler is idempotent.
func (s *Service) ForceDelete(ctx context.Context, name string) (Deleted, error) {
	if _, err := s.get(ctx, name); err != nil {
		return Deleted{}, err
	}

	var taskID string
	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		now := formatTime(s.now())
		if err := tx.EnsureBucketRow(ctx, sqlc.EnsureBucketRowParams{Name: name, CreatedAt: now}); err != nil {
			return fmt.Errorf("bucket: record %q: %w", name, err)
		}
		if err := tx.MarkBucketDeleting(ctx, sqlc.MarkBucketDeletingParams{Name: name, UpdatedAt: now}); err != nil {
			return fmt.Errorf("bucket: mark %q deleting: %w", name, err)
		}
		created, err := s.tasks.CreateInTx(ctx, tx, task.TypeBucketDelete, ResourceID(name), deletePayload{Name: name})
		if err != nil {
			return err
		}
		taskID = created.ID
		return nil
	})
	if err != nil {
		return Deleted{}, err
	}

	b, err := s.get(ctx, name)
	if err != nil {
		return Deleted{}, err
	}
	s.logger.InfoContext(ctx, "bucket: forced deletion queued", "bucket", name, "task_id", taskID)
	return Deleted{Bucket: b, TaskID: taskID}, nil
}

// DeleteHandler runs a bucket.delete task: it empties the bucket of every
// object, version and delete marker, deletes it, then drops the row.
//
// An upload that lands between the emptying and the delete makes the delete
// refuse; the task then fails, the bucket stays in deleting, and a second
// forced delete retries it.
func (s *Service) DeleteHandler(ctx context.Context, t worker.Task) error {
	var payload deletePayload
	if err := t.Decode(&payload); err != nil {
		return err
	}
	if payload.Name == "" {
		return fmt.Errorf("%w: task %s", ErrBadTaskPayload, t.ID)
	}
	name := payload.Name

	row, err := s.store.GetBucketRow(ctx, sqlc.GetBucketRowParams{Name: name})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A task that ran twice, or a row an earlier pass already dropped.
		s.logger.InfoContext(ctx, "bucket: delete skipped, the row is gone", "bucket", name, "task_id", t.ID)
		return nil
	case err != nil:
		return fmt.Errorf("bucket: read the row of %q: %w", name, err)
	case State(row.State) != StateDeleting:
		return fmt.Errorf("%w: bucket %q is %s, not deleting", service.ErrConflict, name, row.State)
	}

	removed, err := s.objects.EmptyBucket(ctx, name)
	if err != nil && !errors.Is(err, objectstore.ErrNotFound) {
		return fmt.Errorf("bucket: empty %q after %d entries: %w", name, removed, mapErr(err))
	}
	if err := s.objects.DeleteBucket(ctx, name); err != nil && !errors.Is(err, objectstore.ErrNotFound) {
		return fmt.Errorf("bucket: delete %q: %w", name, mapErr(err))
	}

	if err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		return forgetInTx(ctx, tx, name)
	}); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "bucket: deleted with its contents",
		"bucket", name, "entries", removed, "task_id", t.ID)
	return nil
}

// Handler returns the worker handlers of the bucket workflows.
func (s *Service) Handler() map[string]worker.Handler {
	return map[string]worker.Handler{
		task.TypeBucketDelete: worker.HandlerFunc(s.DeleteHandler),
	}
}
