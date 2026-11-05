package image

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// DeleteHandler removes the volume of an image and then its row.
//
// The volume goes first: if the row were deleted first and the volume removal
// failed, the bytes would be stranded in the pool with nothing pointing at them.
// A stranded volume is recoverable, a missing row is not.
func (s *Service) DeleteHandler(ctx context.Context, t worker.Task) error {
	var payload deletePayload
	if err := t.Decode(&payload); err != nil {
		return err
	}
	if payload.ImageID == "" {
		return fmt.Errorf("%w: delete task %s names no image", ErrBadTaskPayload, t.ID)
	}

	row, err := s.store.GetImage(ctx, sqlc.GetImageParams{ID: payload.ImageID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Already gone: the volume was removed with it, or the row never
			// had one. Either way there is nothing left to do.
			s.logger.InfoContext(ctx, "image: delete skipped, the image is gone",
				"image_id", payload.ImageID, "task_id", t.ID)
			return nil
		}
		return fmt.Errorf("get image %q: %w", payload.ImageID, err)
	}

	// An instance started after the delete was queued but before the task ran
	// would boot from a volume that is about to disappear, so the check is
	// repeated here and not only in Delete.
	inUse, err := s.store.ListInstancesUsingImage(ctx,
		sqlc.ListInstancesUsingImageParams{ImageID: row.ID})
	if err != nil {
		return fmt.Errorf("list the instances using image %q: %w", row.ID, err)
	}
	if len(inUse) > 0 {
		return fmt.Errorf("%w: %d instance(s) reference image %q while its deletion was queued: %s",
			service.ErrConflict, len(inUse), row.ID, formatInstanceRefs(inUse))
	}

	// The row records the volume the import created, which is what a delete
	// has to remove: deriving it again would miss an image imported before
	// the name carried the format's extension (volumeName).
	name := row.VolumeName.String
	if name == "" {
		name = volumeName(row.ID, row.Format)
	}
	if err := s.hv.DeleteVolume(ctx, s.imagesPool, name); err != nil &&
		!errors.Is(err, hypervisor.ErrNotFound) {
		return fmt.Errorf("delete the volume of image %q: %w", row.ID, err)
	}

	// The tags cascade with the row.
	if _, err := s.store.DeleteImage(ctx, sqlc.DeleteImageParams{ID: row.ID}); err != nil {
		return fmt.Errorf("delete image %q: %w", row.ID, err)
	}

	s.logger.InfoContext(ctx, "image: deleted", "image_id", row.ID, "task_id", t.ID)
	return nil
}

// ResetFailed moves a failed image back to pending and queues a fresh import, so
// a transient CDN failure can be retried from the console without re-entering the
// URL. The row keeps its source_url and checksum.
func (s *Service) ResetFailed(ctx context.Context, imageID string) (Imported, error) {
	var imported Imported

	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		// The reset query is unconditional, so the state is checked first: an
		// available image must not be queued for another download.
		current, err := tx.GetImage(ctx, sqlc.GetImageParams{ID: imageID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no image %q", service.ErrNotFound, imageID)
			}
			return fmt.Errorf("get image %q: %w", imageID, err)
		}
		if State(current.State) != StateFailed {
			return fmt.Errorf("%w: image %q is %s, only a failed import can be retried",
				service.ErrConflict, imageID, current.State)
		}

		row, err := tx.ResetImageToPending(ctx, sqlc.ResetImageToPendingParams{
			ID:        imageID,
			UpdatedAt: formatTime(s.now()),
		})
		if err != nil {
			return fmt.Errorf("reset image %q: %w", imageID, err)
		}

		created, err := s.tasks.CreateInTx(ctx, tx, task.TypeImageImport, row.ID,
			importPayload{ImageID: row.ID})
		if err != nil {
			return err
		}
		imported.TaskID = created.ID
		return nil
	})
	if err != nil {
		return Imported{}, err
	}

	image, err := s.Get(ctx, imageID)
	if err != nil {
		return Imported{}, err
	}
	imported.Image = image
	return imported, nil
}

// formatInstanceRefs names the instances that block a deletion, running ones
// first, so the 409 tells the admin what to terminate instead of just counting.
func formatInstanceRefs(refs []sqlc.ListInstancesUsingImageRow) string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.State == "terminated" {
			names = append(names, ref.ID+" (terminated)")
			continue
		}
		names = append(names, ref.ID+" ("+ref.State+")")
	}
	return strings.Join(names, ", ")
}
