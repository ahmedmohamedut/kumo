package instancetype_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
)

// wantPresets is the seed set of docs/design/01-api.md §Instance types, smallest
// first.
var wantPresets = []instancetype.Type{
	{ID: "kumo.nano", VCPUs: 1, MemoryMiB: 512, Description: "1 vCPU, 512 MiB"},
	{ID: "kumo.micro", VCPUs: 1, MemoryMiB: 1024, Description: "1 vCPU, 1 GiB"},
	{ID: "kumo.small", VCPUs: 1, MemoryMiB: 2048, Description: "1 vCPU, 2 GiB"},
	{ID: "kumo.medium", VCPUs: 2, MemoryMiB: 4096, Description: "2 vCPU, 4 GiB"},
	{ID: "kumo.large", VCPUs: 2, MemoryMiB: 8192, Description: "2 vCPU, 8 GiB"},
	{ID: "kumo.xlarge", VCPUs: 4, MemoryMiB: 16384, Description: "4 vCPU, 16 GiB"},
	{ID: "kumo.2xlarge", VCPUs: 8, MemoryMiB: 32768, Description: "8 vCPU, 32 GiB"},
}

func TestMigrationSeedsTheDocumentedPresets(t *testing.T) {
	t.Parallel()

	svc := newService(t)

	got, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if !slices.Equal(got, wantPresets) {
		t.Errorf("List() on a fresh database = %+v, want %+v", got, wantPresets)
	}
}

func TestMigrationsDoNotResurrectADeletedPreset(t *testing.T) {
	t.Parallel()

	svc, db := newServiceWithStore(t)
	ctx := context.Background()

	if err := svc.Delete(ctx, "kumo.nano"); err != nil {
		t.Fatalf("Delete(kumo.nano) error = %v", err)
	}
	// A restart runs the migrations again; the seed must not come back.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	if _, err := svc.Get(ctx, "kumo.nano"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get(kumo.nano) after a restart error = %v, want service.ErrNotFound", err)
	}
}

func TestGetReportsAnUnknownTypeAsNotFound(t *testing.T) {
	t.Parallel()

	svc := newService(t)

	_, err := svc.Get(context.Background(), "kumo.enormous")
	if !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get(kumo.enormous) error = %v, want service.ErrNotFound", err)
	}
}

func TestCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   instancetype.CreateInput
		want instancetype.Type
	}{
		{
			name: "generates a description in GiB",
			in:   instancetype.CreateInput{ID: "db.large", VCPUs: 4, MemoryMiB: 8192},
			want: instancetype.Type{ID: "db.large", VCPUs: 4, MemoryMiB: 8192, Description: "4 vCPU, 8 GiB"},
		},
		{
			name: "keeps MiB for a size that is not whole GiB",
			in:   instancetype.CreateInput{ID: "odd", VCPUs: 1, MemoryMiB: 1536},
			want: instancetype.Type{ID: "odd", VCPUs: 1, MemoryMiB: 1536, Description: "1 vCPU, 1536 MiB"},
		},
		{
			name: "keeps a given description",
			in:   instancetype.CreateInput{ID: "ci-runner", VCPUs: 8, MemoryMiB: 16384, Description: "CI runners"},
			want: instancetype.Type{ID: "ci-runner", VCPUs: 8, MemoryMiB: 16384, Description: "CI runners"},
		},
		{
			name: "accepts the bounds",
			in:   instancetype.CreateInput{ID: "edge_1", VCPUs: instancetype.MaxVCPUs, MemoryMiB: instancetype.MinMemoryMiB},
			want: instancetype.Type{ID: "edge_1", VCPUs: 256, MemoryMiB: 256, Description: "256 vCPU, 256 MiB"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := newService(t)
			ctx := context.Background()

			got, err := svc.Create(ctx, tt.in)
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Create() = %+v, want %+v", got, tt.want)
			}

			stored, err := svc.Get(ctx, tt.in.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if stored != tt.want {
				t.Errorf("Get() = %+v, want %+v", stored, tt.want)
			}
		})
	}
}

func TestCreateRefusesInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        instancetype.CreateInput
		wantField []string
	}{
		{"empty id", instancetype.CreateInput{VCPUs: 1, MemoryMiB: 512}, []string{"id"}},
		{"uppercase id", instancetype.CreateInput{ID: "Kumo.Small", VCPUs: 1, MemoryMiB: 512}, []string{"id"}},
		{"id starting with a dot", instancetype.CreateInput{ID: ".hidden", VCPUs: 1, MemoryMiB: 512}, []string{"id"}},
		{"id with a slash", instancetype.CreateInput{ID: "a/b", VCPUs: 1, MemoryMiB: 512}, []string{"id"}},
		{"id too long", instancetype.CreateInput{ID: strings.Repeat("a", 65), VCPUs: 1, MemoryMiB: 512}, []string{"id"}},
		{"zero vcpus", instancetype.CreateInput{ID: "t", MemoryMiB: 512}, []string{"vcpus"}},
		{"too many vcpus", instancetype.CreateInput{ID: "t", VCPUs: 257, MemoryMiB: 512}, []string{"vcpus"}},
		{"too little memory", instancetype.CreateInput{ID: "t", VCPUs: 1, MemoryMiB: 255}, []string{"memory_mib"}},
		{"too much memory", instancetype.CreateInput{ID: "t", VCPUs: 1, MemoryMiB: 1024*1024 + 1}, []string{"memory_mib"}},
		{"description too long", instancetype.CreateInput{ID: "t", VCPUs: 1, MemoryMiB: 512, Description: strings.Repeat("d", 256)}, []string{"description"}},
		{"everything at once", instancetype.CreateInput{ID: "", VCPUs: -1, MemoryMiB: 0}, []string{"id", "vcpus", "memory_mib"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := newService(t)
			_, err := svc.Create(context.Background(), tt.in)

			var validation *service.ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("Create() error = %v, want a *service.ValidationError", err)
			}
			var fields []string
			for _, f := range validation.Fields {
				fields = append(fields, f.Field)
			}
			if !slices.Equal(fields, tt.wantField) {
				t.Errorf("Create() rejected fields %v, want %v", fields, tt.wantField)
			}
		})
	}
}

func TestCreateRefusesATakenID(t *testing.T) {
	t.Parallel()

	svc := newService(t)

	_, err := svc.Create(context.Background(), instancetype.CreateInput{ID: "kumo.small", VCPUs: 2, MemoryMiB: 2048})
	if !errors.Is(err, service.ErrAlreadyExists) {
		t.Errorf("Create(kumo.small) error = %v, want service.ErrAlreadyExists", err)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   instancetype.UpdateInput
		want instancetype.Type
	}{
		{
			name: "a generated description follows the new size",
			in:   instancetype.UpdateInput{VCPUs: ptr(2), MemoryMiB: ptr(3072)},
			want: instancetype.Type{ID: "kumo.small", VCPUs: 2, MemoryMiB: 3072, Description: "2 vCPU, 3 GiB"},
		},
		{
			name: "only the memory",
			in:   instancetype.UpdateInput{MemoryMiB: ptr(1536)},
			want: instancetype.Type{ID: "kumo.small", VCPUs: 1, MemoryMiB: 1536, Description: "1 vCPU, 1536 MiB"},
		},
		{
			name: "only the description",
			in:   instancetype.UpdateInput{Description: ptr("web tier")},
			want: instancetype.Type{ID: "kumo.small", VCPUs: 1, MemoryMiB: 2048, Description: "web tier"},
		},
		{
			name: "a size and a description together",
			in:   instancetype.UpdateInput{VCPUs: ptr(4), Description: ptr("")},
			want: instancetype.Type{ID: "kumo.small", VCPUs: 4, MemoryMiB: 2048, Description: ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := newService(t)
			ctx := context.Background()

			got, err := svc.Update(ctx, "kumo.small", tt.in)
			if err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Update() = %+v, want %+v", got, tt.want)
			}
			if stored, err := svc.Get(ctx, "kumo.small"); err != nil || stored != tt.want {
				t.Errorf("Get() = %+v, %v; want %+v", stored, err, tt.want)
			}
		})
	}
}

func TestUpdateKeepsAHandWrittenDescription(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	ctx := context.Background()

	if _, err := svc.Update(ctx, "kumo.small", instancetype.UpdateInput{Description: ptr("web tier")}); err != nil {
		t.Fatalf("Update(description) error = %v", err)
	}
	got, err := svc.Update(ctx, "kumo.small", instancetype.UpdateInput{VCPUs: ptr(2)})
	if err != nil {
		t.Fatalf("Update(vcpus) error = %v", err)
	}
	if got.Description != "web tier" {
		t.Errorf("Update(vcpus).Description = %q, want the hand-written %q", got.Description, "web tier")
	}
}

func TestUpdateRefusals(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	ctx := context.Background()

	if _, err := svc.Update(ctx, "kumo.enormous", instancetype.UpdateInput{VCPUs: ptr(2)}); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Update(unknown) error = %v, want service.ErrNotFound", err)
	}

	var validation *service.ValidationError
	if _, err := svc.Update(ctx, "kumo.small", instancetype.UpdateInput{VCPUs: ptr(0)}); !errors.As(err, &validation) {
		t.Errorf("Update(vcpus=0) error = %v, want a *service.ValidationError", err)
	}
	if _, err := svc.Update(ctx, "kumo.small", instancetype.UpdateInput{MemoryMiB: ptr(64)}); !errors.As(err, &validation) {
		t.Errorf("Update(memory_mib=64) error = %v, want a *service.ValidationError", err)
	}

	// A refused update changes nothing.
	if got, err := svc.Get(ctx, "kumo.small"); err != nil || got != wantPresets[2] {
		t.Errorf("Get(kumo.small) after refused updates = %+v, %v; want %+v", got, err, wantPresets[2])
	}
}

