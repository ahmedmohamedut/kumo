// Package task owns the metadata of the long operations: it creates task rows
// and answers the /api/v1/tasks endpoints. The worker package runs them.
//
// A task row is the only handle a client has on work that happens after the
// response, so it is written in the same transaction as the resource it belongs
// to: an image row and its import task either both exist or neither does.
package task

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Task types Kumo creates. The names are what the worker registry keys on
// (docs/design/02-data-model.md).
const (
	// TypeImageImport downloads an image and registers it as a volume.
	TypeImageImport = "image.import"
	// TypeImageDelete removes the volume and the row of a deleted image.
	TypeImageDelete = "image.delete"
	// TypeInstanceLaunch creates the volumes, the domain and the address of a new
	// instance.
	TypeInstanceLaunch = "instance.launch"
	// TypeInstanceStart boots a domain that was shut off.
	TypeInstanceStart = "instance.start"
	// TypeInstanceStop powers an instance down, gracefully or not.
	TypeInstanceStop = "instance.stop"
	// TypeInstanceReboot restarts a running domain.
	TypeInstanceReboot = "instance.reboot"
	// TypeInstanceTerminate destroys the domain and deletes the volumes of an
	// instance, which is the only operation that releases its allocation.
	TypeInstanceTerminate = "instance.terminate"
	// TypeVPCCreate defines and starts the libvirt network of a new VPC.
	TypeVPCCreate = "vpc.create"
	// TypeVPCDelete destroys, undefines and removes the row of a deleted VPC.
	TypeVPCDelete = "vpc.delete"
	// TypeBucketDelete empties an object storage bucket of every object and
	// version, then deletes it.
	TypeBucketDelete = "bucket.delete"
	// TypeDBInstanceCreate builds the server behind a DB instance: the data
	// disk, the VM, and the wait for the engine to answer.
	TypeDBInstanceCreate = "db.create"
	// TypeDBInstanceDelete removes the server, the data disk and both rows of
	// a deleted DB instance.
	TypeDBInstanceDelete = "db.delete"
	// TypeK8sClusterCreate builds a Kubernetes cluster: the control plane, its
	// endpoint record, the workers, until every node is Ready
	// (docs/design/08-kubernetes.md section 4).
	TypeK8sClusterCreate = "k8s.create"
	// TypeK8sClusterDelete removes every node, the endpoint record, the rows
	// and, last, the cluster's PKI.
	TypeK8sClusterDelete = "k8s.delete"
)

// State is the lifecycle of a task. The values mirror the CHECK constraint on
// tasks.state.
type State string

const (
	// StatePending means the task is queued.
	StatePending State = "pending"
	// StateRunning means a worker claimed it.
	StateRunning State = "running"
	// StateSucceeded means it finished.
	StateSucceeded State = "succeeded"
	// StateFailed means it stopped with an error, which is in Error.
	StateFailed State = "failed"
)

// SortOrder selects the order of a task page.
type SortOrder string

// The supported sort orders.
const (
	SortCreatedDesc SortOrder = "created_at:desc"
	SortCreatedAsc  SortOrder = "created_at:asc"
)

