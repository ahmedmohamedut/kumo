// Package vpc owns virtual private clouds: a private address block with its own
// libvirt network and Linux bridge. Creating and deleting a VPC are long
// operations, so the handlers record a row and a task, and the worker defines,
// starts, destroys and undefines the network
// (docs/design/04-libvirt-integration.md §6).
package vpc

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/network"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/subnet"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker/locks"
)

// Mode is the forwarding mode of a VPC's libvirt network. The values mirror the
// CHECK constraint on vpcs.mode (docs/design/02-data-model.md).
type Mode string

// The supported forwarding modes.
const (
	ModeNAT      Mode = "nat"
	ModeIsolated Mode = "isolated"
)

// SortOrder selects the order of a VPC page.
type SortOrder string

// The supported sort orders.
const (
	SortCreatedDesc SortOrder = "created_at:desc"
	SortCreatedAsc  SortOrder = "created_at:asc"
)

// Limits from 01-api.md §VPCs, the tag rules of 02-data-model.md and the
// pagination bounds of api/openapi.yaml.
const (
	maxNameLength = 63
	maxTags       = 50
	maxTagKey     = 128
	maxTagValue   = 256

	// MinPageSize, DefaultPageSize and MaxPageSize bound ListInput.PageSize;
	// zero means "the default".
	MinPageSize     = 1
	DefaultPageSize = 50
	MaxPageSize     = 200

	// defaultVPCCIDR is the range the fresh default VPC gets, the one
	// docs/design/04-libvirt-integration.md §3 documents for the guest network.
	defaultVPCCIDR = "192.168.100.0/24"
)

// namePattern is the VPC name rule of 01-api.md: an RFC-1123 label of 1 to 63
// characters, because the name seeds the DNS domain of the libvirt network.
var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// hostCollision reports the first host route or interface subnet that overlaps
// a CIDR. It is a package variable so tests can force a collision without a
// real host to scan.
var hostCollision = network.HostCollision

