// Tests for the subnet service. They run in the external package: the service
// keeps no package-level state to swap, so an external test is enough and the
// cases can run in parallel.
package subnet_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/subnet"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker/locks"
)

// testClock is a fixed clock, so the created_at/updated_at assertions below are
// exact and pagination tests can advance with a known cadence.
var testClock = func() time.Time {
	return time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
}

// harness is the service under test plus the pieces assertions reach below the
// API surface.
type harness struct {
	svc   *subnet.Service
	store *store.Store
	// locks is the set the service serializes subnet operations with; tests
	// hold a key to make a delete refuse.
	locks *locks.Set
	now   *time.Time
}

func newHarness(t *testing.T) harness {
	t.Helper()

	now := testClock()
	db := storetest.New(t)
	lockSet := locks.New()
	svc, err := subnet.New(subnet.Options{
		Logger: logging.Discard(),
		Store:  db,
		Locks:  lockSet,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("subnet.New(): %v", err)
	}

	return harness{svc: svc, store: db, locks: lockSet, now: &now}
}

// seedVPC writes the parent a subnet needs. The bridge name follows the VPC id,
// so two VPCs never collide on it.
func seedVPC(t *testing.T, h harness, id, name, cidr, gateway string) {
	t.Helper()

	if _, err := h.store.Exec(context.Background(), `
		INSERT INTO vpcs (id, name, cidr, mode, gateway_ip, netmask, bridge_name,
		                 libvirt_network, is_default, created_at, updated_at)
		VALUES (?1, ?2, ?3, 'nat', ?4, '255.255.0.0', ?5, ?1, 0,
		        '2026-01-02T09:00:00Z', '2026-01-02T09:00:00Z')`,
		id, name, cidr, gateway, hypervisor.BridgeName(id)); err != nil {
		t.Fatalf("seed VPC %q: %v", id, err)
	}
}

// insertInterface stands in for a launch: an instance row plus one network
// interface allocated from a subnet.
func insertInterface(t *testing.T, h harness, subnetID string) string {
	t.Helper()

	if _, err := h.store.Exec(context.Background(),
		`INSERT OR IGNORE INTO instance_types (id, vcpus, memory_mib) VALUES ('kumo.small', 1, 2048)`); err != nil {
		t.Fatalf("insert instance type: %v", err)
	}

	imageID := "img-" + strings.Repeat("0", 17)
	if _, err := h.store.Exec(context.Background(),
		`INSERT INTO images (id, name, state, created_at, updated_at)
		 VALUES (?1, ?1, 'available', '2026-01-02T09:00:00Z', '2026-01-02T09:00:00Z')`,
		imageID); err != nil {
		t.Fatalf("insert image: %v", err)
	}

	instanceID := "i-" + strings.Repeat("1", 17)
	if _, err := h.store.Exec(context.Background(),
		`INSERT INTO instances (id, name, state, image_id, instance_type, vcpus, memory_mib,
		   root_disk_gb, mac_address, private_ip, launched_at, updated_at)
		 VALUES (?1, 'web-1', 'running', ?2, 'kumo.small', 1, 2048, 10, '52:54:00:00:00:01', '10.0.0.10',
		   '2026-01-02T09:05:00Z', '2026-01-02T09:05:00Z')`,
		instanceID, imageID); err != nil {
		t.Fatalf("insert instance: %v", err)
	}

	if _, err := h.store.Exec(context.Background(),
		`INSERT INTO network_interfaces
		   (id, instance_id, subnet_id, device_index, mac_address, private_ip, created_at)
		 VALUES ('eni-`+strings.Repeat("1", 17)+`', ?1, ?2, 0, '52:54:00:00:00:01', '10.0.0.10',
		   '2026-01-02T09:05:00Z')`,
		instanceID, subnetID); err != nil {
		t.Fatalf("insert network interface: %v", err)
	}

	return instanceID
}

// assertFieldError wants err to be a validation failure about the given field.
func assertFieldError(t *testing.T, err error, field string) {
	t.Helper()

	var validation *service.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %v, want a *service.ValidationError about %q", err, field)
	}
	for _, item := range validation.Fields {
		if item.Field == field {
			return
		}
	}
	t.Errorf("Fields = %+v, want an error about %q", validation.Fields, field)
}