// Pagination bounds, the same as every other list endpoint.
const (
	MinPageSize     = 1
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// Task is the safe projection of a task row. The payload is deliberately absent:
// it is the workflow's internal input, not something a client needs.
type Task struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	ResourceID *string   `json:"resource_id"`
	State      State     `json:"state"`
	Progress   int       `json:"progress"`
	Error      *string   `json:"error"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Page is one page of a task listing.
type Page struct {
	Items         []Task  `json:"items"`
	NextPageToken *string `json:"next_page_token"`
}

// ListInput selects a page of tasks.
type ListInput struct {
	ResourceID string
	Sort       SortOrder
	PageSize   int
	PageToken  string
}

// Options configures the service.
type Options struct {
	Store *store.Store
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Service owns task metadata.
type Service struct {
	store *store.Store
	now   func() time.Time
}

// Sentinel errors of the constructor.
var (
	// ErrNoStore means Options.Store was nil.
	ErrNoStore = errors.New("task: a store is required")
	// ErrEmptyType means a caller asked for a task without a type.
	ErrEmptyType = errors.New("task: a task type is required")
)

// New builds the service.
func New(opts Options) (*Service, error) {
	if opts.Store == nil {
		return nil, ErrNoStore
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{store: opts.Store, now: opts.Now}, nil
}

// Create queues a task for a resource. payload is the workflow's own input; it
// is stored as JSON and never returned by the API.
//
// Callers that need the task and its resource to appear together must use
// CreateInTx instead of this.
func (s *Service) Create(ctx context.Context, taskType, resourceID string, payload any) (Task, error) {
	var task Task

	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		created, err := s.create(ctx, tx, taskType, resourceID, payload)
		task = created
		return err
	})
	if err != nil {
		return Task{}, err
	}
	return task, nil
}

// CreateInTx queues a task inside a transaction the caller owns, so the task and
// the resource it tracks commit together.
func (s *Service) CreateInTx(
	ctx context.Context, tx *store.Tx, taskType, resourceID string, payload any,
) (Task, error) {
	return s.create(ctx, tx, taskType, resourceID, payload)
}

// create inserts the row. An empty payload becomes `{}` rather than an empty
// string, so the worker's json.Unmarshal never sees invalid JSON.
func (s *Service) create(
	ctx context.Context, tx *store.Tx, taskType, resourceID string, payload any,
) (Task, error) {
	if strings.TrimSpace(taskType) == "" {
		return Task{}, fmt.Errorf("%w", ErrEmptyType)
	}

	encoded := []byte("{}")
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return Task{}, fmt.Errorf("task: encode the payload of %s: %w", taskType, err)
		}
		encoded = raw
	}

	now := formatTime(s.now())
	row, err := tx.CreateTask(ctx, sqlc.CreateTaskParams{
		ID:         id.Resource(id.PrefixTask),
		Type:       taskType,
		ResourceID: nullable(resourceID),
		State:      string(StatePending),
		Progress:   0,
		Payload:    string(encoded),
		Attempts:   0,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		return Task{}, fmt.Errorf("task: create a %s task: %w", taskType, err)
	}

	return fromRow(row), nil
}

// Get returns one task.
func (s *Service) Get(ctx context.Context, taskID string) (Task, error) {
	row, err := s.store.GetTask(ctx, sqlc.GetTaskParams{ID: taskID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, fmt.Errorf("%w: no task %q", service.ErrNotFound, taskID)
		}
		return Task{}, fmt.Errorf("get task: %w", err)
	}
	return fromRow(row), nil
}

// List returns one page of tasks, optionally restricted to one resource.
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

	resourceID := nullable(in.ResourceID)
	rows, err := s.listPage(ctx, in, cursor, resourceID)
	if err != nil {
		return Page{}, fmt.Errorf("list tasks: %w", err)
	}

	page := Page{Items: make([]Task, 0, min(len(rows), in.PageSize))}
	if len(rows) > in.PageSize {
		last := rows[in.PageSize-1]
		token := encodePageToken(last.CreatedAt, last.ID)
		page.NextPageToken = &token
		rows = rows[:in.PageSize]
	}

	for _, row := range rows {
		page.Items = append(page.Items, fromRow(row))
	}
	return page, nil
}

// listPage runs the asc or desc variant of the listing query and returns at most
// PageSize+1 rows; the extra row is how the caller learns there is a next page.
func (s *Service) listPage(
	ctx context.Context, in ListInput, cursor pageCursor, resourceID sql.NullString,
) ([]sqlc.Task, error) {
	if in.Sort == SortCreatedAsc {
		return s.store.ListTasksAsc(ctx, sqlc.ListTasksAscParams{
			CursorCreated: cursor.created,
			CursorID:      cursor.id,
			ResourceID:    resourceID,
			PageLimit:     int64(in.PageSize) + 1,
		})
	}
	return s.store.ListTasksDesc(ctx, sqlc.ListTasksDescParams{
		CursorCreated: cursor.created,
		CursorID:      cursor.id,
		ResourceID:    resourceID,
		PageLimit:     int64(in.PageSize) + 1,
	})
}

// pageCursor is the position a page token encodes.
type pageCursor struct {
	created string
	id      string
}

// encodePageToken packs the cursor of a row into an opaque token.
func encodePageToken(createdAt, taskID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt + "|" + taskID))
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
	created, taskID, ok := strings.Cut(string(raw), "|")
	if !ok || created == "" || !id.HasResourceShape(id.PrefixTask, taskID) {
		return pageCursor{}, invalidPageToken()
	}
	return pageCursor{created: created, id: taskID}, nil
}

func invalidPageToken() *service.ValidationError {
	return &service.ValidationError{
		Message: "the page token is not acceptable",
		Fields:  []service.FieldError{{Field: "page_token", Reason: "is not a token this API issued"}},
	}
}

// fromRow projects a row onto the API type. The nullable columns become pointers
// so the JSON carries null rather than an empty string.
func fromRow(row sqlc.Task) Task {
	task := Task{
		ID:        row.ID,
		Type:      row.Type,
		State:     State(row.State),
		Progress:  int(row.Progress),
		CreatedAt: parseTime(row.CreatedAt),
		UpdatedAt: parseTime(row.UpdatedAt),
	}
	if row.ResourceID.Valid {
		id := row.ResourceID.String
		task.ResourceID = &id
	}
	if row.Error.Valid {
		message := row.Error.String
		task.Error = &message
	}
	return task
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		// A row this package did not write; the zero time is the honest answer.
		return time.Time{}
	}
	return parsed.UTC()
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nullable(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}
