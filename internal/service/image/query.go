package image

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// SortOrder selects the order of an image page.
type SortOrder string

// The supported sort orders.
const (
	SortCreatedDesc SortOrder = "created_at:desc"
	SortCreatedAsc  SortOrder = "created_at:asc"
)

// Pagination bounds, the same as every other list endpoint.
const (
	// MinPageSize is the smallest page a client may ask for.
	MinPageSize = 1
	// DefaultPageSize is what a client that does not ask gets.
	DefaultPageSize = 50
	// MaxPageSize keeps one response inside what the console can render.
	MaxPageSize = 200
)

// Get returns one image with its tags.
func (s *Service) Get(ctx context.Context, imageID string) (Image, error) {
	row, err := s.store.GetImage(ctx, sqlc.GetImageParams{ID: imageID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Image{}, fmt.Errorf("%w: no image %q", service.ErrNotFound, imageID)
		}
		return Image{}, fmt.Errorf("get image: %w", err)
	}

	tags, err := s.tagsByResource(ctx, []string{row.ID})
	if err != nil {
		return Image{}, err
	}
	return imageFromRow(row, tags[row.ID]), nil
}

// List returns one page of images, newest first by default.
func (s *Service) List(ctx context.Context, in ListInput) (Page, error) {
	if in.Sort == "" {
		in.Sort = SortCreatedDesc
	}
	switch in.Sort {
	case SortCreatedAsc, SortCreatedDesc:
	default:
		return Page{}, &service.ValidationError{
			Message: "the sort order is not acceptable",
			Fields: []service.FieldError{{
				Field:  "sort",
				Reason: `must be "created_at:asc" or "created_at:desc"`,
			}},
		}
	}

	if in.State != "" {
		switch in.State {
		case StatePending, StateDownloading, StateAvailable, StateFailed, StateDeleting:
		default:
			return Page{}, &service.ValidationError{
				Message: "the state filter is not acceptable",
				Fields: []service.FieldError{{
					Field:  "state",
					Reason: "must be pending, downloading, available, failed or deleting",
				}},
			}
		}
	}

	if in.PageSize == 0 {
		in.PageSize = DefaultPageSize
	}
	if in.PageSize < MinPageSize || in.PageSize > MaxPageSize {
		return Page{}, &service.ValidationError{
			Message: "the page size is not acceptable",
			Fields: []service.FieldError{{
				Field:  "page_size",
				Reason: fmt.Sprintf("must be between %d and %d", MinPageSize, MaxPageSize),
			}},
		}
	}

	cursor, err := decodePageToken(in.PageToken)
	if err != nil {
		return Page{}, err
	}

	rows, err := s.listPage(ctx, in, cursor)
	if err != nil {
		return Page{}, fmt.Errorf("list images: %w", err)
	}

	page := Page{Items: make([]Image, 0, min(len(rows), in.PageSize))}
	if len(rows) > in.PageSize {
		last := rows[in.PageSize-1]
		token := encodePageToken(last.CreatedAt, last.ID)
		page.NextPageToken = &token
		rows = rows[:in.PageSize]
	}

	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	tags, err := s.tagsByResource(ctx, ids)
	if err != nil {
		return Page{}, err
	}
	for _, row := range rows {
		page.Items = append(page.Items, imageFromRow(row, tags[row.ID]))
	}

	return page, nil
}

// Update applies a PATCH body. The import columns are deliberately not
// reachable: renaming an image must not rewrite where it came from.
func (s *Service) Update(ctx context.Context, imageID string, in UpdateInput) (Image, error) {
	if fields := validateUpdate(in); len(fields) > 0 {
		return Image{}, &service.ValidationError{Message: "the image is not acceptable", Fields: fields}
	}

	var updated Image
	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		current, err := tx.GetImage(ctx, sqlc.GetImageParams{ID: imageID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no image %q", service.ErrNotFound, imageID)
			}
			return fmt.Errorf("get image: %w", err)
		}

		name := current.Name
		if in.Name != nil {
			name = *in.Name
		}
		description := current.Description
		if in.Description != nil {
			description = *in.Description
		}

		row, err := tx.UpdateImageMetadata(ctx, sqlc.UpdateImageMetadataParams{
			ID:          imageID,
			Name:        name,
			Description: description,
			UpdatedAt:   formatTime(s.now()),
		})
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: an image named %q already exists", service.ErrAlreadyExists, name)
			}
			return fmt.Errorf("update image %q: %w", imageID, err)
		}

		tags := in.Tags
		if in.Tags != nil {
			if err := replaceTags(ctx, tx, imageID, in.Tags); err != nil {
				return err
			}
		} else {
			tags = nil
		}

		updated = imageFromRow(row, tags)
		return nil
	})
	if err != nil {
		return Image{}, err
	}
	return updated, nil
}