func TestNewValidatesItsDependencies(t *testing.T) {
	t.Parallel()

	if _, err := subnet.New(subnet.Options{Store: storetest.New(t)}); !errors.Is(err, subnet.ErrNoLogger) {
		t.Errorf("New() error = %v, want ErrNoLogger", err)
	}
	if _, err := subnet.New(subnet.Options{Logger: logging.Discard()}); !errors.Is(err, subnet.ErrNoStore) {
		t.Errorf("New() error = %v, want ErrNoStore", err)
	}
}

func TestCreateRecordsThePartition(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedVPC(t, h, "vpc-00000000000000001", "prod", "10.0.0.0/16", "10.0.0.1")

	created, err := h.svc.Create(context.Background(), subnet.CreateInput{
		VPCID: "vpc-00000000000000001", Name: "app-a", CIDR: "10.0.0.0/24",
		Tags: map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if !strings.HasPrefix(created.ID, "subnet-") || len(created.ID) != 24 {
		t.Errorf("ID = %q, want a subnet- id of 17 hex", created.ID)
	}
	if created.VPCID != "vpc-00000000000000001" || created.Name != "app-a" || created.CIDR != "10.0.0.0/24" {
		t.Errorf("subnet = %q/%q/%q, want the stored vpc, name and cidr",
			created.VPCID, created.Name, created.CIDR)
	}
	if created.IsDefault {
		t.Error("IsDefault = true for a plain create")
	}
	if created.TotalIPs != 256 || created.UsableIPs != 253 || created.AllocatedIPs != 0 {
		t.Errorf("counts = %d/%d/%d, want 256/253/0: the gateway 10.0.0.1 lives inside this /24",
			created.TotalIPs, created.UsableIPs, created.AllocatedIPs)
	}
	if !created.CreatedAt.Equal(testClock()) {
		t.Errorf("CreatedAt = %v, want %v", created.CreatedAt, testClock())
	}
	if created.Tags["env"] != "prod" {
		t.Errorf("Tags = %v, want env=prod", created.Tags)
	}
}

func TestCreateValidates(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedVPC(t, h, "vpc-00000000000000001", "prod", "10.0.0.0/16", "10.0.0.1")

	tooManyTags := make(map[string]string, 51)
	for i := range 51 {
		tooManyTags[fmt.Sprintf("tag-%02d", i)] = ""
	}

	tests := []struct {
		name  string
		input subnet.CreateInput
		want  string
	}{
		{name: "an empty name", input: subnet.CreateInput{VPCID: "vpc-00000000000000001", CIDR: "10.0.0.0/24"}, want: "name"},
		{name: "a name with a space", input: subnet.CreateInput{VPCID: "vpc-00000000000000001", Name: "app a", CIDR: "10.0.0.0/24"}, want: "name"},
		{name: "an uppercase name", input: subnet.CreateInput{VPCID: "vpc-00000000000000001", Name: "App-a", CIDR: "10.0.0.0/24"}, want: "name"},
		{name: "a 64 character name", input: subnet.CreateInput{VPCID: "vpc-00000000000000001", Name: strings.Repeat("a", 64), CIDR: "10.0.0.0/24"}, want: "name"},
		{name: "a cidr outside the parent", input: subnet.CreateInput{VPCID: "vpc-00000000000000001", Name: "k", CIDR: "10.1.0.0/16"}, want: "cidr"},
		{name: "more than fifty tags", input: subnet.CreateInput{VPCID: "vpc-00000000000000001", Name: "k", CIDR: "10.0.0.0/24", Tags: tooManyTags}, want: "tags"},
		{
			name: "an empty tag key",
			input: subnet.CreateInput{VPCID: "vpc-00000000000000001", Name: "k", CIDR: "10.0.0.0/24",
				Tags: map[string]string{"": "v"}},
			want: "tags",
		},
		{
			name: "a 257 character tag value",
			input: subnet.CreateInput{VPCID: "vpc-00000000000000001", Name: "k", CIDR: "10.0.0.0/24",
				Tags: map[string]string{"k": strings.Repeat("v", 257)}},
			want: "tags",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.svc.Create(context.Background(), tt.input)
			assertFieldError(t, err, tt.want)
		})
	}
}

func TestCreateUnknownVPC(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, err := h.svc.Create(context.Background(), subnet.CreateInput{
		VPCID: "vpc-00000000000000000", Name: "app-a", CIDR: "10.0.0.0/24",
	})
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Create() error = %v, want ErrNotFound", err)
	}
}

