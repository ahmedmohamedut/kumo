package instance_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/imagefmt/qcow2test"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// errBackendSaidNo is what the fake is told to return at the step under test.
var errBackendSaidNo = errors.New("the backend said no")

// ed25519PublicKey is a fixture key: a launch test needs a key pair to inject
// without generating one per test.
const ed25519PublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMSkpB4KwlcQbdHTekHMstz+XeDU0Eugj1nOCRMlwz0e" +
	" fixture-ed25519"

const (
	imagesPool    = "kumo-images"
	instancesPool = "kumo-instances"
	networkName   = "kumo-default"
	instanceType  = "kumo.small"

	// imageDiskGB is the virtual size of the fixture image, and the smallest root
	// disk it boots from.
	imageDiskGB = 1
)

var testClock = func() time.Time {
	return time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC)
}

// harness is the service under test with its collaborators, for the assertions
// below the API surface.
type harness struct {
	svc     *instance.Service
	store   *store.Store
	hv      *fake.Fake
	images  *image.Service
	keyPair *keypair.Service
	tasks   *task.Service
	console string
	now     *time.Time
}

func newHarness(t *testing.T) harness {
	t.Helper()
	return newHarnessWith(t, fake.Config{}, instance.Options{})
}

// newHarnessWith builds a harness around a configured fake, with the options that
// a test needs to move: the node's capacity and the wait for a guest address.
func newHarnessWith(t *testing.T, hvConfig fake.Config, opts instance.Options) harness {
	t.Helper()

	db := storetest.New(t)
	ctx := t.Context()

	hv := fake.New(hvConfig)
	if err := hv.EnsureInfra(ctx, hypervisor.InfraConfig{
		ImagesPool:    imagesPool,
		InstancesPool: instancesPool,
		Network:       networkName,
		DataDir:       t.TempDir(),
	}); err != nil {
		t.Fatalf("EnsureInfra(): %v", err)
	}

	types, err := instancetype.New(instancetype.Options{Store: db})
	if err != nil {
		t.Fatalf("instancetype.New(): %v", err)
	}
	if err := types.Seed(ctx); err != nil {
		t.Fatalf("Seed(): %v", err)
	}

	// The system service measures the node and sums what the instances hold, and
	// the launch check runs on exactly its arithmetic.
	sys := system.New(logging.Discard(), "test", "fake", db, hv, db)
	sys.SetLimits(capacity.Limits{MemoryReserveMiB: 1024, VCPUOvercommit: 4})
	// The limits the launch check uses have to be the ones the report was built
	// with, or the two halves of the arithmetic would not agree.
	sys.SetLimits(opts.Limits)

	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}

	images, err := image.New(image.Options{
		Logger:     logging.Discard(),
		Store:      db,
		Hypervisor: hv,
		Tasks:      tasks,
		Downloader: nilDownloader(),
		ImagesPool: imagesPool,
		TempDir:    t.TempDir(),
		MaxBytes:   4 << 20,
	})
	if err != nil {
		t.Fatalf("image.New(): %v", err)
	}

	pairs, err := keypair.New(keypair.Options{Logger: logging.Discard(), Store: db})
	if err != nil {
		t.Fatalf("keypair.New(): %v", err)
	}

	now := testClock()
	console := t.TempDir()
	// A wait for an address is a second per poll and two minutes in total by
	// default, which no test wants to sit through; a test that cares sets them.
	if opts.PollInterval == 0 {
		opts.PollInterval = time.Millisecond
	}
	if opts.PollTimeout == 0 {
		opts.PollTimeout = time.Second
	}
	opts.Limits = capacity.Limits{MemoryReserveMiB: 1024, VCPUOvercommit: 4}

	opts.Logger = logging.Discard()
	opts.Store = db
	opts.Hypervisor = hv
	opts.Volumes = hv
	opts.Tasks = tasks
	opts.Images = images
	opts.Types = types
	opts.KeyPairs = pairs
	opts.Capacity = sys
	opts.InstancesPool = instancesPool
	opts.ImagesPool = imagesPool
	opts.ConsoleDir = console
	opts.Network = networkName
	opts.Now = func() time.Time { return now }

	svc, err := instance.New(opts)
	if err != nil {
		t.Fatalf("instance.New(): %v", err)
	}

	return harness{
		svc:     svc,
		store:   db,
		hv:      hv,
		images:  images,
		keyPair: pairs,
		tasks:   tasks,
		console: console,
		now:     &now,
	}
}

// nilDownloader is a downloader that refuses everything: these tests publish the
// image volume themselves, so nothing is ever fetched.
func nilDownloader() *imagefetch.Downloader {
	return imagefetch.New(imagefetch.Options{MaxBytes: 1 << 20})
}

// importKeyPair imports a fixture public key, so the seed gets a key to grant.
func (h *harness) importKeyPair(t *testing.T, name string) (keypair.KeyPair, error) {
	t.Helper()
	return h.keyPair.Import(t.Context(), keypair.ImportInput{
		Name:      name,
		PublicKey: ed25519PublicKey,
	})
}

// advance steps the harness clock forward by a second. A frozen clock would
// collapse every launched_at into a tie, and keyset pagination then orders by id.
func (h harness) advance() { *h.now = h.now.Add(time.Second) }