// Delete marks an image for removal and queues the volume deletion. It refuses
// while an instance is still using the image, because the volume is the running
// guest's backing file (docs/design/01-api.md §Images).
func (s *Service) Delete(ctx context.Context, imageID string) (Deleted, error) {
	var deleted Deleted

	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		// A running instance still boots from the volume, and a terminated one
		// keeps the foreign key, so both block the deletion (see
		// CountInstancesUsingImage).
		refs, err := tx.ListInstancesUsingImage(ctx,
			sqlc.ListInstancesUsingImageParams{ImageID: imageID})
		if err != nil {
			return fmt.Errorf("list the instances using image %q: %w", imageID, err)
		}
		if len(refs) > 0 {
			// A domain sentinel keeps the reason in the message, which the api
			// layer renders as a 409 (key pairs do the same for a name in use).
			return fmt.Errorf("%w: %d instance(s) reference image %q; terminate them first or keep the image: %s",
				service.ErrConflict, len(refs), imageID, formatInstanceRefs(refs))
		}

		row, err := tx.MarkImageDeleting(ctx, sqlc.MarkImageDeletingParams{
			ID:        imageID,
			UpdatedAt: formatTime(s.now()),
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no image %q", service.ErrNotFound, imageID)
			}
			return fmt.Errorf("mark image %q for deletion: %w", imageID, err)
		}

		created, err := s.tasks.CreateInTx(ctx, tx, task.TypeImageDelete, row.ID,
			deletePayload{ImageID: row.ID})
		if err != nil {
			return err
		}

		deleted = Deleted{TaskID: created.ID}
		return nil
	})
	if err != nil {
		return Deleted{}, err
	}

	// Read the row back with its tags so the response body matches GET /images.
	image, err := s.Get(ctx, imageID)
	if err != nil {
		return Deleted{}, err
	}
	deleted.Image = image

	s.logger.InfoContext(ctx, "image: deletion queued",
		"image_id", imageID, "task_id", deleted.TaskID)

	return deleted, nil
}

// Catalog returns the curated list of cloud images the console can offer. It is
// static on purpose: Kumo does not mirror or re-publish distribution metadata,
// and a catalog that depends on the network would be a second source of truth
// for the same information (docs/design/01-api.md §Images).
func (s *Service) Catalog(context.Context) []CatalogImage {
	return catalogImages
}

// listPage runs the asc or desc variant of the listing query and returns at most
// PageSize+1 rows; the extra row is how the caller learns there is a next page.
func (s *Service) listPage(
	ctx context.Context, in ListInput, cursor pageCursor,
) ([]sqlc.Image, error) {
	// Both filters use the empty string as "no filter", which keeps the SQL a
	// single prepared statement for every page.
	stateFilter := string(in.State)

	if in.Sort == SortCreatedAsc {
		return s.store.ListImagesAsc(ctx, sqlc.ListImagesAscParams{
			Query:         in.Query,
			StateFilter:   stateFilter,
			CursorCreated: cursor.created,
			CursorID:      cursor.id,
			PageLimit:     int64(in.PageSize) + 1,
		})
	}
	return s.store.ListImagesDesc(ctx, sqlc.ListImagesDescParams{
		Query:         in.Query,
		StateFilter:   stateFilter,
		CursorCreated: cursor.created,
		CursorID:      cursor.id,
		PageLimit:     int64(in.PageSize) + 1,
	})
}

// pageCursor is the position a page token encodes.
type pageCursor struct {
	created string
	id      string
}

// encodePageToken packs the cursor of a row into an opaque token.
func encodePageToken(createdAt, imageID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt + "|" + imageID))
}

// decodePageToken unpacks a token; an empty token means the first page.
func decodePageToken(token string) (pageCursor, error) {
	if token == "" {
		return pageCursor{}, nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return pageCursor{}, invalidPageToken()
	}
	created, imageID, ok := strings.Cut(string(raw), "|")
	if !ok || created == "" || !id.HasResourceShape(id.PrefixImage, imageID) {
		return pageCursor{}, invalidPageToken()
	}
	return pageCursor{created: created, id: imageID}, nil
}

func invalidPageToken() *service.ValidationError {
	return &service.ValidationError{
		Message: "the page token is not acceptable",
		Fields:  []service.FieldError{{Field: "page_token", Reason: "is not a token this API issued"}},
	}
}

// tagsByResource loads the tags of every given resource id in one query.
func (s *Service) tagsByResource(
	ctx context.Context, ids []string,
) (map[string]map[string]string, error) {
	grouped := make(map[string]map[string]string, len(ids))
	if len(ids) == 0 {
		return grouped, nil
	}

	rows, err := s.store.ListTagsForResources(ctx,
		sqlc.ListTagsForResourcesParams{ResourceIds: ids})
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	for _, row := range rows {
		if grouped[row.ResourceID] == nil {
			grouped[row.ResourceID] = map[string]string{}
		}
		grouped[row.ResourceID][row.Key] = row.Value
	}
	return grouped, nil
}

