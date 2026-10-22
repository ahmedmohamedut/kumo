// Tests for the VPC service. They live inside the package because the tests
// swap the hostCollision package variable, which an external test cannot
// reach. For the same reason none of these tests run in parallel: each one
// overrides the variable and restores it on cleanup, so serial execution is
// what keeps the swaps from stepping on each other.
package vpc

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/subnet"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
)

// testClock is a fixed clock, so the created_at/updated_at assertions below
// are exact and pagination tests can advance with a known cadence.
var testClock = func() time.Time {
	return time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
}

// errRouteReadFailure is what the faked host scan returns when reading the
// routing table fails. Package-level, so the wrap assertions stay exact.
var errRouteReadFailure = errors.New("no /proc/net/route")

// The fixture VPC every test creates. One name for the resource and one for
// the env tag keeps the repeated test strings in a single constant.
const (
	testVPCName = "prod"
	testEnvTag  = "env"
)

// harness is the service under test plus the pieces tests assert on below the
// API surface.
type harness struct {
	svc   *Service
	store *store.Store
	hv    *fake.Fake
	tasks *task.Service
	// sub is the default-subnet service the workers and Bootstrap need; pass
	// nil to the constructor directly for the cases that leave it out.
	sub *subnet.Service
	now *time.Time
}

func newHarness(t *testing.T) harness {
	t.Helper()

	// A fixed, collision-free answer keeps every CIDR from depending on the
	// host the test happens to run on.
	overrideHostCollision(t, func(netip.Prefix) (string, error) { return "", nil })

	now := testClock()
	db := storetest.New(t)
	hv := fake.New(fake.Config{})
	sub, err := subnet.New(subnet.Options{Logger: logging.Discard(), Store: db})
	if err != nil {
		t.Fatalf("subnet.New(): %v", err)
	}
	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}
	svc, err := New(Options{
		Logger:     logging.Discard(),
		Store:      db,
		Hypervisor: hv,
		Tasks:      tasks,
		Subnets:    sub,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("vpc.New(): %v", err)
	}

	return harness{svc: svc, store: db, hv: hv, tasks: tasks, sub: sub, now: &now}
}

// advance steps the harness clock forward one second, so the keyset pages that
// order by created_at do not collapse rows into a tie.
func (h harness) advance() { *h.now = h.now.Add(time.Second) }

// overrideHostCollision swaps the package's host-collision probe and restores
// it when the test finishes. The package's tests run sequentially, which keeps
// the swap safe.
func overrideHostCollision(t *testing.T, fn func(netip.Prefix) (string, error)) {
	t.Helper()

	old := hostCollision
	hostCollision = fn
	t.Cleanup(func() { hostCollision = old })
}

// insertInterface stands in for the M4b.3 launch: an instance row plus one
// network interface allocated from a subnet. It is the shape CountVPCInstances
// and CountInterfacesInSubnet count.
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
	// Not parallel: the package tests swap hostCollision, so they run
	// sequentially (see the package comment).
	if _, err := New(Options{Store: storetest.New(t)}); !errors.Is(err, ErrNoLogger) {
		t.Errorf("New() error = %v, want ErrNoLogger", err)
	}
	if _, err := New(Options{Logger: logging.Discard()}); !errors.Is(err, ErrNoStore) {
		t.Errorf("New() error = %v, want ErrNoStore", err)
	}
	if _, err := New(Options{Logger: logging.Discard(), Store: storetest.New(t)}); !errors.Is(err, ErrNoHypervisor) {
		t.Errorf("New() error = %v, want ErrNoHypervisor", err)
	}
	if _, err := New(Options{
		Logger: logging.Discard(), Store: storetest.New(t), Hypervisor: fake.New(fake.Config{}),
	}); !errors.Is(err, ErrNoTasks) {
		t.Errorf("New() error = %v, want ErrNoTasks", err)
	}

	tasks, err := task.New(task.Options{Store: storetest.New(t)})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}
	// A default CIDR that could not back a network fails the constructor, so a
	// bad KUMO_DEFAULT_VPC_CIDR surfaces at boot before the first launch, not
	// when a fresh database reaches Bootstrap.
	if _, err := New(Options{
		Logger: logging.Discard(), Store: storetest.New(t), Hypervisor: fake.New(fake.Config{}),
		Tasks: tasks, DefaultVPCCIDR: "8.8.8.0/24",
	}); !errors.Is(err, ErrInvalidDefaultCIDR) {
		t.Errorf("New() error = %v, want ErrInvalidDefaultCIDR", err)
	}
}

