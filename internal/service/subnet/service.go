// Package subnet owns subnets: allocation partitions of a VPC's bridge with no
// layer-2 separation of their own, so every guest on the VPC shares the bridge
// and the gateway (docs/design/01-api.md §Subnets). All work here is
// synchronous — a subnet is a row, not an object on the host.
package subnet

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

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/network"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker/locks"
)

// SortOrder selects the order of a subnet page.
type SortOrder string

// The supported sort orders.
const (
	SortCreatedDesc SortOrder = "created_at:desc"
	SortCreatedAsc  SortOrder = "created_at:asc"
)

// Limits from 01-api.md §Subnets, the tag rules of 02-data-model.md and the
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
)

// namePattern is the subnet name rule of 01-api.md: the RFC-1123 label of 1 to
// 63 characters a VPC name is. Subnet names do not seed anything, but the
// console treats the two the same way, and the label rule keeps ids, names and
// DNS in one family.
var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Subnet is the safe projection of a subnets row. The three address counts of
// 01-api.md §Subnets are computed, never stored: the subnet lives entirely
// inside its parent VPC, which owns the gateway, so total and usable follow
// from the CIDR alone and allocated from the network_interfaces rows. Tags are
// always present, even when empty.
type Subnet struct {
	ID           string            `json:"id"`
	VPCID        string            `json:"vpc_id"`
	Name         string            `json:"name"`
	CIDR         string            `json:"cidr"`
	TotalIPs     int64             `json:"total_ips"`
	UsableIPs    int64             `json:"usable_ips"`
	AllocatedIPs int64             `json:"allocated_ips"`
	IsDefault    bool              `json:"is_default"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	Tags         map[string]string `json:"tags"`
}

// Page is one page of a subnet listing.
type Page struct {
	Items         []Subnet `json:"items"`
	NextPageToken *string  `json:"next_page_token"`
}

// ListInput selects a page of subnets.
type ListInput struct {
	Query     string
	VpcID     string
	Sort      SortOrder
	PageSize  int
	PageToken string
}

// CreateInput is a POST /subnets body with the calling user attached.
type CreateInput struct {
	VPCID     string
	Name      string
	CIDR      string
	IsDefault bool
	Tags      map[string]string
	CreatedBy string
}

// Options configures the service.
type Options struct {
	Logger *slog.Logger
	Store  *store.Store
	// Locks serializes the operations of one subnet. A nil value gets a set of
	// its own.
	Locks *locks.Set
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Sentinel errors of the constructor, so a caller can tell a wiring mistake
// from a runtime failure.
var (
	// ErrNoLogger means Options.Logger was nil.
	ErrNoLogger = errors.New("subnet: a logger is required")
	// ErrNoStore means Options.Store was nil.
	ErrNoStore = errors.New("subnet: a store is required")
)

// Service owns subnets.
type Service struct {
	logger *slog.Logger
	store  *store.Store
	locks  *locks.Set
	now    func() time.Time
}

// New builds the service.
func New(opts Options) (*Service, error) {
	if opts.Logger == nil {
		return nil, ErrNoLogger
	}
	if opts.Store == nil {
		return nil, ErrNoStore
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Locks == nil {
		opts.Locks = locks.New()
	}

	return &Service{
		logger: opts.Logger,
		store:  opts.Store,
		locks:  opts.Locks,
		now:    opts.Now,
	}, nil
}

// LockKey is the key this service serializes the operations of one subnet with.
// It is exported because the lock belongs to the subnet rather than to the
// service: the launch (M4b.3) allocates from a subnet while a delete would act
// on the same row, and both have to take the very same key.
func LockKey(subnetID string) string { return "subnet:" + subnetID }

// Create records a subnet. It is synchronous: the row is the whole resource,
// and the transaction that checks the VPC and the siblings is also the one
// that writes, so a launch can never slip into a range that was just
// abandoned (docs/design/01-api.md §Subnets, consistency).
func (s *Service) Create(ctx context.Context, in CreateInput) (Subnet, error) {
	if field := validateName(in.Name); field != nil {
		return Subnet{}, &service.ValidationError{
			Message: "the subnet is not acceptable",
			Fields:  []service.FieldError{*field},
		}
	}
	if field := validateTags(in.Tags); field != nil {
		return Subnet{}, &service.ValidationError{
			Message: "the subnet is not acceptable",
			Fields:  []service.FieldError{*field},
		}
	}

	var subnet Subnet
	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		vpcRow, err := tx.GetVPC(ctx, sqlc.GetVPCParams{ID: in.VPCID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no VPC %q", service.ErrNotFound, in.VPCID)
			}
			return fmt.Errorf("get VPC: %w", err)
		}

		vpcPrefix, err := netip.ParsePrefix(vpcRow.Cidr)
		if err != nil {
			return fmt.Errorf("parse the CIDR of VPC %q: %w", in.VPCID, err)
		}
		prefix, err := network.ValidateSubnetCIDR(in.CIDR, vpcPrefix)
		if err != nil {
			return &service.ValidationError{
				Message: "the subnet is not acceptable",
				Fields:  []service.FieldError{{Field: "cidr", Reason: err.Error()}},
			}
		}

		siblings, err := tx.ListSubnetsForVPC(ctx, sqlc.ListSubnetsForVPCParams{VpcID: vpcRow.ID})
		if err != nil {
			return fmt.Errorf("list the sibling subnets of VPC %q: %w", vpcRow.ID, err)
		}
		for _, sibling := range siblings {
			other, parseErr := netip.ParsePrefix(sibling.Cidr)
			if parseErr != nil {
				continue // rows this package wrote parse
			}
			if prefix.Overlaps(other) {
				return fmt.Errorf("%w: subnet %s (%s) overlaps its sibling subnet %s (%s)",
					service.ErrConflict, in.Name, prefix, sibling.Name, sibling.Cidr)
			}
		}

		now := formatTime(s.now())
		row, err := tx.CreateSubnet(ctx, sqlc.CreateSubnetParams{
			ID:        id.Subnet(),
			VpcID:     vpcRow.ID,
			Name:      in.Name,
			Cidr:      prefix.String(),
			IsDefault: boolToInt(in.IsDefault),
			CreatedBy: nullable(in.CreatedBy),
			CreatedAt: now,
			UpdatedAt: now,
		})
		if err != nil {
			if isUniqueViolation(err) {
				// The row is UNIQUE on (vpc_id, name); the vpc_id came from this
				// exact row, so a violation means the name is taken.
				return fmt.Errorf("%w: a subnet named %q already exists in VPC %q",
					service.ErrAlreadyExists, in.Name, vpcRow.ID)
			}
			return fmt.Errorf("create the subnet row: %w", err)
		}

		if err := replaceTags(ctx, tx, row.ID, in.Tags); err != nil {
			return err
		}

		gateway, err := netip.ParseAddr(vpcRow.GatewayIp)
		if err != nil {
			return fmt.Errorf("parse the gateway of VPC %q: %w", vpcRow.ID, err)
		}
		subnet = subnetFromRow(row, prefix, gateway, 0, in.Tags)
		return nil
	})
	if err != nil {
		return Subnet{}, err
	}

	s.logger.InfoContext(ctx, "subnet: created",
		"subnet_id", subnet.ID, "vpc_id", subnet.VPCID, "name", subnet.Name, "cidr", subnet.CIDR)

	return subnet, nil
}

// Get returns one subnet with the counts of the addresses inside it.
func (s *Service) Get(ctx context.Context, subnetID string) (Subnet, error) {
	row, err := s.store.GetSubnetDetail(ctx, sqlc.GetSubnetDetailParams{ID: subnetID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Subnet{}, fmt.Errorf("%w: no subnet %q", service.ErrNotFound, subnetID)
		}
		return Subnet{}, fmt.Errorf("get subnet: %w", err)
	}

	_, gateway, err := s.parentRange(ctx, row.VpcID)
	if err != nil {
		return Subnet{}, err
	}

	tags, err := s.tagsByResource(ctx, []string{row.ID})
	if err != nil {
		return Subnet{}, err
	}

	return subnetFromDetailRow(row, gateway, tags[row.ID]), nil
}

// List returns one page of subnets. The optional vpc_id filter keeps the
// console's VPC detail page to that one VPC.
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
		return Page{}, fmt.Errorf("list subnets: %w", err)
	}

	page := Page{Items: make([]Subnet, 0, min(len(rows), in.PageSize))}
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

	allocated, err := s.allocatedBySubnet(ctx, ids)
	if err != nil {
		return Page{}, err
	}

	gateways, err := s.gatewaysByVPC(ctx, rows)
	if err != nil {
		return Page{}, err
	}

	for _, row := range rows {
		prefix, parseErr := netip.ParsePrefix(row.Cidr)
		if parseErr != nil {
			continue // rows this package wrote parse
		}
		page.Items = append(page.Items, subnetFromRow(row, prefix, gateways[row.VpcID], allocated[row.ID], tags[row.ID]))
	}

	return page, nil
}

// Update replaces the tag set of a subnet. The name, the CIDR and the parent
// VPC are immutable (docs/design/01-api.md §Subnets).
func (s *Service) Update(ctx context.Context, subnetID string, tags map[string]string) (Subnet, error) {
	if field := validateTags(tags); field != nil {
		return Subnet{}, &service.ValidationError{
			Message: "the tags are not acceptable",
			Fields:  []service.FieldError{*field},
		}
	}

	var subnet Subnet
	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		row, err := tx.GetSubnetDetail(ctx, sqlc.GetSubnetDetailParams{ID: subnetID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no subnet %q", service.ErrNotFound, subnetID)
			}
			return fmt.Errorf("get subnet: %w", err)
		}

		if err := replaceTags(ctx, tx, row.ID, tags); err != nil {
			return err
		}

		touched, err := tx.TouchSubnet(ctx, sqlc.TouchSubnetParams{
			ID:        row.ID,
			UpdatedAt: formatTime(s.now()),
		})
		if err != nil {
			return fmt.Errorf("touch subnet: %w", err)
		}

		vpcRow, err := tx.GetVPC(ctx, sqlc.GetVPCParams{ID: row.VpcID})
		if err != nil {
			return fmt.Errorf("get parent VPC: %w", err)
		}
		gateway, err := netip.ParseAddr(vpcRow.GatewayIp)
		if err != nil {
			return fmt.Errorf("parse the gateway of VPC %q: %w", vpcRow.ID, err)
		}

		prefix, err := netip.ParsePrefix(touched.Cidr)
		if err != nil {
			return fmt.Errorf("parse the CIDR of subnet %q: %w", touched.ID, err)
		}

		subnet = subnetFromRow(touched, prefix, gateway, row.AllocatedIps, tags)
		return nil
	})
	if err != nil {
		return Subnet{}, err
	}

	return subnet, nil
}

// Delete removes a subnet and its tags. It is refused while any network
// interface is still allocated from it: removing the row would orphan the
// instances (docs/design/01-api.md §Subnets). The lock makes a delete refuse
// to run beside a launch that is allocating out of the same subnet.
func (s *Service) Delete(ctx context.Context, subnetID string) error {
	// Read before locking, so an unknown id is a 404 rather than a lock on a
	// key nothing owns.
	if _, err := s.store.GetSubnet(ctx, sqlc.GetSubnetParams{ID: subnetID}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no subnet %q", service.ErrNotFound, subnetID)
		}
		return fmt.Errorf("get subnet: %w", err)
	}

	release, err := s.locks.TryAcquire(LockKey(subnetID), "delete")
	if err != nil {
		holder, held := s.locks.Holder(LockKey(subnetID))
		if !held {
			// Unreachable in practice: a held lock is what made TryAcquire fail.
			// The message still has to read well if it ever happens.
			holder = "another operation"
		}
		return fmt.Errorf("%w: subnet %q is busy with %s", service.ErrConflict, subnetID, holder)
	}
	defer release()

	interfaces, err := s.store.CountInterfacesInSubnet(ctx, sqlc.CountInterfacesInSubnetParams{SubnetID: subnetID})
	if err != nil {
		return fmt.Errorf("count the interfaces in subnet %q: %w", subnetID, err)
	}
	if interfaces > 0 {
		return fmt.Errorf("%w: subnet %s still has %d network interface(s); terminate the instances first",
			service.ErrConflict, subnetID, interfaces)
	}

	// The tags table has no foreign key off subnets, so the tags are removed
	// explicitly in the same transaction as the row.
	err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		if err := tx.DeleteResourceTags(ctx, sqlc.DeleteResourceTagsParams{ResourceID: subnetID}); err != nil {
			return fmt.Errorf("delete subnet tags: %w", err)
		}
		deleted, err := tx.DeleteSubnet(ctx, sqlc.DeleteSubnetParams{ID: subnetID})
		if err != nil {
			return fmt.Errorf("delete subnet: %w", err)
		}
		if deleted == 0 {
			return fmt.Errorf("%w: no subnet %q", service.ErrNotFound, subnetID)
		}
		return nil
	})
	if err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "subnet: deleted", "subnet_id", subnetID)
	return nil
}

// parentRange loads a subnet's parent VPC and parses the two addresses its
// counts depend on: the VPC prefix (to confirm the subnet really lives inside
// it) and the gateway (the one address of the VPC that is never for sale).
func (s *Service) parentRange(ctx context.Context, vpcID string) (netip.Prefix, netip.Addr, error) {
	vpcRow, err := s.store.GetVPC(ctx, sqlc.GetVPCParams{ID: vpcID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return netip.Prefix{}, netip.Addr{}, fmt.Errorf("%w: the parent VPC %q is gone", service.ErrNotFound, vpcID)
		}
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("get parent VPC: %w", err)
	}
	return parseParent(vpcRow)
}

// parseParent splits the prefix and gateway of a VPC row.
func parseParent(vpcRow sqlc.Vpc) (netip.Prefix, netip.Addr, error) {
	prefix, err := netip.ParsePrefix(vpcRow.Cidr)
	if err != nil {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("parse the CIDR of VPC %q: %w", vpcRow.ID, err)
	}
	gateway, err := netip.ParseAddr(vpcRow.GatewayIp)
	if err != nil {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("parse the gateway of VPC %q: %w", vpcRow.ID, err)
	}
	return prefix, gateway, nil
}

// listPage runs the asc or desc variant of the listing query and returns at
// most PageSize+1 rows; the extra row is how the caller learns there is a
// next page.
func (s *Service) listPage(ctx context.Context, in ListInput, cursor pageCursor) ([]sqlc.Subnet, error) {
	if in.Sort == SortCreatedAsc {
		return s.store.ListSubnetsAsc(ctx, sqlc.ListSubnetsAscParams{
			Query:         in.Query,
			VpcID:         in.VpcID,
			CursorCreated: cursor.created,
			CursorID:      cursor.id,
			PageLimit:     int64(in.PageSize) + 1,
		})
	}
	return s.store.ListSubnetsDesc(ctx, sqlc.ListSubnetsDescParams{
		Query:         in.Query,
		VpcID:         in.VpcID,
		CursorCreated: cursor.created,
		CursorID:      cursor.id,
		PageLimit:     int64(in.PageSize) + 1,
	})
}

// allocatedBySubnet loads the number of allocated addresses of every given
// subnet id in one grouped query.
func (s *Service) allocatedBySubnet(ctx context.Context, ids []string) (map[string]int64, error) {
	allocated := make(map[string]int64, len(ids))
	if len(ids) == 0 {
		return allocated, nil
	}

	rows, err := s.store.CountSubnetAllocations(ctx, sqlc.CountSubnetAllocationsParams{SubnetIds: ids})
	if err != nil {
		return nil, fmt.Errorf("count subnet allocations: %w", err)
	}
	for _, row := range rows {
		allocated[row.SubnetID] = row.Allocated
	}
	return allocated, nil
}

// gatewaysByVPC loads the gateway of every parent VPC the rows touch, so the
// listing can subtract the one reserved address from each usable count.
func (s *Service) gatewaysByVPC(ctx context.Context, rows []sqlc.Subnet) (map[string]netip.Addr, error) {
	gateways := make(map[string]netip.Addr)
	seen := make(map[string]bool)
	var vpcIDs []string
	for _, row := range rows {
		if !seen[row.VpcID] {
			seen[row.VpcID] = true
			vpcIDs = append(vpcIDs, row.VpcID)
		}
	}
	if len(vpcIDs) == 0 {
		return gateways, nil
	}

	vpcRows, err := s.store.ListVPCsByIDs(ctx, sqlc.ListVPCsByIDsParams{Ids: vpcIDs})
	if err != nil {
		return nil, fmt.Errorf("list parent VPCs: %w", err)
	}
	for _, vpcRow := range vpcRows {
		gateway, err := netip.ParseAddr(vpcRow.GatewayIp)
		if err != nil {
			return nil, fmt.Errorf("parse the gateway of VPC %q: %w", vpcRow.ID, err)
		}
		gateways[vpcRow.ID] = gateway
	}
	return gateways, nil
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

// pageCursor is the position a page token encodes.
type pageCursor struct {
	created string
	id      string
}

// encodePageToken packs the cursor of a row into an opaque token.
func encodePageToken(createdAt, subnetID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt + "|" + subnetID))
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
	created, subnetID, ok := strings.Cut(string(raw), "|")
	if !ok || created == "" || !id.HasResourceShape(id.PrefixSubnet, subnetID) {
		return pageCursor{}, invalidPageToken()
	}
	return pageCursor{created: created, id: subnetID}, nil
}

func invalidPageToken() *service.ValidationError {
	return &service.ValidationError{
		Message: "the page token is not acceptable",
		Fields:  []service.FieldError{{Field: "page_token", Reason: "is not a token this API issued"}},
	}
}

// validateName checks the subnet name rule of 01-api.md.
func validateName(name string) *service.FieldError {
	if name == "" {
		return &service.FieldError{Field: "name", Reason: "must not be empty"}
	}
	if !namePattern.MatchString(name) {
		return &service.FieldError{
			Field: "name",
			Reason: fmt.Sprintf(
				"must be 1-%d lowercase letters, digits or '-' (an RFC-1123 label)", maxNameLength),
		}
	}
	return nil
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

// subnetFromRow projects a row onto the API type, computing the address counts
// from the subnet's own prefix and the parent VPC's gateway.
func subnetFromRow(row sqlc.Subnet, prefix netip.Prefix, gateway netip.Addr, allocated int64, tags map[string]string) Subnet {
	if tags == nil {
		tags = map[string]string{}
	}
	return Subnet{
		ID:           row.ID,
		VPCID:        row.VpcID,
		Name:         row.Name,
		CIDR:         row.Cidr,
		TotalIPs:     network.TotalAddresses(prefix),
		UsableIPs:    network.UsableCount(prefix, gateway),
		AllocatedIPs: allocated,
		IsDefault:    row.IsDefault != 0,
		CreatedAt:    parseTime(row.CreatedAt),
		UpdatedAt:    parseTime(row.UpdatedAt),
		Tags:         tags,
	}
}

// subnetFromDetailRow projects a subnet detail row, which carries every column
// of the base row plus the allocated count.
func subnetFromDetailRow(row sqlc.GetSubnetDetailRow, gateway netip.Addr, tags map[string]string) Subnet {
	return subnetFromRow(sqlc.Subnet{
		ID:        row.ID,
		VpcID:     row.VpcID,
		Name:      row.Name,
		Cidr:      row.Cidr,
		IsDefault: row.IsDefault,
		CreatedBy: row.CreatedBy,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, prefixOf(row.Cidr), gateway, row.AllocatedIps, tags)
}

// prefixOf parses a stored CIDR. Every row passes through ValidateSubnetCIDR or
// the bootstrap half-split before it is written, so a failure means a row some
// other code wrote; the zero prefix keeps the response shaped, the counts go
// to zero.
func prefixOf(cidr string) netip.Prefix {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return netip.Prefix{}
	}
	return prefix
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

func boolToInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}