// VPC is the safe projection of a vpcs row. Tags are always present.
type VPC struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	CIDR           string            `json:"cidr"`
	Mode           Mode              `json:"mode"`
	GatewayIP      string            `json:"gateway_ip"`
	Netmask        string            `json:"netmask"`
	BridgeName     string            `json:"bridge_name"`
	LibvirtNetwork string            `json:"libvirt_network"`
	IsDefault      bool              `json:"is_default"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	Tags           map[string]string `json:"tags"`
}

// Detail is a VPC plus what lives inside it.
type Detail struct {
	VPC
	SubnetCount   int64 `json:"subnet_count"`
	InstanceCount int64 `json:"instance_count"`
}

// Created is the answer to POST /vpcs: the VPC row plus the task that will
// define and start its network.
type Created struct {
	VPC
	TaskID string `json:"task_id"`
}

// Deleted is the answer to DELETE /vpcs/{id}: the VPC row plus the task that
// will remove the network and the row.
type Deleted struct {
	VPC
	TaskID string `json:"task_id"`
}

// Page is one page of a VPC listing.
type Page struct {
	Items         []VPC   `json:"items"`
	NextPageToken *string `json:"next_page_token"`
}

// ListInput selects a page of VPCs.
type ListInput struct {
	Query     string
	Sort      SortOrder
	PageSize  int
	PageToken string
}

// CreateInput is a POST /vpcs body with the calling user attached.
type CreateInput struct {
	Name      string
	CIDR      string
	Mode      Mode
	Tags      map[string]string
	CreatedBy string
}

// Options configures the service.
type Options struct {
	Logger     *slog.Logger
	Store      *store.Store
	Hypervisor hypervisor.Hypervisor
	Tasks      *task.Service
	// DefaultVPCCIDR is the range a fresh default VPC gets when Bootstrap finds
	// no row and no network to adopt. Empty means the documented guest range
	// (192.168.100.0/24), the same default the config package resolves
	// KUMO_DEFAULT_VPC_CIDR to.
	DefaultVPCCIDR string
	// Subnets creates the default subnet of the default VPC during Bootstrap.
	Subnets *subnet.Service
	// Locks serializes the operations of one VPC. A nil value gets a set of its
	// own.
	Locks *locks.Set
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Sentinel errors of the constructor, so a wiring mistake reads differently
// from a runtime failure.
var (
	ErrNoLogger     = errors.New("vpc: a logger is required")
	ErrNoStore      = errors.New("vpc: a store is required")
	ErrNoHypervisor = errors.New("vpc: a hypervisor is required")
	ErrNoTasks      = errors.New("vpc: a task service is required")
	// ErrBadTaskPayload means a worker task names no VPC, which a creator in
	// this codebase cannot produce; it marks a bug rather than a runtime
	// condition.
	ErrBadTaskPayload = errors.New("vpc: bad task payload")
	// ErrInvalidDefaultCIDR means Options.DefaultVPCCIDR cannot back a network,
	// which fails the constructor instead of the first boot after a fresh
	// database.
	ErrInvalidDefaultCIDR = errors.New("vpc: invalid default VPC CIDR")
)

// Service owns VPCs.
type Service struct {
	logger         *slog.Logger
	store          *store.Store
	hv             hypervisor.Hypervisor
	tasks          *task.Service
	subnets        *subnet.Service
	locks          *locks.Set
	now            func() time.Time
	defaultVPCCIDR string
}

// New builds the service.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Logger == nil:
		return nil, ErrNoLogger
	case opts.Store == nil:
		return nil, ErrNoStore
	case opts.Hypervisor == nil:
		return nil, ErrNoHypervisor
	case opts.Tasks == nil:
		return nil, ErrNoTasks
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Locks == nil {
		opts.Locks = locks.New()
	}
	if opts.DefaultVPCCIDR == "" {
		opts.DefaultVPCCIDR = defaultVPCCIDR
	}
	// The range has to back a real network; refusing it here keeps a bad
	// KUMO_DEFAULT_VPC_CIDR from failing the first boot after a fresh database
	// instead of at config load.
	if _, err := network.ValidateVPCCIDR(opts.DefaultVPCCIDR); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidDefaultCIDR, opts.DefaultVPCCIDR)
	}

	return &Service{
		logger:         opts.Logger,
		store:          opts.Store,
		hv:             opts.Hypervisor,
		tasks:          opts.Tasks,
		subnets:        opts.Subnets,
		locks:          opts.Locks,
		now:            opts.Now,
		defaultVPCCIDR: opts.DefaultVPCCIDR,
	}, nil
}

// LockKey is the key this service serializes the operations of one VPC with. It
// is exported because the lock belongs to the VPC rather than to the service:
// anything else that acts on the same VPC has to take the very same key, or the
// two operations interleave.
func LockKey(vpcID string) string { return "vpc:" + vpcID }

// Create records a VPC request and queues the network task. The response is 202
// with a task id (docs/design/01-api.md §VPCs).
func (s *Service) Create(ctx context.Context, in CreateInput) (Created, error) {
	fields := validateCreate(in)
	if len(fields) > 0 {
		return Created{}, &service.ValidationError{Message: "the VPC is not acceptable", Fields: fields}
	}
	if in.Mode == "" {
		in.Mode = ModeNAT
	}

	prefix, err := network.ValidateVPCCIDR(in.CIDR)
	if err != nil {
		return Created{}, &service.ValidationError{
			Message: "the VPC is not acceptable",
			Fields:  []service.FieldError{{Field: "cidr", Reason: err.Error()}},
		}
	}

	collision, err := hostCollision(prefix)
	if err != nil {
		return Created{}, fmt.Errorf("scan the host routes for overlapping networks: %w", err)
	}
	if collision != "" {
		return Created{}, fmt.Errorf("%w: the VPC CIDR %s overlaps %s",
			service.ErrConflict, prefix, collision)
	}

	var created Created
	err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		existing, err := tx.ListVPCCIDRs(ctx)
		if err != nil {
			return fmt.Errorf("list the existing VPC CIDRs: %w", err)
		}
		for _, other := range existing {
			otherPrefix, parseErr := netip.ParsePrefix(other.Cidr)
			if parseErr != nil {
				continue // rows this package wrote parse
			}
			if prefix.Overlaps(otherPrefix) {
				return fmt.Errorf("%w: the VPC CIDR %s overlaps the VPC %s (%s)",
					service.ErrConflict, prefix, other.ID, other.Cidr)
			}
		}

		vpcID := id.VPC()
		now := formatTime(s.now())
		row, err := tx.CreateVPC(ctx, sqlc.CreateVPCParams{
			ID:             vpcID,
			Name:           in.Name,
			Cidr:           prefix.String(),
			Mode:           string(in.Mode),
			GatewayIp:      network.Gateway(prefix).String(),
			Netmask:        network.Netmask(prefix),
			BridgeName:     hypervisor.BridgeName(vpcID),
			LibvirtNetwork: vpcID,
			IsDefault:      0,
			CreatedBy:      nullable(in.CreatedBy),
			CreatedAt:      now,
			UpdatedAt:      now,
		})
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: a VPC named %q already exists", service.ErrAlreadyExists, in.Name)
			}
			return fmt.Errorf("create the VPC row: %w", err)
		}

		if err := replaceTags(ctx, tx, row.ID, in.Tags); err != nil {
			return err
		}

		task, err := s.tasks.CreateInTx(ctx, tx, task.TypeVPCCreate, row.ID,
			createPayload{VPCID: row.ID})
		if err != nil {
			return err
		}

		created = Created{VPC: vpcFromRow(row, in.Tags), TaskID: task.ID}
		return nil
	})
	if err != nil {
		return Created{}, err
	}

	s.logger.InfoContext(ctx, "vpc: create queued",
		"vpc_id", created.ID, "name", created.Name, "cidr", created.CIDR, "task_id", created.TaskID)

	return created, nil
}

// Get returns one VPC with the counts of what lives inside it.
func (s *Service) Get(ctx context.Context, vpcID string) (Detail, error) {
	row, err := s.store.GetVPCDetail(ctx, sqlc.GetVPCDetailParams{ID: vpcID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Detail{}, fmt.Errorf("%w: no VPC %q", service.ErrNotFound, vpcID)
		}
		return Detail{}, fmt.Errorf("get VPC: %w", err)
	}

	tags, err := s.tagsByResource(ctx, []string{row.ID})
	if err != nil {
		return Detail{}, err
	}

	return Detail{
		VPC:           vpcFromDetailRow(row, tags[row.ID]),
		SubnetCount:   row.SubnetCount,
		InstanceCount: row.InstanceCount,
	}, nil
}

// List returns one page of VPCs.
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

	rows, err := s.listPage(ctx, in, cursor)
	if err != nil {
		return Page{}, fmt.Errorf("list VPCs: %w", err)
	}

	page := Page{Items: make([]VPC, 0, min(len(rows), in.PageSize))}
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
		page.Items = append(page.Items, vpcFromRow(row, tags[row.ID]))
	}

	return page, nil
}

// Update replaces the tag set of a VPC. The name, the CIDR and the mode are
// immutable (docs/design/01-api.md §VPCs).
func (s *Service) Update(ctx context.Context, vpcID string, tags map[string]string) (VPC, error) {
	if field := validateTags(tags); field != nil {
		return VPC{}, &service.ValidationError{
			Message: "the tags are not acceptable",
			Fields:  []service.FieldError{*field},
		}
	}

	var vpc VPC
	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		row, err := tx.GetVPC(ctx, sqlc.GetVPCParams{ID: vpcID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no VPC %q", service.ErrNotFound, vpcID)
			}
			return fmt.Errorf("get VPC: %w", err)
		}

		if err := replaceTags(ctx, tx, row.ID, tags); err != nil {
			return err
		}

		touched, err := tx.TouchVPC(ctx, sqlc.TouchVPCParams{
			ID:        row.ID,
			UpdatedAt: formatTime(s.now()),
		})
		if err != nil {
			return fmt.Errorf("touch VPC: %w", err)
		}

		vpc = vpcFromRow(touched, tags)
		return nil
	})
	if err != nil {
		return VPC{}, err
	}

	return vpc, nil
}

// Delete queues the removal of a VPC. It is refused while subnets (or instances
// allocated from them) still exist: the network, the bridge and the row all
// disappear together, so nothing may be left attached to them.
// (docs/design/01-api.md §VPCs)
func (s *Service) Delete(ctx context.Context, vpcID string) (Deleted, error) {
	var deleted Deleted
	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		row, err := tx.GetVPC(ctx, sqlc.GetVPCParams{ID: vpcID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no VPC %q", service.ErrNotFound, vpcID)
			}
			return fmt.Errorf("get VPC: %w", err)
		}

		// Instances are refused first: they live inside subnets, so naming them
		// is the most precise answer to "why will the VPC not go away". The
		// subnet guard below is the next layer down.
		instances, err := tx.CountVPCInstances(ctx, sqlc.CountVPCInstancesParams{VpcID: row.ID})
		if err != nil {
			return fmt.Errorf("count the instances of VPC %q: %w", row.ID, err)
		}
		if instances > 0 {
			return fmt.Errorf("%w: VPC %s still has %d instance(s) allocated from its subnets",
				service.ErrConflict, row.Name, instances)
		}

		subnets, err := tx.CountVPCSubnets(ctx, sqlc.CountVPCSubnetsParams{VpcID: row.ID})
		if err != nil {
			return fmt.Errorf("count the subnets of VPC %q: %w", row.ID, err)
		}
		if subnets > 0 {
			return fmt.Errorf("%w: VPC %s still has %d subnet(s); delete them first",
				service.ErrConflict, row.Name, subnets)
		}

		tags, err := tx.ListResourceTags(ctx, sqlc.ListResourceTagsParams{ResourceID: row.ID})
		if err != nil {
			return fmt.Errorf("list the tags of VPC %q: %w", row.ID, err)
		}

		task, err := s.tasks.CreateInTx(ctx, tx, task.TypeVPCDelete, row.ID,
			deletePayload{VPCID: row.ID})
		if err != nil {
			return err
		}

		deleted = Deleted{
			VPC:    vpcFromRow(row, tagsToMap(tags)),
			TaskID: task.ID,
		}
		return nil
	})
	if err != nil {
		return Deleted{}, err
	}

	s.logger.InfoContext(ctx, "vpc: delete queued",
		"vpc_id", deleted.ID, "name", deleted.Name, "task_id", deleted.TaskID)

	return deleted, nil
}

// createPayload is the task input of a VPC create. It only names the VPC:
// every other input lives on the row, so SQLite stays the single source of
// truth and a retry cannot act on stale task data.
type createPayload struct {
	VPCID string `json:"vpc_id"`
}

// deletePayload is the task input of a VPC delete.
type deletePayload struct {
	VPCID string `json:"vpc_id"`
}

// listPage runs the asc or desc variant of the listing query and returns at
// most PageSize+1 rows; the extra row is how the caller learns there is a
// next page.
func (s *Service) listPage(ctx context.Context, in ListInput, cursor pageCursor) ([]sqlc.Vpc, error) {
	if in.Sort == SortCreatedAsc {
		return s.store.ListVPCsAsc(ctx, sqlc.ListVPCsAscParams{
			Query:         in.Query,
			CursorCreated: cursor.created,
			CursorID:      cursor.id,
			PageLimit:     int64(in.PageSize) + 1,
		})
	}
	return s.store.ListVPCsDesc(ctx, sqlc.ListVPCsDescParams{
		Query:         in.Query,
		CursorCreated: cursor.created,
		CursorID:      cursor.id,
		PageLimit:     int64(in.PageSize) + 1,
	})
}

// tagsByResource loads the tags of every given resource id in one query.
func (s *Service) tagsByResource(ctx context.Context, ids []string) (map[string]map[string]string, error) {
	grouped := make(map[string]map[string]string, len(ids))
	if len(ids) == 0 {
		return grouped, nil
	}

	rows, err := s.store.ListTagsForResources(ctx, sqlc.ListTagsForResourcesParams{ResourceIds: ids})
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

// replaceTags swaps the whole tag set of a resource. The caller runs it inside
// a transaction.
func replaceTags(ctx context.Context, tx *store.Tx, resourceID string, tags map[string]string) error {
	if err := tx.DeleteResourceTags(ctx, sqlc.DeleteResourceTagsParams{ResourceID: resourceID}); err != nil {
		return fmt.Errorf("delete tags: %w", err)
	}
	for key, value := range tags {
		if err := tx.SetTag(ctx, sqlc.SetTagParams{ResourceID: resourceID, Key: key, Value: value}); err != nil {
			return fmt.Errorf("set tag %q: %w", key, err)
		}
	}
	return nil
}

// tagsToMap turns the ordered tag rows of one resource into the map the API
// promises.
func tagsToMap(rows []sqlc.Tag) map[string]string {
	tags := make(map[string]string, len(rows))
	for _, row := range rows {
		tags[row.Key] = row.Value
	}
	return tags
}

// pageCursor is the position a page token encodes.
type pageCursor struct {
	created string
	id      string
}

// encodePageToken packs the cursor of a row into an opaque token.
func encodePageToken(createdAt, vpcID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt + "|" + vpcID))
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
	created, vpcID, ok := strings.Cut(string(raw), "|")
	if !ok || created == "" || !id.HasResourceShape(id.PrefixVPC, vpcID) {
		return pageCursor{}, invalidPageToken()
	}
	return pageCursor{created: created, id: vpcID}, nil
}

func invalidPageToken() *service.ValidationError {
	return &service.ValidationError{
		Message: "the page token is not acceptable",
		Fields:  []service.FieldError{{Field: "page_token", Reason: "is not a token this API issued"}},
	}
}

// validateCreate checks the fields of a create request.
func validateCreate(in CreateInput) []service.FieldError {
	var fields []service.FieldError
	if in.Name == "" {
		fields = append(fields, service.FieldError{Field: "name", Reason: "must not be empty"})
	} else if !namePattern.MatchString(in.Name) {
		fields = append(fields, service.FieldError{
			Field: "name",
			Reason: fmt.Sprintf(
				"must be 1-%d lowercase letters, digits or '-' (an RFC-1123 label; it seeds the DNS domain)",
				maxNameLength),
		})
	}
	switch in.Mode {
	case "", ModeNAT, ModeIsolated:
	default:
		fields = append(fields, service.FieldError{
			Field: "mode", Reason: `must be "nat" or "isolated"`,
		})
	}
	if field := validateTags(in.Tags); field != nil {
		fields = append(fields, *field)
	}
	return fields
}

// validateTags enforces the tag limits of 02-data-model.md.
func validateTags(tags map[string]string) *service.FieldError {
	if len(tags) > maxTags {
		return &service.FieldError{
			Field: "tags", Reason: fmt.Sprintf("at most %d tags per resource", maxTags),
		}
	}
	for key, value := range tags {
		if key == "" {
			return &service.FieldError{Field: "tags", Reason: "tag keys must not be empty"}
		}
		if len(key) > maxTagKey || len(value) > maxTagValue {
			return &service.FieldError{
				Field: "tags",
				Reason: fmt.Sprintf("tag keys are at most %d and values at most %d characters",
					maxTagKey, maxTagValue),
			}
		}
	}
	return nil
}

// vpcFromRow projects a row onto the API type. A nil tag map becomes empty, so
// the JSON carries {} rather than null.
func vpcFromRow(row sqlc.Vpc, tags map[string]string) VPC {
	if tags == nil {
		tags = map[string]string{}
	}
	return VPC{
		ID:             row.ID,
		Name:           row.Name,
		CIDR:           row.Cidr,
		Mode:           Mode(row.Mode),
		GatewayIP:      row.GatewayIp,
		Netmask:        row.Netmask,
		BridgeName:     row.BridgeName,
		LibvirtNetwork: row.LibvirtNetwork,
		IsDefault:      row.IsDefault != 0,
		CreatedAt:      parseTime(row.CreatedAt),
		UpdatedAt:      parseTime(row.UpdatedAt),
		Tags:           tags,
	}
}

// vpcFromDetailRow projects a VPC detail row, which carries every column of the
// base row.
func vpcFromDetailRow(row sqlc.GetVPCDetailRow, tags map[string]string) VPC {
	return vpcFromRow(sqlc.Vpc{
		ID:             row.ID,
		Name:           row.Name,
		Cidr:           row.Cidr,
		Mode:           row.Mode,
		GatewayIp:      row.GatewayIp,
		Netmask:        row.Netmask,
		BridgeName:     row.BridgeName,
		LibvirtNetwork: row.LibvirtNetwork,
		IsDefault:      row.IsDefault,
		CreatedBy:      row.CreatedBy,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}, tags)
}

// networkSpec derives the libvirt network of a VPC from its row. The row is the
// single source of truth: a retried create task targets the same network, bridge
// and gateway every time.
func networkSpec(row sqlc.Vpc) hypervisor.NetworkSpec {
	return hypervisor.NetworkSpec{
		Name:    row.LibvirtNetwork,
		Bridge:  row.BridgeName,
		CIDR:    row.Cidr,
		Gateway: row.GatewayIp,
		Mode:    hypervisor.NetworkMode(row.Mode),
		Domain:  row.Name,
	}
}

// isUniqueViolation reports a UNIQUE constraint failure. The match is exact
// because a bare "constraint failed" also covers CHECK, NOT NULL and foreign key
// violations, which are not conflicts.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		// A row this package did not write; the zero time is the honest answer.
		return time.Time{}
	}
	return parsed.UTC()
}

func nullable(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}