func TestCreateRejectsAnOverlappingSibling(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedVPC(t, h, "vpc-00000000000000001", "prod", "10.0.0.0/16", "10.0.0.1")

	if _, err := h.svc.Create(context.Background(), subnet.CreateInput{
		VPCID: "vpc-00000000000000001", Name: "app-a", CIDR: "10.0.0.0/24",
	}); err != nil {
		t.Fatalf("first Create() error = %v", err)
	}

	_, err := h.svc.Create(context.Background(), subnet.CreateInput{
		VPCID: "vpc-00000000000000001", Name: "app-b", CIDR: "10.0.0.128/25",
	})
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("overlapping Create() error = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "overlaps") {
		t.Errorf("the conflict message does not say so: %v", err)
	}
}

func TestCreateDuplicateNameScopedToTheVPC(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedVPC(t, h, "vpc-00000000000000001", "prod", "10.0.0.0/16", "10.0.0.1")
	seedVPC(t, h, "vpc-11111111111111112", "staging", "10.1.0.0/16", "10.1.0.1")

	if _, err := h.svc.Create(context.Background(), subnet.CreateInput{
		VPCID: "vpc-00000000000000001", Name: "app-a", CIDR: "10.0.0.0/24",
	}); err != nil {
		t.Fatalf("first Create() error = %v", err)
	}

	// The same name in the same VPC is a conflict...
	_, err := h.svc.Create(context.Background(), subnet.CreateInput{
		VPCID: "vpc-00000000000000001", Name: "app-a", CIDR: "10.0.1.0/24",
	})
	if !errors.Is(err, service.ErrAlreadyExists) {
		t.Fatalf("duplicate Create() error = %v, want ErrAlreadyExists", err)
	}

	// ...and in another VPC it is free.
	sibling, err := h.svc.Create(context.Background(), subnet.CreateInput{
		VPCID: "vpc-11111111111111112", Name: "app-a", CIDR: "10.1.0.0/24",
	})
	if err != nil {
		t.Fatalf("same-name other-VPC Create() error = %v, want success", err)
	}
	if sibling.VPCID != "vpc-11111111111111112" {
		t.Errorf("sibling = %+v, want the second VPC", sibling)
	}
}

