package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
)

// instanceTypesBody is the body of GET /api/v1/instance-types.
type instanceTypesBody struct {
	Items []struct {
		ID          string `json:"id"`
		VCPUs       int    `json:"vcpus"`
		MemoryMiB   int    `json:"memory_mib"`
		Description string `json:"description"`
	} `json:"items"`
}

// capacityMetricBody mirrors one CapacityMetric of api/openapi.yaml.
type capacityMetricBody struct {
	Total     int64 `json:"total"`
	Allocated int64 `json:"allocated"`
}

// capacityBody is the body of GET /api/v1/system/capacity.
type capacityBody struct {
	VCPUs       capacityMetricBody `json:"vcpus"`
	MemoryBytes capacityMetricBody `json:"memory_bytes"`
	DiskBytes   capacityMetricBody `json:"disk_bytes"`
}

// newCapacityRouter wires the two M4.1 read endpoints over a real database and
// the in-memory backend, which is what kumod does with KUMO_HYPERVISOR=fake.
func newCapacityRouter(t *testing.T) *gin.Engine {
	t.Helper()

	router, _ := newCapacityRouterWithStore(t)
	return router
}

func newCapacityRouterWithStore(t *testing.T) (*gin.Engine, *store.Store) {
	t.Helper()

	logger := logging.Discard()
	db := storetest.New(t)

	signer, err := auth.NewSigner([]byte(strings.Repeat("i", 32)), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewSigner(): %v", err)
	}
	authSvc, err := authservice.New(authservice.Options{
		Logger: logger, Store: db, Signer: signer, RefreshTTL: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("authservice.New(): %v", err)
	}

	types, err := instancetype.New(instancetype.Options{Store: db})
	if err != nil {
		t.Fatalf("instancetype.New(): %v", err)
	}
	// kumod seeds the presets on every boot, so the endpoint is only exercised
	// through a router that did the same.
	if err := types.Seed(t.Context()); err != nil {
		t.Fatalf("Seed(): %v", err)
	}

	hv := fake.New(fake.Config{})
	if err := hv.EnsureInfra(t.Context(), hypervisor.InfraConfig{
		ImagesPool: "kumo-images", InstancesPool: "kumo-instances", DataDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("EnsureInfra(): %v", err)
	}

	handler := api.NewHandler(api.Options{
		Logger:        logger,
		GinMode:       gin.TestMode,
		System:        system.New(logger, "test", "fake", db, hv, db),
		Auth:          authSvc,
		InstanceTypes: types,
		Signer:        signer,
		RefreshTTL:    7 * 24 * time.Hour,
		CookieSecure:  false,
	})

	return handler.Router(), db
}

func TestListInstanceTypes(t *testing.T) {
	t.Parallel()

	router := newCapacityRouter(t)
	login := setupUserAndLogin(t, router)

	recorder := doWithToken(t, router, http.MethodGet, "/api/v1/instance-types", "", login.access)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /instance-types status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}

	var body instanceTypesBody
	decode(t, recorder, &body)

	// The seed set of docs/design/01-api.md, smallest first.
	want := []struct {
		id     string
		vcpus  int
		memory int
		descr  string
	}{
		{"kumo.nano", 1, 512, "1 vCPU, 512 MiB"},
		{"kumo.micro", 1, 1024, "1 vCPU, 1 GiB"},
		{"kumo.small", 1, 2048, "1 vCPU, 2 GiB"},
		{"kumo.medium", 2, 4096, "2 vCPU, 4 GiB"},
		{"kumo.large", 2, 8192, "2 vCPU, 8 GiB"},
		{"kumo.xlarge", 4, 16384, "4 vCPU, 16 GiB"},
		{"kumo.2xlarge", 8, 32768, "8 vCPU, 32 GiB"},
	}

	if len(body.Items) != len(want) {
		t.Fatalf("GET /instance-types returned %d presets, want %d", len(body.Items), len(want))
	}
	for i, w := range want {
		got := body.Items[i]
		if got.ID != w.id || got.VCPUs != w.vcpus || got.MemoryMiB != w.memory ||
			got.Description != w.descr {
			t.Errorf("preset %d = %+v, want %s (%d vCPU, %d MiB)",
				i, got, w.id, w.vcpus, w.memory)
		}
	}
}

func TestListInstanceTypesNeedsAToken(t *testing.T) {
	t.Parallel()

	router := newCapacityRouter(t)

	recorder := do(t, router, http.MethodGet, "/api/v1/instance-types", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("GET /instance-types status = %d, want 401", recorder.Code)
	}
}

func TestSystemCapacity(t *testing.T) {
	t.Parallel()

	router := newCapacityRouter(t)
	login := setupUserAndLogin(t, router)

	recorder := doWithToken(t, router, http.MethodGet, "/api/v1/system/capacity", "", login.access)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /system/capacity status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}

	var body capacityBody
	decode(t, recorder, &body)

	// The fake host reports 8 vCPUs, 16 GiB and 250 GiB, and the default
	// overcommit ratio of 4 is what the vCPU total carries.
	if body.VCPUs.Total != 32 {
		t.Errorf("vcpus.total = %d, want 32 (8 host vCPUs at the default ratio of 4)", body.VCPUs.Total)
	}
	if body.MemoryBytes.Total != 16<<30 {
		t.Errorf("memory_bytes.total = %d, want %d", body.MemoryBytes.Total, 16<<30)
	}
	if body.DiskBytes.Total != 250<<30 {
		t.Errorf("disk_bytes.total = %d, want %d", body.DiskBytes.Total, 250<<30)
	}
	if body.VCPUs.Allocated != 0 || body.MemoryBytes.Allocated != 0 || body.DiskBytes.Allocated != 0 {
		t.Errorf("a node with no instances reports allocated %+v, want zeros", body)
	}
}