// availableImage publishes an image the launch can boot from, by writing the row
// and the pool volume directly: the import workflow is not what these tests are
// about.
func (h *harness) availableImage(t *testing.T, name string) image.LaunchSource {
	t.Helper()
	return h.availableImageWithMin(t, name, imageDiskGB)
}

// availableImageWithMin publishes an image whose smallest root disk is minGB.
func (h *harness) availableImageWithMin(t *testing.T, name string, minGB int) image.LaunchSource {
	t.Helper()

	imported, err := h.images.Import(t.Context(), image.ImportInput{
		Name:        name,
		SourceURL:   "https://example.invalid/image.qcow2",
		DefaultUser: "kumo",
		MinDiskGB:   minGB,
	})
	if err != nil {
		t.Fatalf("Import(): %v", err)
	}

	// Publish the volume and finalize the row the way ImportHandler does, without
	// a download in the way.
	body := qcow2test.Tiny(imageDiskGB << 30)
	path := filepath.Join(t.TempDir(), "image.qcow2")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write the fixture image: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the fixture image: %v", err)
	}
	defer file.Close() //nolint:errcheck // read-only handle

	if err := h.hv.ImportImage(t.Context(), imported.ID, file, hypervisor.ImportSpec{
		SizeBytes:        int64(len(body)),
		VirtualSizeBytes: imageDiskGB << 30,
		Format:           hypervisor.FormatQCOW2,
	}); err != nil {
		t.Fatalf("ImportImage(): %v", err)
	}

	if _, err := h.store.FinalizeImage(t.Context(), sqlc.FinalizeImageParams{
		ID:               imported.ID,
		Format:           string(hypervisor.FormatQCOW2),
		SizeBytes:        sql.NullInt64{Int64: int64(len(body)), Valid: true},
		VirtualSizeBytes: sql.NullInt64{Int64: imageDiskGB << 30, Valid: true},
		VolumeName:       sql.NullString{String: imported.ID, Valid: true},
		UpdatedAt:        h.now.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("FinalizeImage(): %v", err)
	}

	source, err := h.images.LaunchSource(t.Context(), imported.ID)
	if err != nil {
		t.Fatalf("LaunchSource(): %v", err)
	}
	return source
}

// pendingImage is an image that never finished importing, which a launch must
// refuse.
func (h *harness) pendingImage(t *testing.T, name string) image.LaunchSource {
	t.Helper()

	imported, err := h.images.Import(t.Context(), image.ImportInput{
		Name:      name,
		SourceURL: "https://example.invalid/image.qcow2",
		MinDiskGB: imageDiskGB,
	})
	if err != nil {
		t.Fatalf("Import(): %v", err)
	}
	source, err := h.images.LaunchSource(t.Context(), imported.ID)
	if err != nil {
		t.Fatalf("LaunchSource(): %v", err)
	}
	return source
}

// launch runs the launch task the way the worker does.
func (h *harness) launch(t *testing.T, instanceID string) error {
	t.Helper()
	return h.svc.LaunchHandler(t.Context(), worker.Task{
		ID:         "task-test",
		Type:       task.TypeInstanceLaunch,
		ResourceID: instanceID,
		Payload:    []byte(fmt.Sprintf(`{"instance_id":%q}`, instanceID)),
	})
}

// volumeNames lists the volumes of a pool, so a rollback can be asserted without
// sorting anything.
func (h *harness) volumeNames(pool string) []string {
	var names []string
	for _, volume := range h.hv.Volumes() {
		if volume.Pool == pool {
			names = append(names, volume.Name)
		}
	}
	return names
}

