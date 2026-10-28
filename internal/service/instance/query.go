package instance

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

// imageName is the denormalised image of an instance projection. The row only
// carries the image id; a list view also wants the name, and one query for the
// whole page beats one per instance.
type imageName struct {
	ID   string
	Name string
}

// Get returns one instance with its tags.
//
// includeUserData is the caller's decision: the seed payload is returned only for
// an administrator, because a user may have pasted a secret into it
// (docs/design/01-api.md §Instances).
func (s *Service) Get(ctx context.Context, instanceID string, includeUserData bool) (Instance, error) {
	row, err := s.store.GetInstance(ctx, sqlc.GetInstanceParams{ID: instanceID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Instance{}, fmt.Errorf("%w: no instance %q", service.ErrNotFound, instanceID)
		}
		return Instance{}, fmt.Errorf("get instance: %w", err)
	}

	tags, err := s.tagsByResource(ctx, []string{row.ID})
	if err != nil {
		return Instance{}, err
	}
	images, err := s.imageNames(ctx, []string{row.ImageID})
	if err != nil {
		return Instance{}, err
	}
	interfaces, err := s.interfacesByInstances(ctx, []string{row.ID})
	if err != nil {
		return Instance{}, err
	}

	out := instanceFromRow(row, images[row.ImageID], tags[row.ID])
	applyNetwork(&out, interfaces[row.ID])
	if includeUserData && row.UserData.Valid {
		out.UserData = &row.UserData.String
	}

	// The task of the most recent state change is what the console polls while an
	// instance is pending, so a single read carries it rather than making the
	// console ask for the task list too.
	taskID, err := s.latestTaskID(ctx, row.ID)
	if err != nil {
		return Instance{}, err
	}
	out.TaskID = taskID

	return out, nil
}

// ListInput selects a page of instances.
type ListInput struct {
	// Query matches a substring of the name.
	Query string
	// State restricts the page to one state.
	State State
	Sort  SortOrder
	// PageSize and PageToken are the keyset pagination controls.
	PageSize  int
	PageToken string
	// IncludeManaged also returns the VMs another module owns. The zero value
	// hides them, which is what makes every caller that does not think about
	// it -- the Instances pages, the dashboard tallies, a name search -- safe
	// by default (docs/design/07-databases.md R7).
	IncludeManaged bool
}