func TestCreateQueuesTheNetworkTask(t *testing.T) {
	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), CreateInput{
		Name: testVPCName, CIDR: "10.10.0.0/16",
		Tags: map[string]string{testEnvTag: testVPCName},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if !strings.HasPrefix(created.ID, "vpc-") || len(created.ID) != 21 {
		t.Errorf("ID = %q, want a vpc- id of 17 hex", created.ID)
	}
	if created.Name != testVPCName || created.CIDR != "10.10.0.0/16" || created.Mode != ModeNAT {
		t.Errorf("created = %q %q %q, want the stored name, cidr and nat", created.Name, created.CIDR, created.Mode)
	}
	if created.GatewayIP != "10.10.0.1" || created.Netmask != "255.255.0.0" {
		t.Errorf("gateway/netmask = %q/%q, want 10.10.0.1/255.255.0.0", created.GatewayIP, created.Netmask)
	}
	if wanted := hypervisor.BridgeName(created.ID); created.BridgeName != wanted {
		t.Errorf("BridgeName = %q, want %q", created.BridgeName, wanted)
	}
	if created.LibvirtNetwork != created.ID {
		t.Errorf("LibvirtNetwork = %q, want the VPC id as the network name", created.LibvirtNetwork)
	}
	if !created.IsDefault {
		t.Error("IsDefault = false for the first VPC on a node with no default")
	}
	if !created.CreatedAt.Equal(testClock()) {
		t.Errorf("CreatedAt = %v, want %v", created.CreatedAt, testClock())
	}
	if created.Tags[testEnvTag] != testVPCName {
		t.Errorf("Tags = %v, want %s=%s", created.Tags, testEnvTag, testVPCName)
	}

	// The task names the VPC and is still pending: the network work happens in
	// the worker, never in the request.
	queued, err := h.tasks.Get(context.Background(), created.TaskID)
	if err != nil {
		t.Fatalf("task.Get() error = %v", err)
	}
	if queued.Type != task.TypeVPCCreate || queued.State != task.StatePending {
		t.Errorf("task = %q %q, want a pending %q task", queued.Type, queued.State, task.TypeVPCCreate)
	}
	if queued.ResourceID == nil || *queued.ResourceID != created.ID {
		t.Errorf("task resource_id = %v, want %q", queued.ResourceID, created.ID)
	}
	if len(h.hv.Networks()) != 0 {
		t.Errorf("the create defined %d networks already; the worker owns that", len(h.hv.Networks()))
	}
}

func TestCreateDefaultsToNAT(t *testing.T) {
	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), CreateInput{
		Name: "quiet", CIDR: "10.20.0.0/16",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.Mode != ModeNAT {
		t.Errorf("Mode = %q, want the nat default", created.Mode)
	}
}