func TestNewValidatesItsDependencies(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	hv := fake.New(fake.Config{})
	types, err := instancetype.New(instancetype.Options{Store: db})
	if err != nil {
		t.Fatalf("instancetype.New(): %v", err)
	}
	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}
	images, err := image.New(image.Options{
		Logger: logging.Discard(), Store: db, Hypervisor: hv, ImagesPool: imagesPool,
		Tasks: tasks, TempDir: t.TempDir(), MaxBytes: 1 << 20, Downloader: nilDownloader(),
	})
	if err != nil {
		t.Fatalf("image.New(): %v", err)
	}
	pairs, err := keypair.New(keypair.Options{Logger: logging.Discard(), Store: db})
	if err != nil {
		t.Fatalf("keypair.New(): %v", err)
	}
	sys := system.New(logging.Discard(), "test", "fake", db, hv, db)

	full := instance.Options{
		Logger: logging.Discard(), Store: db, Hypervisor: hv, Volumes: hv, Tasks: tasks,
		Images: images, Types: types, KeyPairs: pairs, Capacity: sys,
		InstancesPool: instancesPool, ImagesPool: imagesPool,
		ConsoleDir: t.TempDir(), Network: networkName,
	}

	tests := []struct {
		name   string
		mutate func(*instance.Options)
		want   error
	}{
		{"no logger", func(o *instance.Options) { o.Logger = nil }, instance.ErrNoLogger},
		{"no store", func(o *instance.Options) { o.Store = nil }, instance.ErrNoStore},
		{"no hypervisor", func(o *instance.Options) { o.Hypervisor = nil }, instance.ErrNoHypervisor},
		{"no volume resolver", func(o *instance.Options) { o.Volumes = nil }, instance.ErrNoVolumes},
		{"no tasks", func(o *instance.Options) { o.Tasks = nil }, instance.ErrNoTasks},
		{"no images", func(o *instance.Options) { o.Images = nil }, instance.ErrNoImages},
		{"no types", func(o *instance.Options) { o.Types = nil }, instance.ErrNoTypes},
		{"no key pairs", func(o *instance.Options) { o.KeyPairs = nil }, instance.ErrNoKeyPairs},
		{"no capacity", func(o *instance.Options) { o.Capacity = nil }, instance.ErrNoCapacity},
		{"no instances pool", func(o *instance.Options) { o.InstancesPool = "" }, instance.ErrNoInstancesPool},
		{"no images pool", func(o *instance.Options) { o.ImagesPool = "" }, instance.ErrNoImagesPool},
		{"no console dir", func(o *instance.Options) { o.ConsoleDir = "" }, instance.ErrNoConsoleDir},
		{"no network", func(o *instance.Options) { o.Network = "" }, instance.ErrNoNetwork},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			opts := full
			test.mutate(&opts)
			if _, err := instance.New(opts); !errors.Is(err, test.want) {
				t.Fatalf("New() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestLaunchRefusesAnUnacceptableRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		in    instance.LaunchInput
		field string
	}{
		{
			name:  "no name",
			in:    instance.LaunchInput{ImageID: "img-1", InstanceType: instanceType},
			field: "name",
		},
		{
			name:  "uppercase name",
			in:    instance.LaunchInput{Name: "Web", ImageID: "img-1", InstanceType: instanceType},
			field: "name",
		},
		{
			name:  "name with a trailing dash",
			in:    instance.LaunchInput{Name: "web-", ImageID: "img-1", InstanceType: instanceType},
			field: "name",
		},
		{
			name:  "no image",
			in:    instance.LaunchInput{Name: "web", InstanceType: instanceType},
			field: "image_id",
		},
		{
			name:  "no instance type",
			in:    instance.LaunchInput{Name: "web", ImageID: "img-1"},
			field: "instance_type",
		},
		{
			name:  "root disk too large",
			in:    instance.LaunchInput{Name: "web", ImageID: "img-1", InstanceType: instanceType, RootDiskGB: 4096},
			field: "root_disk_gb",
		},
		{
			name:  "user data too large",
			in:    instance.LaunchInput{Name: "web", ImageID: "img-1", InstanceType: instanceType, UserData: strings.Repeat("x", 16<<10+1)},
			field: "user_data",
		},
		{
			name:  "count above one",
			in:    instance.LaunchInput{Name: "web", ImageID: "img-1", InstanceType: instanceType, Count: 3},
			field: "count",
		},
		{
			name:  "empty tag key",
			in:    instance.LaunchInput{Name: "web", ImageID: "img-1", InstanceType: instanceType, Tags: map[string]string{" ": "x"}},
			field: "tags",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			_, err := h.svc.Launch(t.Context(), test.in)

			if !isValidationErrorFor(t, err, test.field) {
				t.Fatalf("Launch() error = %v, want a %s validation error", err, test.field)
			}
		})
	}
}

func TestLaunchRefusesAnUnknownImageOrType(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if _, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: "img-doesnotexist", InstanceType: instanceType,
	}); !isValidationErrorFor(t, err, "image_id") {
		t.Fatalf("Launch() error = %v, want an image_id validation error", err)
	}

	if _, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: "img-1", InstanceType: "kumo.enormous",
	}); !isValidationErrorFor(t, err, "instance_type") {
		t.Fatalf("Launch() error = %v, want an instance_type validation error", err)
	}
}

func TestLaunchRefusesAnImageThatIsNotAvailable(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.pendingImage(t, "still-downloading")

	_, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: source.ID, InstanceType: instanceType,
	})
	if !isValidationErrorFor(t, err, "image_id") {
		t.Fatalf("Launch() error = %v, want an image_id validation error", err)
	}
	var invalid *service.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("Launch() error = %v, want a validation error", err)
	}
	if !hasFieldReason(invalid, "image_id", string(image.StatePending)) {
		t.Fatalf("Launch() fields = %v, want image_id to name the image state", invalid.Fields)
	}
}

func TestLaunchRefusesARootDiskSmallerThanTheImage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImageWithMin(t, "windows-like", 8)

	// The overlay cannot be smaller than the image it is a delta of: the guest
	// would see a disk that ends before its own filesystem.
	_, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "db", ImageID: source.ID, InstanceType: instanceType, RootDiskGB: 4,
	})
	if !isValidationErrorFor(t, err, "root_disk_gb") {
		t.Fatalf("Launch() error = %v, want a root_disk_gb validation error", err)
	}

	// At the image's own minimum it is accepted.
	if _, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "db2", ImageID: source.ID, InstanceType: instanceType, RootDiskGB: 8,
	}); err != nil {
		t.Fatalf("Launch(): %v", err)
	}
}

func TestLaunchRefusesAnUnknownKeyPair(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	_, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: source.ID, InstanceType: instanceType,
		KeyPairID: "key-00000000000000000",
	})
	if !isValidationErrorFor(t, err, "key_pair_id") {
		t.Fatalf("Launch() error = %v, want a key_pair_id validation error", err)
	}
}