func TestUpdateLeavesLaunchedInstancesAlone(t *testing.T) {
	t.Parallel()

	svc, db := newServiceWithStore(t)
	ctx := context.Background()
	insertInstance(t, db, "i-00000000000000001", "kumo.small")

	if _, err := svc.Update(ctx, "kumo.small", instancetype.UpdateInput{VCPUs: ptr(4), MemoryMiB: ptr(8192)}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	var vcpus, memory int
	if err := db.DB().QueryRowContext(ctx,
		`SELECT vcpus, memory_mib FROM instances WHERE id = 'i-00000000000000001'`).Scan(&vcpus, &memory); err != nil {
		t.Fatalf("read the instance: %v", err)
	}
	if vcpus != 1 || memory != 2048 {
		t.Errorf("instance size after the type changed = %d vCPU / %d MiB, want the launched 1 / 2048", vcpus, memory)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	ctx := context.Background()

	if err := svc.Delete(ctx, "kumo.2xlarge"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := svc.Get(ctx, "kumo.2xlarge"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get() after Delete() error = %v, want service.ErrNotFound", err)
	}
	if err := svc.Delete(ctx, "kumo.2xlarge"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("a second Delete() error = %v, want service.ErrNotFound", err)
	}
}

func TestDeleteRefusesATypeInUse(t *testing.T) {
	t.Parallel()

	for _, state := range []string{"running", "terminated"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()

			svc, db := newServiceWithStore(t)
			ctx := context.Background()
			insertInstanceInState(t, db, "i-00000000000000001", "kumo.small", state)

			err := svc.Delete(ctx, "kumo.small")
			if !errors.Is(err, service.ErrConflict) {
				t.Fatalf("Delete() error = %v, want service.ErrConflict", err)
			}
			if !strings.Contains(err.Error(), "i-00000000000000001") {
				t.Errorf("Delete() error = %q, want it to name the instance", err)
			}

			detail, err := svc.Detail(ctx, "kumo.small")
			if err != nil {
				t.Fatalf("Detail() error = %v", err)
			}
			if !slices.Equal(detail.UsedBy, []string{"i-00000000000000001"}) {
				t.Errorf("Detail().UsedBy = %v, want the instance", detail.UsedBy)
			}
		})
	}
}

func TestDetailOfAnUnusedType(t *testing.T) {
	t.Parallel()

	svc := newService(t)

	detail, err := svc.Detail(context.Background(), "kumo.nano")
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if detail.Type != wantPresets[0] || detail.UsedBy == nil || len(detail.UsedBy) != 0 {
		t.Errorf("Detail() = %+v, want the preset with an empty, non-nil used_by", detail)
	}
	if _, err := svc.Detail(context.Background(), "nope"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Detail(unknown) error = %v, want service.ErrNotFound", err)
	}
}

func TestNewValidatesItsDependencies(t *testing.T) {
	t.Parallel()

	if _, err := instancetype.New(instancetype.Options{Store: storetest.New(t)}); !errors.Is(err, instancetype.ErrNoLogger) {
		t.Errorf("New() without a logger error = %v, want instancetype.ErrNoLogger", err)
	}
	if _, err := instancetype.New(instancetype.Options{Logger: logging.Discard()}); !errors.Is(err, instancetype.ErrNoStore) {
		t.Errorf("New() without a store error = %v, want instancetype.ErrNoStore", err)
	}
}

func newService(t *testing.T) *instancetype.Service {
	t.Helper()

	svc, _ := newServiceWithStore(t)
	return svc
}

func newServiceWithStore(t *testing.T) (*instancetype.Service, *store.Store) {
	t.Helper()

	db := storetest.New(t)
	svc, err := instancetype.New(instancetype.Options{Logger: logging.Discard(), Store: db})
	if err != nil {
		t.Fatalf("instancetype.New(): %v", err)
	}
	return svc, db
}

// insertInstance writes a running instance row launched with the given type.
func insertInstance(t *testing.T, db *store.Store, id, instanceType string) {
	t.Helper()
	insertInstanceInState(t, db, id, instanceType, "running")
}

// insertInstanceInState writes the minimum instance row (and its image) that
// references a type, snapshotting the type's size the way a launch does.
func insertInstanceInState(t *testing.T, db *store.Store, id, instanceType, state string) {
	t.Helper()

	ctx := context.Background()
	if _, err := db.Exec(ctx,
		`INSERT OR IGNORE INTO images (id, name, state, created_at, updated_at)
		 VALUES ('img-00000000000000000', 'img-00000000000000000', 'available',
		         '2026-01-02T09:00:00Z', '2026-01-02T09:00:00Z')`); err != nil {
		t.Fatalf("insert image: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO instances (id, name, state, image_id, instance_type, vcpus, memory_mib,
		  root_disk_gb, launched_at, updated_at)
		 SELECT ?1, ?1, ?2, 'img-00000000000000000', id, vcpus, memory_mib, 10,
		        '2026-01-02T09:05:00Z', '2026-01-02T09:05:00Z'
		 FROM instance_types WHERE id = ?3`,
		id, state, instanceType); err != nil {
		t.Fatalf("insert instance: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