// List returns one page of instances, newest first by default.
func (s *Service) List(ctx context.Context, in ListInput) (Page, error) {
	if in.Sort == "" {
		in.Sort = SortLaunchedDesc
	}
	switch in.Sort {
	case SortLaunchedAsc, SortLaunchedDesc:
	default:
		return Page{}, &service.ValidationError{
			Message: "the sort order is not acceptable",
			Fields: []service.FieldError{{
				Field:  "sort",
				Reason: `must be "launched_at:asc" or "launched_at:desc"`,
			}},
		}
	}

	if in.State != "" {
		switch in.State {
		case StatePending, StateRunning, StateStopping, StateStopped,
			StateTerminating, StateTerminated, StateError:
		default:
			return Page{}, &service.ValidationError{
				Message: "the state filter is not acceptable",
				Fields: []service.FieldError{{
					Field:  "state",
					Reason: "must be pending, running, stopping, stopped, terminating, terminated or error",
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
		return Page{}, fmt.Errorf("list instances: %w", err)
	}

	page := Page{Items: make([]Instance, 0, min(len(rows), in.PageSize))}
	if len(rows) > in.PageSize {
		last := rows[in.PageSize-1]
		token := encodePageToken(last.LaunchedAt, last.ID)
		page.NextPageToken = &token
		rows = rows[:in.PageSize]
	}

	ids := make([]string, 0, len(rows))
	imageIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
		imageIDs = append(imageIDs, row.ImageID)
	}

	tags, err := s.tagsByResource(ctx, ids)
	if err != nil {
		return Page{}, err
	}
	images, err := s.imageNames(ctx, imageIDs)
	if err != nil {
		return Page{}, err
	}
	interfaces, err := s.interfacesByInstances(ctx, ids)
	if err != nil {
		return Page{}, err
	}

	// A list never carries the task of each instance: that is one more query per
	// row, and a table does not show it. Get returns it.
	for _, row := range rows {
		out := instanceFromRow(row, images[row.ImageID], tags[row.ID])
		applyNetwork(&out, interfaces[row.ID])
		page.Items = append(page.Items, out)
	}

	return page, nil
}

// listPage runs the asc or desc variant of the listing query and returns at most
// PageSize+1 rows; the extra row is how the caller learns there is a next page.
func (s *Service) listPage(ctx context.Context, in ListInput, cursor pageCursor) ([]sqlc.Instance, error) {
	includeManaged := int64(0)
	if in.IncludeManaged {
		includeManaged = 1
	}
	params := sqlc.ListInstancesDescParams{
		Query:          in.Query,
		StateFilter:    string(in.State),
		IncludeManaged: includeManaged,
		CursorLaunched: cursor.launched,
		CursorID:       cursor.id,
		PageLimit:      int64(in.PageSize) + 1,
	}
	if in.Sort == SortLaunchedAsc {
		return s.store.ListInstancesAsc(ctx, sqlc.ListInstancesAscParams(params))
	}
	return s.store.ListInstancesDesc(ctx, params)
}

// pageCursor is the position a page token encodes.
type pageCursor struct {
	launched string
	id       string
}

// encodePageToken packs the cursor of a row into an opaque token.
func encodePageToken(launchedAt, instanceID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(launchedAt + "|" + instanceID))
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
	launched, instanceID, ok := strings.Cut(string(raw), "|")
	if !ok || launched == "" || !id.HasResourceShape(id.PrefixInstance, instanceID) {
		return pageCursor{}, invalidPageToken()
	}
	return pageCursor{launched: launched, id: instanceID}, nil
}

func invalidPageToken() *service.ValidationError {
	return &service.ValidationError{
		Message: "the page token is not acceptable",
		Fields:  []service.FieldError{{Field: "page_token", Reason: "is not a token this API issued"}},
	}
}

// latestTaskID returns the id of the most recent task for an instance, or nil when
// it has none. An instance whose task was deleted is not an error.
func (s *Service) latestTaskID(ctx context.Context, instanceID string) (*string, error) {
	row, err := s.store.GetLatestInstanceTask(ctx, sqlc.GetLatestInstanceTaskParams{
		ResourceID: sql.NullString{String: instanceID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get the last task of instance %q: %w", instanceID, err)
	}
	return &row.ID, nil
}

// tagsByResource reads the tags of several resources at once.
func (s *Service) tagsByResource(ctx context.Context, instanceIDs []string) (map[string]map[string]string, error) {
	rows, err := s.store.ListTagsForResources(ctx, sqlc.ListTagsForResourcesParams{
		ResourceIds: instanceIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("list the tags of the instances: %w", err)
	}

	tags := make(map[string]map[string]string, len(instanceIDs))
	for _, row := range rows {
		if _, ok := tags[row.ResourceID]; !ok {
			tags[row.ResourceID] = map[string]string{}
		}
		tags[row.ResourceID][row.Key] = row.Value
	}
	return tags, nil
}

// imageNames reads the names of the images behind a page.
func (s *Service) imageNames(ctx context.Context, imageIDs []string) (map[string]imageName, error) {
	unique := make([]string, 0, len(imageIDs))
	seen := make(map[string]struct{}, len(imageIDs))
	for _, imageID := range imageIDs {
		if _, ok := seen[imageID]; ok {
			continue
		}
		seen[imageID] = struct{}{}
		unique = append(unique, imageID)
	}
	if len(unique) == 0 {
		return map[string]imageName{}, nil
	}

	rows, err := s.store.ListImageNames(ctx, sqlc.ListImageNamesParams{Ids: unique})
	if err != nil {
		return nil, fmt.Errorf("list the images of the instances: %w", err)
	}

	names := make(map[string]imageName, len(rows))
	for _, row := range rows {
		names[row.ID] = imageName{ID: row.ID, Name: row.Name}
	}
	return names, nil
}

// instanceFromRow projects a row onto the API type. The nullable columns become
// pointers so the JSON carries null rather than an empty string, and the tags are
// never nil.
func instanceFromRow(row sqlc.Instance, image imageName, tags map[string]string) Instance {
	out := Instance{
		ID:           row.ID,
		Name:         row.Name,
		State:        State(row.State),
		ImageID:      row.ImageID,
		ImageName:    image.Name,
		InstanceType: row.InstanceType,
		VCPUs:        int(row.Vcpus),
		MemoryMiB:    int(row.MemoryMib),
		RootDiskGB:   int(row.RootDiskGb),
		LaunchedAt:   parseTime(row.LaunchedAt),
		UpdatedAt:    parseTime(row.UpdatedAt),
		Tags:         map[string]string{},
	}

	if row.StateReason.Valid {
		out.StateReason = &row.StateReason.String
	}
	if row.KeyPairID.Valid {
		out.KeyPairID = &row.KeyPairID.String
	}
	if row.KeyPairName.Valid {
		out.KeyPairName = &row.KeyPairName.String
	}
	if row.PrivateIp.Valid {
		out.PrivateIP = &row.PrivateIp.String
	}
	if row.MacAddress.Valid {
		out.MACAddress = &row.MacAddress.String
	}
	if row.TerminatedAt.Valid {
		terminated := parseTime(row.TerminatedAt.String)
		out.TerminatedAt = &terminated
	}

	for key, value := range tags {
		out.Tags[key] = value
	}

	return out
}

// interfacesByInstances reads the live interfaces of several instances at once,
// grouped by instance. An instance with no interface (a terminated one) is
// absent from the map.
func (s *Service) interfacesByInstances(ctx context.Context, instanceIDs []string) (map[string][]Interface, error) {
	unique := make([]string, 0, len(instanceIDs))
	seen := make(map[string]struct{}, len(instanceIDs))
	for _, instanceID := range instanceIDs {
		if _, ok := seen[instanceID]; ok {
			continue
		}
		seen[instanceID] = struct{}{}
		unique = append(unique, instanceID)
	}
	if len(unique) == 0 {
		return map[string][]Interface{}, nil
	}

	rows, err := s.store.ListInterfacesByInstances(ctx, sqlc.ListInterfacesByInstancesParams{
		InstanceIds: unique,
	})
	if err != nil {
		return nil, fmt.Errorf("list the network interfaces of the instances: %w", err)
	}

	out := make(map[string][]Interface, len(unique))
	for _, row := range rows {
		out[row.InstanceID] = append(out[row.InstanceID], Interface{
			ID:          row.ID,
			DeviceIndex: int(row.DeviceIndex),
			MACAddress:  row.MacAddress,
			PrivateIP:   row.PrivateIp,
			SubnetID:    row.SubnetID,
			VPCID:       row.VpcID,
		})
	}
	return out, nil
}

// applyNetwork fills the network placement of an instance from its interfaces:
// the top-level vpc_id and subnet_id are the first interface's, and the array
// itself. An instance without an interface keeps nulls and an empty array.
func applyNetwork(out *Instance, interfaces []Interface) {
	if len(interfaces) == 0 {
		out.NetworkInterfaces = []Interface{}
		return
	}
	out.NetworkInterfaces = interfaces
	out.VPCID = &interfaces[0].VPCID
	out.SubnetID = &interfaces[0].SubnetID
}