func TestLaunchRefusesWhatTheNodeCannotHold(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		capacity hypervisor.Capacity
		in       func(imageID string) instance.LaunchInput
		want     string
	}{
		{
			// One vCPU and 64 MiB: every preset of docs/design/01-api.md wants more
			// memory than that, whatever the disk says.
			name:     "memory",
			capacity: hypervisor.Capacity{VCPUs: 1, MemoryBytes: 64 << 20, DiskBytes: 1 << 40},
			in: func(imageID string) instance.LaunchInput {
				return instance.LaunchInput{
					Name: "web", ImageID: imageID, InstanceType: instanceType, RootDiskGB: 8,
				}
			},
			want: "memory",
		},
		{
			// Memory to spare, but only 1 GiB of disk against an 8 GiB request.
			name:     "root disk",
			capacity: hypervisor.Capacity{VCPUs: 1, MemoryBytes: 4 << 30, DiskBytes: 1 << 30},
			in: func(imageID string) instance.LaunchInput {
				return instance.LaunchInput{
					Name: "web", ImageID: imageID, InstanceType: "kumo.nano", RootDiskGB: 8,
				}
			},
			want: "root disk",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarnessWith(t, fake.Config{Capacity: test.capacity}, instance.Options{})
			source := h.availableImage(t, "ubuntu")

			_, err := h.svc.Launch(t.Context(), test.in(source.ID))
			if !errors.Is(err, service.ErrQuota) {
				t.Fatalf("Launch() error = %v, want a quota error", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Launch() error = %v, want it to name the short resource", err)
			}
		})
	}
}

func TestLaunchRefusesANameThatIsTaken(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	in := instance.LaunchInput{Name: "web", ImageID: source.ID, InstanceType: instanceType}
	if _, err := h.svc.Launch(t.Context(), in); err != nil {
		t.Fatalf("Launch(): %v", err)
	}

	_, err := h.svc.Launch(t.Context(), in)
	if !errors.Is(err, service.ErrAlreadyExists) {
		t.Fatalf("Launch() error = %v, want an already-exists error", err)
	}
}

func TestLaunchRecordsTheRequestAndTheTask(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	pair, err := h.importKeyPair(t, "laptop")
	if err != nil {
		t.Fatalf("keypair.Import(): %v", err)
	}

	launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name:         "web",
		ImageID:      source.ID,
		InstanceType: instanceType,
		KeyPairID:    pair.ID,
		RootDiskGB:   8,
		UserData:     "#cloud-config\npackages: [curl]\n",
		Tags:         map[string]string{"env": "test"},
		CreatedBy:    "",
	})
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}

	if !strings.HasPrefix(launched.ID, "i-") || len(launched.ID) != 19 {
		t.Fatalf("Launch() id = %q, want i- plus 17 hex characters", launched.ID)
	}
	if launched.State != instance.StatePending {
		t.Fatalf("Launch() state = %q, want pending", launched.State)
	}
	if launched.TaskID == nil || !strings.HasPrefix(*launched.TaskID, "task-") {
		t.Fatalf("Launch() task_id = %v, want a task id", launched.TaskID)
	}
	if launched.PrivateIP != nil || launched.MACAddress != nil {
		t.Fatal("Launch() published an address before the guest booted")
	}
	if launched.RootDiskGB != 8 || launched.ImageID != source.ID {
		t.Fatalf("Launch() row = %+v, want the requested size and image", launched)
	}
	if launched.KeyPairName == nil || *launched.KeyPairName != pair.Name {
		t.Fatalf("Launch() key_pair_name = %v, want %q", launched.KeyPairName, pair.Name)
	}
	if launched.ImageName != source.Name {
		t.Fatalf("Launch() image_name = %q, want %q", launched.ImageName, source.Name)
	}
	if launched.Tags["env"] != "test" {
		t.Fatalf("Launch() tags = %v, want env=test", launched.Tags)
	}
	if launched.VCPUs == 0 || launched.MemoryMiB == 0 {
		t.Fatalf("Launch() sizes = %d vCPUs / %d MiB, want the preset snapshot",
			launched.VCPUs, launched.MemoryMiB)
	}

	// The task is queued and points at the instance, which is the only handle a
	// client has on the work that follows.
	queued, err := h.tasks.Get(t.Context(), *launched.TaskID)
	if err != nil {
		t.Fatalf("tasks.Get(): %v", err)
	}
	if queued.Type != task.TypeInstanceLaunch || queued.State != task.StatePending {
		t.Fatalf("task = %+v, want a pending instance.launch", queued)
	}
	if queued.ResourceID == nil || *queued.ResourceID != launched.ID {
		t.Fatalf("task resource_id = %v, want %q", queued.ResourceID, launched.ID)
	}

	// Nothing has touched libvirt: the launch is a task, not an operation.
	if got := h.volumeNames(instancesPool); len(got) != 0 {
		t.Fatalf("Launch() created the volumes %v, want none", got)
	}
}

func TestLaunchDefaultsTheRootDiskToTheImage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: source.ID, InstanceType: instanceType,
	})
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}
	if launched.RootDiskGB != source.MinDiskGB {
		t.Fatalf("root_disk_gb = %d, want the image's min_disk_gb of %d",
			launched.RootDiskGB, source.MinDiskGB)
	}
}