func TestCreateCountsUsableAddressesOutsideTheGateway(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedVPC(t, h, "vpc-00000000000000001", "prod", "10.0.0.0/16", "10.0.0.1")

	// 10.0.255.0/24 does not contain the VPC gateway 10.0.0.1, so every address
	// but the network and the broadcast is on sale.
	created, err := h.svc.Create(context.Background(), subnet.CreateInput{
		VPCID: "vpc-00000000000000001", Name: "far", CIDR: "10.0.255.0/24",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.TotalIPs != 256 || created.UsableIPs != 254 {
		t.Errorf("counts = %d/%d, want 256/254 away from the gateway",
			created.TotalIPs, created.UsableIPs)
	}
}

func TestGetReportsAllocations(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedVPC(t, h, "vpc-00000000000000001", "prod", "10.0.0.0/16", "10.0.0.1")

	created, err := h.svc.Create(context.Background(), subnet.CreateInput{
		VPCID: "vpc-00000000000000001", Name: "app-a", CIDR: "10.0.0.0/24",
		Tags: map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := h.svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "app-a" || got.Tags["env"] != "prod" {
		t.Errorf("Get() = %+v, want the stored subnet", got)
	}
	if got.TotalIPs != 256 || got.UsableIPs != 253 || got.AllocatedIPs != 0 {
		t.Errorf("counts = %d/%d/%d, want 256/253/0 before anything is allocated",
			got.TotalIPs, got.UsableIPs, got.AllocatedIPs)
	}

	// An interface allocated from the subnet counts.
	insertInterface(t, h, created.ID)

	got, err = h.svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.AllocatedIPs != 1 {
		t.Errorf("AllocatedIPs = %d, want 1 with one interface inside", got.AllocatedIPs)
	}
}

func TestGetUnknownSubnet(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, err := h.svc.Get(context.Background(), "subnet-00000000000000000")
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestListPagesAcrossVPCs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedVPC(t, h, "vpc-00000000000000001", "prod", "10.0.0.0/16", "10.0.0.1")
	seedVPC(t, h, "vpc-11111111111111112", "other", "10.1.0.0/16", "10.1.0.1")

	// created_at drives the page order, so the cases step the clock.
	for _, tt := range []struct{ vpc, name, cidr string }{
		{"vpc-00000000000000001", "alpha", "10.0.0.0/24"},
		{"vpc-00000000000000001", "bravo", "10.0.1.0/24"},
		{"vpc-11111111111111112", "charlie", "10.1.0.0/24"},
	} {
		*h.now = h.now.Add(time.Second)
		if _, err := h.svc.Create(context.Background(), subnet.CreateInput{
			VPCID: tt.vpc, Name: tt.name, CIDR: tt.cidr,
		}); err != nil {
			t.Fatalf("Create(%q) error = %v", tt.name, err)
		}
	}

	// Newest first by default, one item per page.
	page, err := h.svc.List(context.Background(), subnet.ListInput{PageSize: 1})
	if err != nil {
		t.Fatalf("List() page 1 error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Name != "charlie" {
		t.Fatalf("page 1 = %+v, want charlie alone", page.Items)
	}
	if page.NextPageToken == nil {
		t.Fatal("page 1 has no next_page_token, want one")
	}

	page2, err := h.svc.List(context.Background(), subnet.ListInput{PageSize: 1, PageToken: *page.NextPageToken})
	if err != nil {
		t.Fatalf("List() page 2 error = %v", err)
	}
	if len(page2.Items) != 1 || page2.Items[0].Name != "bravo" {
		t.Fatalf("page 2 = %+v, want bravo alone", page2.Items)
	}

	// Ascending flips the order.
	asc, err := h.svc.List(context.Background(), subnet.ListInput{PageSize: 10, Sort: subnet.SortCreatedAsc})
	if err != nil {
		t.Fatalf("List(asc) error = %v", err)
	}
	if asc.Items[0].Name != "alpha" || asc.Items[2].Name != "charlie" {
		t.Errorf("asc page = %+v, want alpha..charlie", asc.Items)
	}

	// A name substring filters.
	filtered, err := h.svc.List(context.Background(), subnet.ListInput{Query: "brav"})
	if err != nil {
		t.Fatalf("List(q=brav) error = %v", err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].Name != "bravo" {
		t.Errorf("filtered page = %+v, want bravo alone", filtered.Items)
	}

	// The vpc_id filter keeps a listing to its parent.
	own, err := h.svc.List(context.Background(), subnet.ListInput{VpcID: "vpc-00000000000000001"})
	if err != nil {
		t.Fatalf("List(vpc filter) error = %v", err)
	}
	if len(own.Items) != 2 {
		t.Errorf("vpc-filtered page = %+v, want the two subnets of prod", own.Items)
	}
}

func TestListRejectsBadArguments(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	tests := []struct {
		name  string
		input subnet.ListInput
		want  string
	}{
		{name: "an unknown sort", input: subnet.ListInput{Sort: "name:desc"}, want: "sort"},
		{name: "a negative page size", input: subnet.ListInput{PageSize: -1}, want: "page_size"},
		{name: "an oversized page", input: subnet.ListInput{PageSize: 500}, want: "page_size"},
		{name: "a mangled page token", input: subnet.ListInput{PageToken: "not base64 ==="}, want: "page_token"},
		{
			name:  "a page token for a foreign resource",
			input: subnet.ListInput{PageToken: encodePageToken("2026-01-02T10:00:00Z", "vpc-0123456789abcdef0")},
			want:  "page_token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.svc.List(context.Background(), tt.input)
			assertFieldError(t, err, tt.want)
		})
	}
}

// encodePageToken is the same shape the service encodes, so the foreign-token
// case above builds a token the decoder has to reject.
func encodePageToken(createdAt, resourceID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt + "|" + resourceID))
}

func TestUpdateTags(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedVPC(t, h, "vpc-00000000000000001", "prod", "10.0.0.0/16", "10.0.0.1")

	created, err := h.svc.Create(context.Background(), subnet.CreateInput{
		VPCID: "vpc-00000000000000001", Name: "app-a", CIDR: "10.0.0.0/24",
		Tags: map[string]string{"env": "dev"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	updated, err := h.svc.Update(context.Background(), created.ID,
		map[string]string{"env": "prod", "owner": "ada"})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Tags["env"] != "prod" || updated.Tags["owner"] != "ada" {
		t.Errorf("Tags = %v, want the replaced set", updated.Tags)
	}
	if updated.Name != "app-a" || updated.CIDR != "10.0.0.0/24" || updated.VPCID != created.VPCID {
		t.Errorf("Update() moved immutable fields: %+v", updated)
	}

	// An empty map clears the set.
	cleared, err := h.svc.Update(context.Background(), created.ID, map[string]string{})
	if err != nil {
		t.Fatalf("Update({}) error = %v", err)
	}
	if len(cleared.Tags) != 0 {
		t.Errorf("Tags = %v, want empty", cleared.Tags)
	}

	// Unknown subnets and bad tags are their own errors.
	if _, err := h.svc.Update(context.Background(), "subnet-00000000000000000", nil); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Update(unknown) error = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.Update(context.Background(), created.ID, map[string]string{"": "x"}); err == nil {
		t.Error("Update(invalid) error = nil, want a validation failure")
	}
}

func TestDeleteRemovesTheRowAndTheTags(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedVPC(t, h, "vpc-00000000000000001", "prod", "10.0.0.0/16", "10.0.0.1")

	created, err := h.svc.Create(context.Background(), subnet.CreateInput{
		VPCID: "vpc-00000000000000001", Name: "app-a", CIDR: "10.0.0.0/24",
		Tags: map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := h.svc.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if _, err := h.svc.Get(context.Background(), created.ID); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get() after Delete() error = %v, want ErrNotFound", err)
	}
	var tags int
	if err := h.store.DB().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM tags WHERE resource_id = ?", created.ID).Scan(&tags); err != nil {
		t.Fatalf("count tags: %v", err)
	}
	if tags != 0 {
		t.Errorf("the delete left %d tags behind", tags)
	}

	// A second delete reads as a not-found instead of a silent success.
	if err := h.svc.Delete(context.Background(), created.ID); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("second Delete() error = %v, want ErrNotFound", err)
	}
}

func TestDeleteRefusesWhileInterfacesExist(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedVPC(t, h, "vpc-00000000000000001", "prod", "10.0.0.0/16", "10.0.0.1")

	created, err := h.svc.Create(context.Background(), subnet.CreateInput{
		VPCID: "vpc-00000000000000001", Name: "app-a", CIDR: "10.0.0.0/24",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	insertInterface(t, h, created.ID)

	err = h.svc.Delete(context.Background(), created.ID)
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("Delete() error = %v, want ErrConflict while an interface is allocated", err)
	}
	if !strings.Contains(err.Error(), "interface") {
		t.Errorf("the conflict message does not mention the interface: %v", err)
	}

	// The row survives the refusal.
	if _, err := h.svc.Get(context.Background(), created.ID); err != nil {
		t.Errorf("Get() after the refusal error = %v, want the row alive", err)
	}
}

func TestDeleteRefusesWhileTheLockIsHeld(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedVPC(t, h, "vpc-00000000000000001", "prod", "10.0.0.0/16", "10.0.0.1")

	created, err := h.svc.Create(context.Background(), subnet.CreateInput{
		VPCID: "vpc-00000000000000001", Name: "app-a", CIDR: "10.0.0.0/24",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// A launch (M4b.3) holds the key while it allocates; a delete in the middle
	// must refuse and say who is busy.
	release, err := h.locks.TryAcquire(subnet.LockKey(created.ID), "launch i-2")
	if err != nil {
		t.Fatalf("TryAcquire() error = %v", err)
	}
	defer release()

	err = h.svc.Delete(context.Background(), created.ID)
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("Delete() error = %v, want ErrConflict while the lock is held", err)
	}
	if !strings.Contains(err.Error(), "launch i-2") {
		t.Errorf("the refusal does not name the holder: %v", err)
	}
}
