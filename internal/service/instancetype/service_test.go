package instancetype_test

import (
	"context"
	"errors"
	"testing"

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

func TestSeedInsertsTheDocumentedPresets(t *testing.T) {
	t.Parallel()

	svc := newService(t)

	if got, err := svc.List(context.Background()); err != nil {
		t.Fatalf("List() error = %v", err)
	} else if len(got) != 0 {
		t.Fatalf("List() on an empty database = %+v, want nothing before Seed", got)
	}

	if err := svc.Seed(context.Background()); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	got, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != len(wantPresets) {
		t.Fatalf("List() returned %d presets, want %d", len(got), len(wantPresets))
	}
	for i, want := range wantPresets {
		if got[i] != want {
			t.Errorf("List()[%d] = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestSeedIsIdempotent(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	ctx := context.Background()

	for range 3 {
		if err := svc.Seed(ctx); err != nil {
			t.Fatalf("Seed() error = %v", err)
		}
	}

	got, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != len(wantPresets) {
		t.Errorf("List() returned %d presets after three seeds, want %d", len(got), len(wantPresets))
	}
}

func TestSeedKeepsAnEditedPreset(t *testing.T) {
	t.Parallel()

	svc, db := newServiceWithStore(t)
	ctx := context.Background()

	if err := svc.Seed(ctx); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	// INSERT OR IGNORE is what makes a restart safe: a preset an operator
	// tuned on the node survives the next boot.
	if _, err := db.Exec(ctx,
		`UPDATE instance_types SET memory_mib = 3072 WHERE id = 'kumo.small'`); err != nil {
		t.Fatalf("update the preset: %v", err)
	}
	if err := svc.Seed(ctx); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	edited, err := svc.Get(ctx, "kumo.small")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if edited.MemoryMiB != 3072 {
		t.Errorf("Get(kumo.small).MemoryMiB = %d, want the edited 3072", edited.MemoryMiB)
	}
}

func TestGetReportsAnUnknownPresetAsNotFound(t *testing.T) {
	t.Parallel()

	svc := newService(t)
	ctx := context.Background()
	if err := svc.Seed(ctx); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	_, err := svc.Get(ctx, "kumo.enormous")
	if !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get(kumo.enormous) error = %v, want service.ErrNotFound", err)
	}
}

func TestNewNeedsAStore(t *testing.T) {
	t.Parallel()

	if _, err := instancetype.New(instancetype.Options{}); !errors.Is(err, instancetype.ErrNoStore) {
		t.Errorf("New() error = %v, want instancetype.ErrNoStore", err)
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
	svc, err := instancetype.New(instancetype.Options{Store: db})
	if err != nil {
		t.Fatalf("instancetype.New(): %v", err)
	}
	return svc, db
}