// replaceTags swaps the whole tag set of a resource. The caller runs it inside a
// transaction.
func replaceTags(ctx context.Context, tx *store.Tx, resourceID string, tags map[string]string) error {
	if err := tx.DeleteResourceTags(ctx, sqlc.DeleteResourceTagsParams{ResourceID: resourceID}); err != nil {
		return fmt.Errorf("delete tags: %w", err)
	}
	for key, value := range tags {
		if err := tx.SetTag(ctx, sqlc.SetTagParams{
			ResourceID: resourceID, Key: key, Value: value,
		}); err != nil {
			return fmt.Errorf("set tag %q: %w", key, err)
		}
	}
	return nil
}

// imageFromRow projects a row onto the API type. The nullable columns become
// pointers so the JSON carries null rather than an empty string, and the tags are
// never nil.
func imageFromRow(row sqlc.Image, tags map[string]string) Image {
	image := Image{
		ID:           row.ID,
		Name:         row.Name,
		Description:  row.Description,
		State:        State(row.State),
		Progress:     int(row.Progress),
		Architecture: Architecture(row.Architecture),
		OSFamily:     row.OsFamily,
		OSVersion:    row.OsVersion,
		DefaultUser:  row.DefaultUser,
		MinDiskGB:    int(row.MinDiskGb),
		CreatedAt:    parseTime(row.CreatedAt),
		UpdatedAt:    parseTime(row.UpdatedAt),
		Tags:         map[string]string{},
	}
	for key, value := range tags {
		image.Tags[key] = value
	}

	// The format column holds a provisional value until the header parse
	// settles it, so it is only reported for an available image.
	if State(row.State) == StateAvailable {
		image.Format = imageFormat(row.Format)
	}

	if row.StateReason.Valid {
		reason := row.StateReason.String
		image.StateReason = &reason
	}
	if row.SourceUrl.Valid {
		source := row.SourceUrl.String
		image.SourceURL = &source
	}
	if row.ChecksumAlgo.Valid && row.ChecksumValue.Valid {
		image.Checksum = &Checksum{
			Algorithm: ChecksumAlgorithm(row.ChecksumAlgo.String),
			Value:     row.ChecksumValue.String,
		}
	}
	if row.SizeBytes.Valid {
		size := row.SizeBytes.Int64
		image.SizeBytes = &size
	}

	return image
}

// newImageID mints the id that is also the volume name, so the mapping between a
// row and a volume needs no second column at runtime.
func newImageID() string { return id.Resource(id.PrefixImage) }

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		// A row this package did not write; the zero time is the honest answer.
		return time.Time{}
	}
	return parsed.UTC()
}

// volumeName is the libvirt volume of an image: the image id itself. It is
// derived rather than stored so a retry of the same task always targets the same
// volume.
func volumeName(imageID string) string { return imageID }

// Handler returns the worker handlers for the image workflows, keyed by task
// type. The worker registry is built from this, so a missing type fails at
// startup rather than at the first download.
func (s *Service) Handler() map[string]worker.Handler {
	return map[string]worker.Handler{
		task.TypeImageImport: worker.HandlerFunc(s.ImportHandler),
		task.TypeImageDelete: worker.HandlerFunc(s.DeleteHandler),
	}
}

// isUniqueViolation reports a UNIQUE constraint failure. The match is exact
// because a bare "constraint failed" also covers CHECK, NOT NULL and foreign key
// violations, which are not conflicts.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// LaunchSource is what a launch needs from an image: the volume to boot from and
// the facts the seed is rendered from.
//
// It is deliberately not part of the Image projection: volume_name is where the
// image lives inside libvirt, which is an implementation detail an API client has
// no business reading (docs/design/04-libvirt-integration.md §2).
type LaunchSource struct {
	ID         string
	Name       string
	State      State
	VolumeName string
	// DefaultUser is the login cloud-config grants a key pair to.
	DefaultUser string
	// MinDiskGB is the smallest root disk this image boots from.
	MinDiskGB int
}

// LaunchSource returns the image a launch boots from.
func (s *Service) LaunchSource(ctx context.Context, imageID string) (LaunchSource, error) {
	row, err := s.store.GetImage(ctx, sqlc.GetImageParams{ID: imageID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LaunchSource{}, fmt.Errorf("%w: no image %q", service.ErrNotFound, imageID)
		}
		return LaunchSource{}, fmt.Errorf("get image: %w", err)
	}

	return LaunchSource{
		ID:          row.ID,
		Name:        row.Name,
		State:       State(row.State),
		VolumeName:  row.VolumeName.String,
		DefaultUser: row.DefaultUser,
		MinDiskGB:   int(row.MinDiskGb),
	}, nil
}
