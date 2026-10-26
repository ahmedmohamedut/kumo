// Package bucket owns the object storage buckets (docs/design/06-object-storage.md).
//
// RustFS is the source of truth for which buckets exist; the buckets table
// holds what Kumo adds, its creator and its state, and the tags table holds
// its tags under the resource id "bucket/<name>". A bucket created outside
// Kumo is listed like any other and gains a row the first time it is changed
// here.
package bucket

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Sentinel errors of the constructor, so a caller can tell a wiring mistake
// from a runtime failure.
var (
	// ErrNoLogger means Options.Logger was nil.
	ErrNoLogger = errors.New("bucket: a logger is required")
	// ErrNoStore means Options.Store was nil.
	ErrNoStore = errors.New("bucket: a store is required")
	// ErrNoObjectStore means Options.Objects was nil.
	ErrNoObjectStore = errors.New("bucket: an object store is required")
	// ErrNoTasks means Options.Tasks was nil.
	ErrNoTasks = errors.New("bucket: a task service is required")
	// ErrBadTaskPayload means a bucket.delete task names no bucket.
	ErrBadTaskPayload = errors.New("bucket: the task payload names no bucket")
)

// State is Kumo's view of a bucket's lifecycle.
type State string

// The bucket states, mirroring the CHECK constraint on buckets.state.
const (
	StateAvailable State = "available"
	// StateDeleting means a forced delete is emptying the bucket.
	StateDeleting State = "deleting"
)