func TestSystemCapacityCountsOnlyInstancesThatAreNotTerminated(t *testing.T) {
	t.Parallel()

	router, db := newCapacityRouterWithStore(t)
	login := setupUserAndLogin(t, router)

	insertCapacityInstance(t, db, "running", 2, 4096, 20)
	insertCapacityInstance(t, db, "terminated", 8, 32768, 100)

	recorder := doWithToken(t, router, http.MethodGet, "/api/v1/system/capacity", "", login.access)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /system/capacity status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}

	var body capacityBody
	decode(t, recorder, &body)

	// Only the running instance counts: a terminated row is history.
	if body.VCPUs.Allocated != 2 {
		t.Errorf("vcpus.allocated = %d, want 2 (a terminated instance must not count)", body.VCPUs.Allocated)
	}
	if body.MemoryBytes.Allocated != 4<<30 {
		t.Errorf("memory_bytes.allocated = %d, want %d", body.MemoryBytes.Allocated, 4<<30)
	}
	if body.DiskBytes.Allocated != 20<<30 {
		t.Errorf("disk_bytes.allocated = %d, want %d", body.DiskBytes.Allocated, 20<<30)
	}
}

func TestSystemCapacityNeedsAToken(t *testing.T) {
	t.Parallel()

	router := newCapacityRouter(t)

	recorder := do(t, router, http.MethodGet, "/api/v1/system/capacity", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("GET /system/capacity status = %d, want 401", recorder.Code)
	}
}

// insertCapacityInstance adds one instance row of the given size, which is what
// the allocated half of the capacity report is summed from.
func insertCapacityInstance(t *testing.T, db *store.Store, state string, vcpus, memoryMiB, diskGB int64) {
	t.Helper()

	ctx := context.Background()
	if _, err := db.Exec(ctx,
		`INSERT OR IGNORE INTO instance_types (id, vcpus, memory_mib) VALUES ('kumo.small', 1, 2048)`); err != nil {
		t.Fatalf("insert instance type: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT OR IGNORE INTO images (id, name, state, created_at, updated_at)
		 VALUES ('img-00000000000000000', 'img-00000000000000000', 'available',
		         '2026-01-02T09:00:00Z', '2026-01-02T09:00:00Z')`); err != nil {
		t.Fatalf("insert image: %v", err)
	}

	// The id only has to be unique, so derive it from the state.
	id := "i-" + state + strings.Repeat("0", 17-len(state))
	if _, err := db.Exec(ctx,
		`INSERT INTO instances (id, name, state, image_id, instance_type, vcpus, memory_mib,
		  root_disk_gb, launched_at, updated_at)
		 VALUES (?1, ?2, ?3, 'img-00000000000000000', 'kumo.small', ?4, ?5, ?6,
		         '2026-01-02T09:05:00Z', '2026-01-02T09:05:00Z')`,
		id, "web-"+state, state, vcpus, memoryMiB, diskGB); err != nil {
		t.Fatalf("insert instance: %v", err)
	}
}