func TestLaunchWorkflowPublishesARunningInstance(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	pair, err := h.importKeyPair(t, "laptop")
	if err != nil {
		t.Fatalf("keypair.Import(): %v", err)
	}

	launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: source.ID, InstanceType: instanceType,
		KeyPairID: pair.ID, RootDiskGB: 8, UserData: "#cloud-config\npackages: [curl]\n",
	})
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}

	if err := h.launch(t, launched.ID); err != nil {
		t.Fatalf("LaunchHandler(): %v", err)
	}

	got, err := h.svc.Get(t.Context(), launched.ID, true)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.State != instance.StateRunning {
		t.Fatalf("state = %q with reason %v, want running", got.State, derefOr(got.StateReason))
	}
	if got.PrivateIP == nil || *got.PrivateIP == "" {
		t.Fatal("private_ip is empty; the workflow did not wait for a lease")
	}
	if got.MACAddress == nil || *got.MACAddress == "" {
		t.Fatal("mac_address is empty")
	}
	if got.UserData == nil || !strings.Contains(*got.UserData, "curl") {
		t.Fatalf("user_data = %v, want the launch payload", got.UserData)
	}

	// Both volumes exist in the instances pool, and neither touched the images
	// pool: the root disk is an overlay, the image stays where it was.
	rootDisk := hypervisor.RootDiskName(launched.ID)
	seed := hypervisor.SeedISOName(launched.ID)
	for _, name := range []string{rootDisk, seed} {
		if !contains(h.volumeNames(instancesPool), name) {
			t.Fatalf("the pool holds %v, want %q", h.volumeNames(instancesPool), name)
		}
	}

	// The seed carries the instance id, the hostname, the DHCP request and the
	// key, which is everything cloud-init needs to hand over the login.
	var seedVolume *fake.Volume
	for i, volume := range h.hv.Volumes() {
		if volume.Name == seed {
			seedVolume = &h.hv.Volumes()[i]
			break
		}
	}
	if seedVolume == nil {
		t.Fatal("the seed volume is missing")
	}
	if seedVolume.Seed != launched.ID {
		t.Fatalf("the seed belongs to %q, want %q", seedVolume.Seed, launched.ID)
	}
	for _, want := range []string{
		"instance-id: " + launched.ID,
		"local-hostname: web",
		"dhcp4: true",         // the network-config asks for DHCP
		"packages: [curl]",    // the launch's own user data survived the merge
		"ssh-ed25519 AAAA",    // the key pair's public key was granted
		"ssh_authorized_keys", // to the image's default user
		"kumo",                // which is the image's, not an arbitrary name
	} {
		if !strings.Contains(seedVolume.Content, want) {
			t.Fatalf("the seed does not carry %q:\n%s", want, seedVolume.Content)
		}
	}

	// The root disk is a delta of the image, sized to the request.
	for _, volume := range h.hv.Volumes() {
		if volume.Name != rootDisk {
			continue
		}
		if volume.Backing != source.ID {
			t.Fatalf("the root disk is a delta of %q, want %q", volume.Backing, source.ID)
		}
		if volume.VirtualSizeBytes != 8<<30 {
			t.Fatalf("the root disk is %d bytes, want 8 GiB", volume.VirtualSizeBytes)
		}
	}
}

func TestLaunchWorkflowIsIdempotent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: source.ID, InstanceType: instanceType, RootDiskGB: 8,
	})
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}

	if err := h.launch(t, launched.ID); err != nil {
		t.Fatalf("LaunchHandler(): %v", err)
	}
	// A requeued task runs the same workflow again. It must not define a second
	// domain or fail on the volumes the first run created.
	if err := h.launch(t, launched.ID); err != nil {
		t.Fatalf("the second LaunchHandler(): %v", err)
	}

	domains, err := h.hv.ListDomains(t.Context())
	if err != nil {
		t.Fatalf("ListDomains(): %v", err)
	}
	if len(domains) != 1 {
		t.Fatalf("the fake holds %d domains, want 1", len(domains))
	}
}

func TestLaunchWorkflowSkipsAnInstanceThatIsGone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	// A task whose instance was deleted before it ran has nothing to launch and
	// nothing to clean up.
	err := h.svc.LaunchHandler(t.Context(), worker.Task{
		ID: "task-test", Type: task.TypeInstanceLaunch,
		ResourceID: "i-00000000000000000",
		Payload:    []byte(`{"instance_id":"i-00000000000000000"}`),
	})
	if err != nil {
		t.Fatalf("LaunchHandler(): %v", err)
	}
}

func TestLaunchWorkflowRefusesAnUnusableTask(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	for _, test := range []struct {
		name    string
		payload string
		want    error
	}{
		// A payload the decoder rejects is refused by the worker before the
		// handler runs, which is the same refusal with a different name.
		{"no payload", ``, worker.ErrEmptyPayload},
		{"empty object", `{}`, instance.ErrBadTaskPayload},
		{"no instance id", `{"image_id":"img-1"}`, instance.ErrBadTaskPayload},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := h.svc.LaunchHandler(t.Context(), worker.Task{
				ID: "task-test", Type: task.TypeInstanceLaunch, Payload: []byte(test.payload),
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("LaunchHandler() error = %v, want %v", err, test.want)
			}
		})
	}

	// A payload naming an instance that no longer exists is skipped rather than
	// failed: the row was deleted after the task was queued, so there is nothing
	// to launch and nothing to clean up.
	if err := h.svc.LaunchHandler(t.Context(), worker.Task{
		ID: "task-test", Type: task.TypeInstanceLaunch,
		Payload: []byte(`{"instance_id":"i-00000000000000000"}`),
	}); err != nil {
		t.Fatalf("LaunchHandler() error = %v, want the missing row to be skipped", err)
	}
}

