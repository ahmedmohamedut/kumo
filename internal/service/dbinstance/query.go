package dbinstance

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Get returns one DB instance.
func (s *Service) Get(ctx context.Context, dbInstanceID string) (DBInstance, error) {
	row, err := s.store.GetDBInstance(ctx, sqlc.GetDBInstanceParams{ID: dbInstanceID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DBInstance{}, fmt.Errorf("%w: no DB instance %q", service.ErrNotFound, dbInstanceID)
		}
		return DBInstance{}, fmt.Errorf("get DB instance: %w", err)
	}

	out := fromRow(row)
	servers, err := s.servers(ctx, []string{row.ID})
	if err != nil {
		return DBInstance{}, err
	}
	applyServer(&out, servers[row.ID])

	// The task of the most recent state change is what the console polls while
	// a DB instance is creating.
	taskID, err := s.latestTaskID(ctx, row.ID)
	if err != nil {
		return DBInstance{}, err
	}
	out.TaskID = taskID

	return out, nil
}

// ListInput selects a page of DB instances.
type ListInput struct {
	// Query matches a substring of the identifier.
	Query string
	// State restricts the page to one state.
	State State
	Sort  SortOrder
	// PageSize and PageToken are the keyset pagination controls.
	PageSize  int
	PageToken string
}

// List returns one page of DB instances, newest first by default.
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

	if in.State != "" && !knownState(in.State) {
		return Page{}, &service.ValidationError{
			Message: "the state filter is not acceptable",
			Fields: []service.FieldError{{
				Field: "state",
				Reason: "must be creating, available, starting, stopping, stopped, " +
					"rebooting, deleting or failed",
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

	rows, err := s.listPage(ctx, in, cursor)
	if err != nil {
		return Page{}, fmt.Errorf("list DB instances: %w", err)
	}

	page := Page{Items: make([]DBInstance, 0, min(len(rows), in.PageSize))}
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
	servers, err := s.servers(ctx, ids)
	if err != nil {
		return Page{}, err
	}

	// A list never carries the task of each row: that is one more query per
	// row, and a table does not show it. Get returns it.
	for _, row := range rows {
		out := fromRow(row)
		applyServer(&out, servers[row.ID])
		page.Items = append(page.Items, out)
	}
	return page, nil
}

// listPage runs the asc or desc variant of the listing query and returns at
// most PageSize+1 rows; the extra row is how the caller learns there is a next
// page.
func (s *Service) listPage(
	ctx context.Context, in ListInput, cursor pageCursor,
) ([]sqlc.DbInstance, error) {
	params := sqlc.ListDBInstancesDescParams{
		Query:         in.Query,
		StateFilter:   string(in.State),
		CursorCreated: cursor.created,
		CursorID:      cursor.id,
		PageLimit:     int64(in.PageSize) + 1,
	}
	if in.Sort == SortCreatedAsc {
		return s.store.ListDBInstancesAsc(ctx, sqlc.ListDBInstancesAscParams(params))
	}
	return s.store.ListDBInstancesDesc(ctx, params)
}

// server is what the backing VM contributes to a DB instance projection. The
// compute shape is not snapshotted on the db_instances row: the instances row
// already holds it, and that row is the one capacity sums (section 3).
type server struct {
	vcpus     int
	memoryMiB int
	privateIP string
}

// servers reads the backing VMs of several DB instances at once.
func (s *Service) servers(ctx context.Context, dbInstanceIDs []string) (map[string]server, error) {
	if len(dbInstanceIDs) == 0 {
		return map[string]server{}, nil
	}
	rows, err := s.store.ListDBInstanceServers(ctx, sqlc.ListDBInstanceServersParams{
		DbInstanceIds: dbInstanceIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("list the servers of the DB instances: %w", err)
	}

	out := make(map[string]server, len(rows))
	for _, row := range rows {
		out[row.DbInstanceID] = server{
			vcpus:     int(row.Vcpus),
			memoryMiB: int(row.MemoryMib),
			privateIP: row.PrivateIp.String,
		}
	}
	return out, nil
}

// applyServer fills the compute shape of a projection from its backing VM.
func applyServer(out *DBInstance, srv server) {
	out.VCPUs = srv.vcpus
	out.MemoryMiB = srv.memoryMiB
}

// fromRow projects a row onto the API type. The nullable columns become
// pointers so the JSON carries null rather than an empty string.
func fromRow(row sqlc.DbInstance) DBInstance {
	out := DBInstance{
		ID:            row.ID,
		Identifier:    row.Identifier,
		State:         State(row.State),
		Engine:        Engine(row.Engine),
		EngineVersion: row.EngineVersion,
		ImageID:       row.ImageID,
		InstanceID:    row.InstanceID,
		InstanceType:  row.InstanceType,
		StorageGB:     int(row.StorageGb),
		Port:          int(row.Port),
		AdminUsername: row.AdminUsername,
		DatabaseName:  row.DatabaseName,
		CreatedAt:     parseTime(row.CreatedAt),
		UpdatedAt:     parseTime(row.UpdatedAt),
	}
	if row.StateReason.Valid {
		out.StateReason = &row.StateReason.String
	}
	if row.Endpoint.Valid {
		out.Endpoint = &row.Endpoint.String
	}
	return out
}

// knownState reports whether a state filter is one of the states the schema
// allows.
func knownState(state State) bool {
	switch state {
	case StateCreating, StateAvailable, StateStarting, StateStopping,
		StateStopped, StateRebooting, StateDeleting, StateFailed:
		return true
	default:
		return false
	}
}

// pageCursor is the position a page token encodes.
type pageCursor struct {
	created string
	id      string
}

// encodePageToken packs the cursor of a row into an opaque token.
func encodePageToken(createdAt, dbInstanceID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt + "|" + dbInstanceID))
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
	created, dbInstanceID, ok := strings.Cut(string(raw), "|")
	if !ok || created == "" || !id.HasResourceShape(id.PrefixDBInstance, dbInstanceID) {
		return pageCursor{}, invalidPageToken()
	}
	return pageCursor{created: created, id: dbInstanceID}, nil
}

func invalidPageToken() *service.ValidationError {
	return &service.ValidationError{
		Message: "the page token is not acceptable",
		Fields:  []service.FieldError{{Field: "page_token", Reason: "is not a token this API issued"}},
	}
}

// latestTaskID returns the id of the most recent task for a DB instance, or nil
// when it has none.
func (s *Service) latestTaskID(ctx context.Context, dbInstanceID string) (*string, error) {
	row, err := s.store.GetLatestInstanceTask(ctx, sqlc.GetLatestInstanceTaskParams{
		ResourceID: sql.NullString{String: dbInstanceID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get the last task of DB instance %q: %w", dbInstanceID, err)
	}
	return &row.ID, nil
}