// Bucket is one bucket as the API returns it.
type Bucket struct {
	Name      string    `json:"name"`
	State     State     `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	// CreatedBy is the username of the creator; nil for a bucket made outside
	// Kumo or whose creator was deleted.
	CreatedBy *string           `json:"created_by"`
	Tags      map[string]string `json:"tags"`
}

// Detail is a bucket plus what its detail page shows.
type Detail struct {
	Bucket
	Versioning objectstore.Versioning `json:"versioning"`
	// Endpoint and Region are what an S3 client is configured with.
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
}

// CreateInput is a POST /s3/buckets body.
type CreateInput struct {
	Name       string
	Versioning bool
	Tags       map[string]string
	CreatedBy  string
}

// UpdateInput is a PATCH /s3/buckets/{name} body. A nil field is left alone.
type UpdateInput struct {
	Versioning *bool
	Tags       map[string]string
	// SetTags distinguishes an absent tags field from {}, which drops every tag.
	SetTags bool
}

// Deleted is the answer to a forced delete: the bucket in its deleting state
// and the task emptying it.
type Deleted struct {
	Bucket Bucket `json:"bucket"`
	TaskID string `json:"task_id"`
}

// Options configures the service.
type Options struct {
	Logger  *slog.Logger
	Store   *store.Store
	Objects objectstore.ObjectStore
	Tasks   *task.Service
	// PublicURL and Region are reported on the detail page.
	PublicURL string
	Region    string
	// CORSOrigins are allowed on every bucket Kumo creates; empty sets no rule.
	CORSOrigins []string
}

// Service owns the buckets.
type Service struct {
	logger      *slog.Logger
	store       *store.Store
	objects     objectstore.ObjectStore
	tasks       *task.Service
	publicURL   string
	region      string
	corsOrigins []string
	now         func() time.Time
}

// New builds the service.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Logger == nil:
		return nil, ErrNoLogger
	case opts.Store == nil:
		return nil, ErrNoStore
	case opts.Objects == nil:
		return nil, ErrNoObjectStore
	case opts.Tasks == nil:
		return nil, ErrNoTasks
	}
	return &Service{
		logger:      opts.Logger,
		store:       opts.Store,
		objects:     opts.Objects,
		tasks:       opts.Tasks,
		publicURL:   opts.PublicURL,
		region:      opts.Region,
		corsOrigins: opts.CORSOrigins,
		now:         time.Now,
	}, nil
}

// ResourceID is the id a bucket's tags and tasks are filed under.
func ResourceID(name string) string { return "bucket/" + name }

// List returns every bucket in the object store, by name, with Kumo's
// metadata where it has some.
//
// A row whose bucket has gone from the server (an operator deleted it with
// another client) is removed here, so a bucket recreated later under the same
// name does not inherit a stranger's tags.
func (s *Service) List(ctx context.Context) ([]Bucket, error) {
	listed, err := s.objects.ListBuckets(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	rows, err := s.store.ListBucketRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("bucket: list the rows: %w", err)
	}

	byName := make(map[string]sqlc.ListBucketRowsRow, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
	}

	present := make(map[string]bool, len(listed))
	ids := make([]string, 0, len(listed))
	for _, b := range listed {
		present[b.Name] = true
		ids = append(ids, ResourceID(b.Name))
	}
	for _, row := range rows {
		if !present[row.Name] {
			s.forget(ctx, row.Name, "the bucket is gone from the object store")
		}
	}

	tags, err := s.tagsFor(ctx, ids)
	if err != nil {
		return nil, err
	}

	buckets := make([]Bucket, 0, len(listed))
	for _, b := range listed {
		buckets = append(buckets, merge(b, byName[b.Name], tags[ResourceID(b.Name)]))
	}
	return buckets, nil
}

// Get returns one bucket with its versioning and connection details.
func (s *Service) Get(ctx context.Context, name string) (Detail, error) {
	b, err := s.get(ctx, name)
	if err != nil {
		return Detail{}, err
	}
	versioning, err := s.objects.GetVersioning(ctx, name)
	if err != nil {
		return Detail{}, mapErr(err)
	}
	return Detail{Bucket: b, Versioning: versioning, Endpoint: s.publicURL, Region: s.region}, nil
}

// get returns one bucket without the detail fields.
func (s *Service) get(ctx context.Context, name string) (Bucket, error) {
	if !validName(name) {
		// No bucket can have this name, so it is a 404 and not a 400: the
		// name is in the path.
		return Bucket{}, fmt.Errorf("%w: no bucket %q", service.ErrNotFound, name)
	}
	listed, err := s.objects.HeadBucket(ctx, name)
	if err != nil {
		return Bucket{}, mapErr(err)
	}
	row, err := s.store.GetBucketRow(ctx, sqlc.GetBucketRowParams{Name: name})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Bucket{}, fmt.Errorf("bucket: read the row of %q: %w", name, err)
	}
	tags, err := s.tagsFor(ctx, []string{ResourceID(name)})
	if err != nil {
		return Bucket{}, err
	}
	return merge(listed, sqlc.ListBucketRowsRow(row), tags[ResourceID(name)]), nil
}

// Create makes a bucket, then applies versioning and the console's CORS rule
// and records it. A failure after the bucket exists removes it again, so a
// create either leaves a configured bucket or nothing.
func (s *Service) Create(ctx context.Context, in CreateInput) (Detail, error) {
	var fields []service.FieldError
	if field := validateName(in.Name); field != nil {
		fields = append(fields, *field)
	}
	if field := validateTags(in.Tags); field != nil {
		fields = append(fields, *field)
	}
	if len(fields) > 0 {
		return Detail{}, &service.ValidationError{Message: "the bucket is not acceptable", Fields: fields}
	}

	if err := s.objects.CreateBucket(ctx, in.Name); err != nil {
		if errors.Is(err, objectstore.ErrAlreadyExists) {
			return Detail{}, fmt.Errorf("%w: a bucket named %q already exists", service.ErrAlreadyExists, in.Name)
		}
		return Detail{}, mapErr(err)
	}

	if err := s.configure(ctx, in); err != nil {
		s.rollback(ctx, in.Name, err)
		return Detail{}, err
	}

	s.logger.InfoContext(ctx, "bucket: created",
		"bucket", in.Name, "versioning", in.Versioning, "user_id", in.CreatedBy)
	return s.Get(ctx, in.Name)
}

// configure is the part of a create that follows the bucket's creation.
func (s *Service) configure(ctx context.Context, in CreateInput) error {
	if in.Versioning {
		if err := s.objects.SetVersioning(ctx, in.Name, true); err != nil {
			return mapErr(err)
		}
	}
	if len(s.corsOrigins) > 0 {
		if err := s.objects.SetCORS(ctx, in.Name, s.corsOrigins); err != nil {
			return mapErr(err)
		}
	}

	now := formatTime(s.now())
	return s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		// A row left by a bucket of the same name that was deleted outside
		// Kumo would carry the wrong creator and tags.
		if err := forgetInTx(ctx, tx, in.Name); err != nil {
			return err
		}
		if err := tx.CreateBucketRow(ctx, sqlc.CreateBucketRowParams{
			Name:      in.Name,
			CreatedBy: nullable(in.CreatedBy),
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("bucket: record %q: %w", in.Name, err)
		}
		return replaceTags(ctx, tx, ResourceID(in.Name), in.Tags)
	})
}

// rollback removes a bucket whose create failed halfway. It is empty, since
// nobody has had its name for longer than this request.
func (s *Service) rollback(ctx context.Context, name string, cause error) {
	ctx = context.WithoutCancel(ctx)
	if err := s.objects.DeleteBucket(ctx, name); err != nil && !errors.Is(err, objectstore.ErrNotFound) {
		s.logger.ErrorContext(ctx, "bucket: a failed create left the bucket behind",
			"bucket", name, "cause", cause, "error", err)
		return
	}
	s.logger.WarnContext(ctx, "bucket: create rolled back", "bucket", name, "cause", cause)
}

// Update changes versioning and/or tags.
func (s *Service) Update(ctx context.Context, name string, in UpdateInput) (Detail, error) {
	if in.SetTags {
		if field := validateTags(in.Tags); field != nil {
			return Detail{}, &service.ValidationError{Message: "the bucket update is not acceptable",
				Fields: []service.FieldError{*field}}
		}
	}

	current, err := s.get(ctx, name)
	if err != nil {
		return Detail{}, err
	}
	if current.State == StateDeleting {
		return Detail{}, fmt.Errorf("%w: bucket %q is being deleted", service.ErrConflict, name)
	}

	if in.Versioning != nil {
		if err := s.setVersioning(ctx, name, *in.Versioning); err != nil {
			return Detail{}, err
		}
	}
	if in.SetTags {
		err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
			if err := tx.EnsureBucketRow(ctx, sqlc.EnsureBucketRowParams{
				Name: name, CreatedAt: formatTime(s.now()),
			}); err != nil {
				return fmt.Errorf("bucket: record %q: %w", name, err)
			}
			return replaceTags(ctx, tx, ResourceID(name), in.Tags)
		})
		if err != nil {
			return Detail{}, err
		}
	}

	s.logger.InfoContext(ctx, "bucket: updated", "bucket", name,
		"versioning_changed", in.Versioning != nil, "tags_changed", in.SetTags)
	return s.Get(ctx, name)
}

// setVersioning applies the S3 rule that a bucket that was never versioned
// stays unversioned when asked to turn versioning off: suspending it would
// change its state for nothing.
func (s *Service) setVersioning(ctx context.Context, name string, enabled bool) error {
	if !enabled {
		state, err := s.objects.GetVersioning(ctx, name)
		if err != nil {
			return mapErr(err)
		}
		if state == objectstore.VersioningOff {
			return nil
		}
	}
	if err := s.objects.SetVersioning(ctx, name, enabled); err != nil {
		return mapErr(err)
	}
	return nil
}

// tagsFor reads the tags of several resources, grouped by resource id.
func (s *Service) tagsFor(ctx context.Context, ids []string) (map[string]map[string]string, error) {
	grouped := map[string]map[string]string{}
	if len(ids) == 0 {
		return grouped, nil
	}
	rows, err := s.store.ListTagsForResources(ctx, sqlc.ListTagsForResourcesParams{ResourceIds: ids})
	if err != nil {
		return nil, fmt.Errorf("bucket: read the tags: %w", err)
	}
	for _, row := range rows {
		if grouped[row.ResourceID] == nil {
			grouped[row.ResourceID] = map[string]string{}
		}
		grouped[row.ResourceID][row.Key] = row.Value
	}
	return grouped, nil
}

// forget removes the row and tags of a bucket that no longer exists. A
// failure is logged, not returned: the listing is still right without it.
func (s *Service) forget(ctx context.Context, name, reason string) {
	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		return forgetInTx(ctx, tx, name)
	})
	if err != nil {
		s.logger.WarnContext(ctx, "bucket: could not drop a stale row", "bucket", name, "error", err)
		return
	}
	s.logger.InfoContext(ctx, "bucket: dropped a stale row", "bucket", name, "reason", reason)
}

func forgetInTx(ctx context.Context, tx *store.Tx, name string) error {
	if err := tx.DeleteResourceTags(ctx, sqlc.DeleteResourceTagsParams{ResourceID: ResourceID(name)}); err != nil {
		return fmt.Errorf("bucket: delete the tags of %q: %w", name, err)
	}
	if err := tx.DeleteBucketRow(ctx, sqlc.DeleteBucketRowParams{Name: name}); err != nil {
		return fmt.Errorf("bucket: delete the row of %q: %w", name, err)
	}
	return nil
}

// replaceTags swaps the whole tag set of a resource inside a transaction.
func replaceTags(ctx context.Context, tx *store.Tx, resourceID string, tags map[string]string) error {
	if err := tx.DeleteResourceTags(ctx, sqlc.DeleteResourceTagsParams{ResourceID: resourceID}); err != nil {
		return fmt.Errorf("bucket: delete tags: %w", err)
	}
	for key, value := range tags {
		if err := tx.SetTag(ctx, sqlc.SetTagParams{ResourceID: resourceID, Key: key, Value: value}); err != nil {
			return fmt.Errorf("bucket: set tag %q: %w", key, err)
		}
	}
	return nil
}

// merge combines the server's view of a bucket with Kumo's row and tags. A
// zero row is a bucket made outside Kumo.
func merge(b objectstore.Bucket, row sqlc.ListBucketRowsRow, tags map[string]string) Bucket {
	out := Bucket{
		Name:      b.Name,
		State:     StateAvailable,
		CreatedAt: b.CreatedAt,
		Tags:      tags,
	}
	if out.Tags == nil {
		out.Tags = map[string]string{}
	}
	if row.State != "" {
		out.State = State(row.State)
	}
	if row.CreatedByUsername.Valid {
		username := row.CreatedByUsername.String
		out.CreatedBy = &username
	}
	return out
}

// mapErr translates object store errors into service errors.
func mapErr(err error) error {
	switch {
	case errors.Is(err, objectstore.ErrNotFound):
		return fmt.Errorf("%w: %w", service.ErrNotFound, err)
	case errors.Is(err, objectstore.ErrAlreadyExists):
		return fmt.Errorf("%w: %w", service.ErrAlreadyExists, err)
	case errors.Is(err, objectstore.ErrBucketNotEmpty):
		return fmt.Errorf("%w: %w", service.ErrConflict, err)
	case errors.Is(err, objectstore.ErrInvalidName):
		return &service.ValidationError{
			Message: "the object store refused the name",
			Fields:  []service.FieldError{{Field: "name", Reason: err.Error()}},
		}
	case errors.Is(err, objectstore.ErrUnavailable):
		return fmt.Errorf("%w: the object store: %w", service.ErrUnavailable, err)
	default:
		return fmt.Errorf("bucket: %w", err)
	}
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nullable(v string) sql.NullString {
	return sql.NullString{String: v, Valid: v != ""}
}