func TestLaunchWorkflowRollsBackWhatItCreated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		op   hypervisor.Op
		// remaining are the volumes a clean rollback must leave behind.
		remaining []string
	}{
		{name: "the root disk fails", op: hypervisor.OpCreateRootDisk},
		{
			name: "the seed fails",
			op:   hypervisor.OpCreateSeedISO,
			// The overlay the previous step made is removed too.
			remaining: []string{},
		},
		{
			name:      "the domain fails",
			op:        hypervisor.OpDefineAndStart,
			remaining: []string{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			source := h.availableImage(t, "ubuntu")

			launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
				Name: "web", ImageID: source.ID, InstanceType: instanceType, RootDiskGB: 8,
			})
			if err != nil {
				t.Fatalf("Launch(): %v", err)
			}

			h.hv.FailNext(test.op, errBackendSaidNo)

			if err := h.launch(t, launched.ID); err == nil {
				t.Fatal("LaunchHandler() succeeded, want the injected failure")
			}

			// Nothing is left in the instances pool, and the image is untouched.
			if got := h.volumeNames(instancesPool); len(got) != len(test.remaining) {
				t.Fatalf("the instances pool holds %v, want %v", got, test.remaining)
			}
			if got := h.volumeNames(imagesPool); len(got) != 1 {
				t.Fatalf("the images pool holds %v, want the image untouched", got)
			}

			// The row says why, so the console can show it.
			got, err := h.svc.Get(t.Context(), launched.ID, false)
			if err != nil {
				t.Fatalf("Get(): %v", err)
			}
			if got.State != instance.StateError {
				t.Fatalf("state = %q, want error", got.State)
			}
			if got.StateReason == nil || !strings.Contains(*got.StateReason, "the backend said no") {
				t.Fatalf("state_reason = %v, want the backend's message", got.StateReason)
			}
			if got.PrivateIP != nil {
				t.Fatal("a failed launch published an address")
			}
		})
	}
}

func TestLaunchWorkflowFailsWhenTheGuestTakesNoAddress(t *testing.T) {
	t.Parallel()

	h := newHarnessWith(t, fake.Config{}, instance.Options{
		PollInterval: time.Millisecond,
		PollTimeout:  20 * time.Millisecond,
	})
	source := h.availableImage(t, "ubuntu")

	launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: source.ID, InstanceType: instanceType, RootDiskGB: 8,
	})
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}
	h.hv.LeaseDelay(1_000_000)

	// A guest whose DHCP handshake never finishes. The launch must give up rather
	// than publish an instance nobody can reach.
	err = h.launch(t, launched.ID)
	if !errors.Is(err, instance.ErrNoAddress) {
		t.Fatalf("LaunchHandler() error = %v, want ErrNoAddress", err)
	}

	got, err := h.svc.Get(t.Context(), launched.ID, false)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.State != instance.StateError {
		t.Fatalf("state = %q, want error", got.State)
	}
	if got := h.volumeNames(instancesPool); len(got) != 0 {
		t.Fatalf("the instances pool holds %v, want a clean rollback", got)
	}
	if _, err := h.hv.GetDomain(t.Context(), launched.ID); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Fatalf("GetDomain() error = %v, want the domain to be gone", err)
	}
}

func TestLaunchWorkflowWaitsForTheLease(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: source.ID, InstanceType: instanceType, RootDiskGB: 8,
	})
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}

	// Three polls without an address, then the lease appears: the workflow has to
	// keep asking rather than give up at the first empty answer.
	h.hv.LeaseDelay(3)

	if err := h.launch(t, launched.ID); err != nil {
		t.Fatalf("LaunchHandler(): %v", err)
	}

	got, err := h.svc.Get(t.Context(), launched.ID, false)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.PrivateIP == nil || *got.PrivateIP == "" {
		t.Fatal("private_ip is empty after the lease appeared")
	}
}

func TestLaunchWorkflowKeepsTheDomainWhenTheRowFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: source.ID, InstanceType: instanceType, RootDiskGB: 8,
	})
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}

	// A trigger that refuses the state change makes the final write fail with the
	// guest already running. Tearing the instance down then would destroy work
	// that is fine, so the row stays pending and the reconciler can catch up.
	if _, err := h.store.Exec(t.Context(), `
		CREATE TRIGGER refuse_running BEFORE UPDATE OF state ON instances
		WHEN NEW.state = 'running'
		BEGIN SELECT RAISE(ABORT, 'the row refuses to become running'); END;`); err != nil {
		t.Fatalf("Exec(): %v", err)
	}

	if err := h.launch(t, launched.ID); err == nil {
		t.Fatal("LaunchHandler() succeeded, want the write failure")
	}

	if _, err := h.hv.GetDomain(t.Context(), launched.ID); err != nil {
		t.Fatalf("the domain was removed even though the guest is running: %v", err)
	}
	if got := h.volumeNames(instancesPool); len(got) != 2 {
		t.Fatalf("the instances pool holds %v, want the volumes of a running guest", got)
	}
}