func TestCreateValidates(t *testing.T) {
	h := newHarness(t)

	tooManyTags := make(map[string]string, 51)
	for i := range 51 {
		tooManyTags[fmt.Sprintf("tag-%02d", i)] = ""
	}

	tests := []struct {
		name  string
		input CreateInput
		want  string
	}{
		{name: "an empty name", input: CreateInput{CIDR: "10.0.0.0/16"}, want: "name"},
		{name: "a name with a space", input: CreateInput{Name: "web vpc", CIDR: "10.0.0.0/16"}, want: "name"},
		{name: "an uppercase name", input: CreateInput{Name: "Prod", CIDR: "10.0.0.0/16"}, want: "name"},
		{name: "a 64 character name", input: CreateInput{Name: strings.Repeat("a", 64), CIDR: "10.0.0.0/16"}, want: "name"},
		{name: "an unknown mode", input: CreateInput{Name: "k", CIDR: "10.0.0.0/16", Mode: "bridged"}, want: "mode"},
		{name: "a cidr that is not private space", input: CreateInput{Name: "k", CIDR: "8.8.8.0/24"}, want: "cidr"},
		{name: "a cidr with host bits set", input: CreateInput{Name: "k", CIDR: "10.0.0.1/16"}, want: "cidr"},
		{name: "an empty cidr", input: CreateInput{Name: "k"}, want: "cidr"},
		{name: "more than fifty tags", input: CreateInput{Name: "k", CIDR: "10.0.0.0/16", Tags: tooManyTags}, want: "tags"},
		{
			name:  "an empty tag key",
			input: CreateInput{Name: "k", CIDR: "10.0.0.0/16", Tags: map[string]string{"": "v"}},
			want:  "tags",
		},
		{
			name: "a 129 character tag key",
			input: CreateInput{
				Name: "k", CIDR: "10.0.0.0/16",
				Tags: map[string]string{strings.Repeat("k", 129): "v"},
			},
			want: "tags",
		},
		{
			name: "a 257 character tag value",
			input: CreateInput{
				Name: "k", CIDR: "10.0.0.0/16",
				Tags: map[string]string{"k": strings.Repeat("v", 257)},
			},
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

func TestCreateRejectsAnOverlappingVPC(t *testing.T) {
	h := newHarness(t)

	if _, err := h.svc.Create(context.Background(), CreateInput{Name: "alpha", CIDR: "10.0.0.0/16"}); err != nil {
		t.Fatalf("first Create() error = %v", err)
	}

	_, err := h.svc.Create(context.Background(), CreateInput{Name: "beta", CIDR: "10.0.128.0/24"})
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("overlapping Create() error = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "10.0.128.0/24") {
		t.Errorf("the conflict message does not name the CIDR: %v", err)
	}
}

func TestCreateRejectsADuplicateName(t *testing.T) {
	h := newHarness(t)

	if _, err := h.svc.Create(context.Background(), CreateInput{Name: testVPCName, CIDR: "10.0.0.0/16"}); err != nil {
		t.Fatalf("first Create() error = %v", err)
	}

	_, err := h.svc.Create(context.Background(), CreateInput{Name: testVPCName, CIDR: "10.1.0.0/16"})
	if !errors.Is(err, service.ErrAlreadyExists) {
		t.Fatalf("duplicate Create() error = %v, want ErrAlreadyExists", err)
	}
}

func TestCreateRejectsAHostCollision(t *testing.T) {
	h := newHarness(t)
	overrideHostCollision(t, func(netip.Prefix) (string, error) {
		return "the host route 172.16.0.0/12 via eth0", nil
	})

	_, err := h.svc.Create(context.Background(), CreateInput{Name: testVPCName, CIDR: "10.0.0.0/16"})
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("Create() error = %v, want ErrConflict on a host collision", err)
	}
	if !strings.Contains(err.Error(), "eth0") {
		t.Errorf("the collision message does not name the host route: %v", err)
	}
}

func TestCreateSurfacesHostScanErrors(t *testing.T) {
	h := newHarness(t)
	overrideHostCollision(t, func(netip.Prefix) (string, error) {
		return "", errRouteReadFailure
	})

	_, err := h.svc.Create(context.Background(), CreateInput{Name: testVPCName, CIDR: "10.0.0.0/16"})
	if err == nil || !strings.Contains(err.Error(), "scan the host routes") {
		t.Fatalf("Create() error = %v, want a wrapped host scan failure", err)
	}
	if errors.Is(err, service.ErrConflict) {
		t.Error("a scan failure must not read as a conflict")
	}
}

func TestGetReportsWhatLivesInside(t *testing.T) {
	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), CreateInput{
		Name: testVPCName, CIDR: "10.0.0.0/16",
		Tags: map[string]string{testEnvTag: testVPCName},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	detail, err := h.svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if detail.Name != testVPCName || detail.Tags[testEnvTag] != testVPCName {
		t.Errorf("Get() = %+v, want the stored VPC", detail)
	}
	if detail.SubnetCount != 0 || detail.InstanceCount != 0 {
		t.Errorf("counts = %d/%d, want 0/0 before anything is launched",
			detail.SubnetCount, detail.InstanceCount)
	}

	// A subnet inside the VPC shows up in the count.
	sub, err := h.sub.Create(context.Background(), subnet.CreateInput{
		VPCID: created.ID, Name: "app-a", CIDR: "10.0.0.0/24",
	})
	if err != nil {
		t.Fatalf("subnet.Create() error = %v", err)
	}

	detail, err = h.svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if detail.SubnetCount != 1 || detail.InstanceCount != 0 {
		t.Errorf("counts = %d/%d, want 1/0 with one subnet", detail.SubnetCount, detail.InstanceCount)
	}

	// An instance allocated from that subnet counts too.
	insertInterface(t, h, sub.ID)

	detail, err = h.svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if detail.SubnetCount != 1 || detail.InstanceCount != 1 {
		t.Errorf("counts = %d/%d, want 1/1 with one instance inside", detail.SubnetCount, detail.InstanceCount)
	}
}

func TestGetUnknownVPC(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.Get(context.Background(), "vpc-00000000000000000")
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestListPagesInCreationOrder(t *testing.T) {
	h := newHarness(t)

	for _, tt := range []struct{ name, cidr string }{
		{"alpha", "10.0.0.0/16"},
		{"bravo", "10.1.0.0/16"},
		{"charlie", "10.2.0.0/16"},
	} {
		h.advance() // created_at drives the page order, so space the VPCs out
		if _, err := h.svc.Create(context.Background(), CreateInput{Name: tt.name, CIDR: tt.cidr}); err != nil {
			t.Fatalf("Create(%q) error = %v", tt.name, err)
		}
	}

	// Newest first by default, one item per page.
	page, err := h.svc.List(context.Background(), ListInput{PageSize: 1})
	if err != nil {
		t.Fatalf("List() page 1 error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Name != "charlie" {
		t.Fatalf("page 1 = %+v, want charlie alone", page.Items)
	}
	if page.NextPageToken == nil {
		t.Fatal("page 1 has no next_page_token, want one")
	}

	page2, err := h.svc.List(context.Background(), ListInput{PageSize: 1, PageToken: *page.NextPageToken})
	if err != nil {
		t.Fatalf("List() page 2 error = %v", err)
	}
	if len(page2.Items) != 1 || page2.Items[0].Name != "bravo" {
		t.Fatalf("page 2 = %+v, want bravo alone", page2.Items)
	}

	page3, err := h.svc.List(context.Background(), ListInput{PageSize: 1, PageToken: *page2.NextPageToken})
	if err != nil {
		t.Fatalf("List() page 3 error = %v", err)
	}
	if len(page3.Items) != 1 || page3.Items[0].Name != "alpha" {
		t.Fatalf("page 3 = %+v, want alpha alone", page3.Items)
	}
	if page3.NextPageToken != nil {
		t.Errorf("page 3 next_page_token = %q, want none on the last page", *page3.NextPageToken)
	}

	// Ascending order flips the page order.
	asc, err := h.svc.List(context.Background(), ListInput{PageSize: 2, Sort: SortCreatedAsc})
	if err != nil {
		t.Fatalf("List(asc) error = %v", err)
	}
	if asc.Items[0].Name != "alpha" || asc.Items[1].Name != "bravo" {
		t.Errorf("asc page = %+v, want alpha then bravo", asc.Items)
	}

	// A name substring filters the listing.
	filtered, err := h.svc.List(context.Background(), ListInput{Query: "char"})
	if err != nil {
		t.Fatalf("List(q=char) error = %v", err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].Name != "charlie" {
		t.Errorf("filtered page = %+v, want charlie alone", filtered.Items)
	}
}

func TestListRejectsBadArguments(t *testing.T) {
	h := newHarness(t)

	tests := []struct {
		name  string
		input ListInput
		want  string
	}{
		{name: "an unknown sort", input: ListInput{Sort: "name:desc"}, want: "sort"},
		{name: "a negative page size", input: ListInput{PageSize: -1}, want: "page_size"},
		{name: "an oversized page", input: ListInput{PageSize: 500}, want: "page_size"},
		{name: "a mangled page token", input: ListInput{PageToken: "not base64 ==="}, want: "page_token"},
		{
			name:  "a page token for a foreign resource",
			input: ListInput{PageToken: encodePageToken("2026-01-02T10:00:00Z", "i-0123456789abcdef0")},
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

func TestUpdateTags(t *testing.T) {
	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), CreateInput{
		Name: testVPCName, CIDR: "10.0.0.0/16",
		Tags: map[string]string{testEnvTag: "dev"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	updated, err := h.svc.Update(context.Background(), created.ID,
		map[string]string{testEnvTag: testVPCName, "owner": "ada"})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Tags[testEnvTag] != testVPCName || updated.Tags["owner"] != "ada" {
		t.Errorf("Tags = %v, want the replaced set", updated.Tags)
	}
	if updated.Name != testVPCName || updated.CIDR != "10.0.0.0/16" {
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

	// Unknown VPCs and bad tags are their own errors.
	if _, err := h.svc.Update(context.Background(), "vpc-00000000000000000", nil); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Update(unknown) error = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.Update(context.Background(), created.ID, map[string]string{"": "x"}); err == nil {
		t.Error("Update(invalid) error = nil, want a validation failure")
	}
}

func TestDeleteQueuesTheTeardownTask(t *testing.T) {
	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), CreateInput{
		Name: testVPCName, CIDR: "10.0.0.0/16",
		Tags: map[string]string{testEnvTag: testVPCName},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	deleted, err := h.svc.Delete(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if deleted.ID != created.ID || deleted.Tags[testEnvTag] != testVPCName {
		t.Errorf("Deleted = %+v, want the VPC with its tags", deleted)
	}
	if !strings.HasPrefix(deleted.TaskID, "task-") {
		t.Errorf("TaskID = %q, want a task- prefixed id", deleted.TaskID)
	}

	queued, err := h.tasks.Get(context.Background(), deleted.TaskID)
	if err != nil {
		t.Fatalf("task.Get() error = %v", err)
	}
	if queued.Type != task.TypeVPCDelete || queued.State != task.StatePending {
		t.Errorf("task = %q %q, want a pending %q task", queued.Type, queued.State, task.TypeVPCDelete)
	}

	// The row survives until the worker runs; the request only queued the work.
	if _, err := h.svc.Get(context.Background(), created.ID); err != nil {
		t.Errorf("Get() after Delete() error = %v, want the row until the task runs", err)
	}
}

func TestDeleteUnknownVPC(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.Delete(context.Background(), "vpc-00000000000000000")
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Delete() error = %v, want ErrNotFound", err)
	}
}

func TestDeleteRefusesWhileSubnetsExist(t *testing.T) {
	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), CreateInput{Name: testVPCName, CIDR: "10.0.0.0/16"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := h.sub.Create(context.Background(), subnet.CreateInput{
		VPCID: created.ID, Name: "app-a", CIDR: "10.0.0.0/24",
	}); err != nil {
		t.Fatalf("subnet.Create() error = %v", err)
	}

	_, err = h.svc.Delete(context.Background(), created.ID)
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("Delete() error = %v, want ErrConflict while a subnet exists", err)
	}
	if !strings.Contains(err.Error(), "subnet") {
		t.Errorf("the conflict message does not mention the subnet: %v", err)
	}
}

func TestDeleteRefusesWhileInstancesExist(t *testing.T) {
	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), CreateInput{Name: testVPCName, CIDR: "10.0.0.0/16"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	sub, err := h.sub.Create(context.Background(), subnet.CreateInput{
		VPCID: created.ID, Name: "app-a", CIDR: "10.0.0.0/24",
	})
	if err != nil {
		t.Fatalf("subnet.Create() error = %v", err)
	}
	insertInterface(t, h, sub.ID)

	// Instances are named first of all: an instance implies a subnet, so the
	// message that says so is the most precise refusal.
	_, err = h.svc.Delete(context.Background(), created.ID)
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("Delete() error = %v, want ErrConflict while an instance is allocated", err)
	}
	if !strings.Contains(err.Error(), "instance") {
		t.Errorf("the conflict message does not mention the instance: %v", err)
	}
}

func TestCreateMakesTheFirstVPCTheDefaultOnly(t *testing.T) {
	h := newHarness(t)

	first, err := h.svc.Create(context.Background(), CreateInput{Name: "first", CIDR: "10.1.0.0/16"})
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	second, err := h.svc.Create(context.Background(), CreateInput{Name: "second", CIDR: "10.2.0.0/16"})
	if err != nil {
		t.Fatalf("second Create() error = %v", err)
	}
	if !first.IsDefault {
		t.Error("the first VPC on a node with no default is not the default")
	}
	if second.IsDefault {
		t.Error("a second VPC took the default from the first")
	}
}