func TestLaunchWorkflowSurvivesAShutdown(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: source.ID, InstanceType: instanceType, RootDiskGB: 8,
	})
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}

	// A canceled context is what a worker sees when the process is stopping. The
	// worker leaves the task 'running' and requeues it on the next Start
	// (worker.Pool.run), so the launch must leave the row pending and whatever it
	// managed to create in place, ready to be adopted.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := h.svc.LaunchHandler(ctx, worker.Task{
		ID: "task-test", Type: task.TypeInstanceLaunch, ResourceID: launched.ID,
		Payload: []byte(fmt.Sprintf(`{"instance_id":%q}`, launched.ID)),
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("LaunchHandler() error = %v, want the cancellation", err)
	}

	got, err := h.svc.Get(t.Context(), launched.ID, false)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.State != instance.StatePending {
		t.Fatalf("state = %q, want pending for the requeued task to pick up", got.State)
	}
	if got.StateReason != nil {
		t.Fatalf("state_reason = %q, want a shutdown not to be recorded as a failure", *got.StateReason)
	}
	if names := h.volumeNames(instancesPool); len(names) != 0 {
		t.Fatalf("a shutdown left %v behind, want the rollback to be left to the requeued task", names)
	}

	// The requeued task runs the whole launch and adopts nothing, because the
	// interrupted run created nothing.
	if err := h.launch(t, launched.ID); err != nil {
		t.Fatalf("the requeued launch: %v", err)
	}
	if _, err := h.hv.GetDomain(t.Context(), launched.ID); err != nil {
		t.Fatalf("GetDomain(): %v", err)
	}
}

func TestLaunchWorkflowLaunchesWithoutAKeyPairThatWasForceDeleted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	pair, err := h.importKeyPair(t, "laptop")
	if err != nil {
		t.Fatalf("keypair.Import(): %v", err)
	}

	launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: source.ID, InstanceType: instanceType,
		KeyPairID: pair.ID, RootDiskGB: 8,
	})
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}

	// A key pair cannot be deleted while an instance is using it, but force
	// exists for the operator who insists. Deleting it detaches the key from
	// every instance that pointed at it (ON DELETE SET NULL in
	// internal/store/migrations), so the launch goes ahead without a key rather
	// than refusing to boot.
	if err := h.keyPair.Delete(t.Context(), pair.ID, true); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	if _, err := h.keyPair.Get(t.Context(), pair.ID); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Get() error = %v, want the key pair to be gone", err)
	}

	if err := h.launch(t, launched.ID); err != nil {
		t.Fatalf("LaunchHandler(): %v", err)
	}

	var seed *fake.Volume
	for _, volume := range h.hv.Volumes() {
		if volume.Name == hypervisor.SeedISOName(launched.ID) {
			seed = &volume
		}
	}
	if seed == nil {
		t.Fatal("the launch made no seed to inspect")
	}
	if strings.Contains(seed.Content, "ssh_authorized_keys") {
		t.Fatalf("the seed grants a key that was deleted:\n%s", seed.Content)
	}

	got, err := h.svc.Get(t.Context(), launched.ID, false)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.State != instance.StateRunning {
		t.Fatalf("state = %q, want running", got.State)
	}

	// The runtime data the reconciler matches a domain against is on the row even
	// though it is not in the API response.
	info, err := h.hv.GetDomain(t.Context(), launched.ID)
	if err != nil {
		t.Fatalf("GetDomain(): %v", err)
	}
	row, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: launched.ID})
	if err != nil {
		t.Fatalf("GetInstance(): %v", err)
	}
	if !row.LibvirtUuid.Valid || row.LibvirtUuid.String != info.UUID {
		t.Errorf("libvirt_uuid = %v, want %q", row.LibvirtUuid, info.UUID)
	}
	if !row.MacAddress.Valid || row.MacAddress.String != info.MAC {
		t.Errorf("mac_address = %v, want %q", row.MacAddress, info.MAC)
	}
	// The row keeps the key pair name for display, even after the key is gone.
	if got.KeyPairName == nil || *got.KeyPairName != pair.Name {
		t.Fatalf("key_pair_name = %v, want %q", got.KeyPairName, pair.Name)
	}
}

func TestGetReportsAnUnknownInstanceAsNotFound(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if _, err := h.svc.Get(t.Context(), "i-00000000000000000", false); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Get() error = %v, want a not-found error", err)
	}
}

func TestGetHidesTheUserDataFromANonAdmin(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: source.ID, InstanceType: instanceType,
		UserData: "#cloud-config\nruncmd: [secret]\n",
	})
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}

	asViewer, err := h.svc.Get(t.Context(), launched.ID, false)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if asViewer.UserData != nil {
		t.Fatalf("user_data = %v, want it hidden from a viewer", asViewer.UserData)
	}

	asAdmin, err := h.svc.Get(t.Context(), launched.ID, true)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if asAdmin.UserData == nil || !strings.Contains(*asAdmin.UserData, "secret") {
		t.Fatalf("user_data = %v, want it for an administrator", asAdmin.UserData)
	}
}

func TestListPagesThroughInstances(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	var ids []string
	for _, name := range []string{"web", "db", "cache"} {
		launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
			Name: name, ImageID: source.ID, InstanceType: instanceType, RootDiskGB: 8,
		})
		if err != nil {
			t.Fatalf("Launch(%s): %v", name, err)
		}
		ids = append(ids, launched.ID)
		h.advance()
	}

	// Newest first is the default, so the last launch comes first.
	page, err := h.svc.List(t.Context(), instance.ListInput{PageSize: 2})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("the page holds %d instances, want 2", len(page.Items))
	}
	if page.Items[0].ID != ids[2] || page.Items[1].ID != ids[1] {
		t.Fatalf("the page is ordered %s, %s; want the newest first", page.Items[0].ID, page.Items[1].ID)
	}
	if page.NextPageToken == nil {
		t.Fatal("next_page_token is nil, want the third instance to be reachable")
	}

	next, err := h.svc.List(t.Context(), instance.ListInput{
		PageSize: 2, PageToken: *page.NextPageToken,
	})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(next.Items) != 1 || next.Items[0].ID != ids[0] {
		t.Fatalf("the second page holds %v, want only %s", next.Items, ids[0])
	}
	if next.NextPageToken != nil {
		t.Fatalf("next_page_token = %v, want nil on the last page", next.NextPageToken)
	}
}

func TestListFiltersAndSorts(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	for _, name := range []string{"web", "web-2", "db"} {
		launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
			Name: name, ImageID: source.ID, InstanceType: instanceType, RootDiskGB: 8,
		})
		if err != nil {
			t.Fatalf("Launch(%s): %v", name, err)
		}
		h.advance()
		if err := h.launch(t, launched.ID); err != nil {
			t.Fatalf("LaunchHandler(%s): %v", name, err)
		}
	}

	pending, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "queue", ImageID: source.ID, InstanceType: instanceType, RootDiskGB: 8,
	})
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}

	tests := []struct {
		name  string
		in    instance.ListInput
		want  []string
		count int
	}{
		{name: "by name", in: instance.ListInput{Query: "web"}, count: 2},
		{name: "by state", in: instance.ListInput{State: instance.StatePending}, want: []string{pending.ID}, count: 1},
		{name: "by running", in: instance.ListInput{State: instance.StateRunning}, count: 3},
		{
			name: "oldest first",
			in:   instance.ListInput{Sort: instance.SortLaunchedAsc, PageSize: 10},
			want: []string{"web", "web-2", "db", pending.ID}, count: 4,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page, err := h.svc.List(t.Context(), test.in)
			if err != nil {
				t.Fatalf("List(): %v", err)
			}
			if len(page.Items) != test.count {
				t.Fatalf("the page holds %d instances, want %d: %+v", len(page.Items), test.count, page.Items)
			}
			for i, want := range test.want {
				if got := page.Items[i]; got.ID != want && got.Name != want {
					t.Fatalf("item %d is %q (%s), want %q", i, got.ID, got.Name, want)
				}
			}
		})
	}
}

func TestListCarriesTheImageNameAndNoUserData(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "ubuntu")

	if _, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web", ImageID: source.ID, InstanceType: instanceType,
		UserData: "#cloud-config\nruncmd: [secret]\n",
	}); err != nil {
		t.Fatalf("Launch(): %v", err)
	}

	page, err := h.svc.List(t.Context(), instance.ListInput{})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if page.Items[0].ImageName != source.Name {
		t.Fatalf("image_name = %q, want %q", page.Items[0].ImageName, source.Name)
	}
	if page.Items[0].UserData != nil {
		t.Fatalf("user_data = %v, want a list never to carry it", page.Items[0].UserData)
	}
}

func TestListRefusesAnUnacceptablePage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	tests := []struct {
		name string
		in   instance.ListInput
		want string
	}{
		{"bad sort", instance.ListInput{Sort: "name:asc"}, "sort"},
		{"bad state", instance.ListInput{State: "booting"}, "state"},
		{"page size too small", instance.ListInput{PageSize: -1}, "page_size"},
		{"page size too large", instance.ListInput{PageSize: 1000}, "page_size"},
		{"bad page token", instance.ListInput{PageToken: "not-a-token"}, "page_token"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := h.svc.List(t.Context(), test.in)
			if !isValidationErrorFor(t, err, test.want) {
				t.Fatalf("List() error = %v, want a %s validation error", err, test.want)
			}
		})
	}
}

// helpers

// isValidationErrorFor reports whether err is a service.ValidationError with a
// field error about want.
func isValidationErrorFor(t *testing.T, err error, want string) bool {
	t.Helper()

	var invalid *service.ValidationError
	if !errors.As(err, &invalid) {
		t.Errorf("error = %v, want a validation error", err)
		return false
	}
	for _, field := range invalid.Fields {
		if field.Field == want {
			return true
		}
	}
	t.Errorf("validation fields = %v, want one about %q", invalid.Fields, want)
	return false
}

// hasFieldReason reports whether a field error mentions want in its reason.
func hasFieldReason(err *service.ValidationError, field, want string) bool {
	for _, f := range err.Fields {
		if f.Field == field && strings.Contains(f.Reason, want) {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func derefOr(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
