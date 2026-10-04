package fake_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
)

const (
	imagesPool    = "kumo-images"
	instancesPool = "kumo-instances"
)

var infra = hypervisor.InfraConfig{
	ImagesPool:    imagesPool,
	InstancesPool: instancesPool,
	Network:       "kumo-default",
	DataDir:       "/var/lib/kumo",
}

var errBoom = errors.New("boom")

// metaDataFor renders the meta-data document the way the instance service does,
// so a fake test reads like the service that feeds it.
func metaDataFor(instanceID string) string {
	return "instance-id: " + instanceID + "\nlocal-hostname: web\n"
}

// newFake returns a fake with the infrastructure already in place.
func newFake(t *testing.T) *fake.Fake {
	t.Helper()

	f := fake.New(fake.Config{})
	if err := f.EnsureInfra(t.Context(), infra); err != nil {
		t.Fatalf("EnsureInfra() error = %v", err)
	}
	return f
}

// qcow2Spec is the import spec of a qcow2-looking image of len(content) bytes,
// whose virtual size is twice the file.
func qcow2Spec(content []byte) hypervisor.ImportSpec {
	return hypervisor.ImportSpec{
		SizeBytes:        int64(len(content)),
		VirtualSizeBytes: int64(len(content)) * 2,
		Format:           hypervisor.FormatQCOW2,
	}
}

// rawSpec is the import spec of a plain file of size bytes.
func rawSpec(size int64) hypervisor.ImportSpec {
	return hypervisor.ImportSpec{
		SizeBytes:        size,
		VirtualSizeBytes: size,
		Format:           hypervisor.FormatRaw,
	}
}

// importImage stores a qcow2-looking image in the images pool.
func importImage(t *testing.T, f *fake.Fake, name string) {
	t.Helper()

	content := append([]byte("QFI\xfb"), bytes.Repeat([]byte{0x00}, 1023)...)
	if err := f.ImportImage(t.Context(), name, bytes.NewReader(content), qcow2Spec(content)); err != nil {
		t.Fatalf("ImportImage(%q) error = %v", name, err)
	}
}

// launchInstance stores the volumes a launch would create and boots a domain.
func launchInstance(t *testing.T, f *fake.Fake, id, image string) hypervisor.DomainInfo {
	t.Helper()

	importImage(t, f, image)

	ctx := t.Context()
	if err := f.CreateRootDisk(ctx, hypervisor.RootDiskSpec{
		InstanceID:   id,
		SourceVolume: image,
		Pool:         instancesPool,
		SizeGiB:      10,
	}); err != nil {
		t.Fatalf("CreateRootDisk(%q) error = %v", id, err)
	}
	if err := f.CreateSeedISO(ctx, instancesPool, fake.SeedISOName(id), hypervisor.CloudInit{
		MetaData:      "instance-id: " + id + "\nlocal-hostname: web\n",
		NetworkConfig: "version: 2\n",
	}); err != nil {
		t.Fatalf("CreateSeedISO(%q) error = %v", id, err)
	}

	info, err := f.DefineAndStart(ctx, hypervisor.DomainSpec{
		ID:           id,
		Name:         "web",
		UUID:         "6f1c0d64-6f3f-4c3f-8a1d-0a9f3d1b2c3d",
		VCPUs:        2,
		MemoryMiB:    2048,
		RootDiskPool: instancesPool,
		Network:      infra.Network,
	})
	if err != nil {
		t.Fatalf("DefineAndStart(%q) error = %v", id, err)
	}
	return info
}

func TestFakeImplementsHypervisor(t *testing.T) {
	t.Parallel()

	var _ hypervisor.Hypervisor = fake.New(fake.Config{})
}

func TestImportImage(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()

	qcow2 := append([]byte("QFI\xfb"), bytes.Repeat([]byte{0x01}, 2047)...)
	if err := f.ImportImage(ctx, "img-qcow2", bytes.NewReader(qcow2), qcow2Spec(qcow2)); err != nil {
		t.Fatalf("ImportImage() error = %v", err)
	}
	if err := f.ImportImage(ctx, "img-raw", strings.NewReader("not a qcow2"), rawSpec(11)); err != nil {
		t.Fatalf("ImportImage() error = %v", err)
	}

	tests := []struct {
		name   string
		format hypervisor.ImageFormat
		size   int64
	}{
		{name: "img-qcow2", format: hypervisor.FormatQCOW2, size: int64(len(qcow2))},
		{name: "img-raw", format: hypervisor.FormatRaw, size: 11},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := f.ImageInfo(ctx, tt.name)
			if err != nil {
				t.Fatalf("ImageInfo() error = %v", err)
			}
			if info.Format != tt.format {
				t.Errorf("format = %q, want %q", info.Format, tt.format)
			}
			if info.SizeBytes != tt.size {
				t.Errorf("size = %d, want %d", info.SizeBytes, tt.size)
			}
			if info.Pool != imagesPool {
				t.Errorf("pool = %q, want %q", info.Pool, imagesPool)
			}
		})
	}
}

func TestImportImageErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		image     string
		content   string
		size      int64
		format    hypervisor.ImageFormat
		wantErrIs error
		wantErr   string
	}{
		{name: "empty name", image: "", content: "x", size: 1, wantErr: "image name"},
		{name: "empty content", image: "img-empty", content: "", size: 0, wantErr: "empty"},
		{name: "too large", image: "img-big", content: "123456", size: 3, wantErr: "larger than"},
		{
			// A workflow that declares qcow2 for a plain file would hand libvirt
			// a volume that cannot boot, so the fake refuses it.
			name:      "declared format disagrees with the header",
			image:     "img-mismatch",
			content:   "not a qcow2",
			size:      12,
			format:    hypervisor.FormatQCOW2,
			wantErrIs: fake.ErrFormatMismatch,
		},
		{
			name:      "duplicate",
			image:     "img-duplicate",
			content:   "1234",
			size:      4,
			wantErrIs: hypervisor.ErrAlreadyExists,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			ctx := t.Context()

			if tt.name == "duplicate" {
				importImage(t, f, tt.image)
			}

			err := f.ImportImage(ctx, tt.image, strings.NewReader(tt.content),
				hypervisor.ImportSpec{
					SizeBytes:        tt.size,
					VirtualSizeBytes: tt.size,
					Format:           tt.format,
				})
			if err == nil {
				t.Fatal("ImportImage() error = nil, want an error")
			}
			if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
				t.Errorf("error = %v, want it to wrap %v", err, tt.wantErrIs)
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestVolumeLifecycle(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()

	importImage(t, f, "img-base")

	if err := f.DeleteVolume(ctx, imagesPool, "img-base"); err != nil {
		t.Fatalf("DeleteVolume() error = %v", err)
	}
	if err := f.DeleteVolume(ctx, imagesPool, "img-base"); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("second DeleteVolume() error = %v, want ErrNotFound", err)
	}
	if _, err := f.ImageInfo(ctx, "img-base"); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("ImageInfo() after delete error = %v, want ErrNotFound", err)
	}
}

func TestCreateRootDisk(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()

	t.Run("missing source image", func(t *testing.T) {
		err := f.CreateRootDisk(ctx, hypervisor.RootDiskSpec{
			InstanceID:   "i-1",
			SourceVolume: "img-nope",
			Pool:         instancesPool,
			SizeGiB:      10,
		})
		if !errors.Is(err, hypervisor.ErrNotFound) {
			t.Fatalf("CreateRootDisk() error = %v, want ErrNotFound", err)
		}
	})

	t.Run("invalid spec", func(t *testing.T) {
		for _, spec := range []hypervisor.RootDiskSpec{
			{SourceVolume: "img-base", Pool: instancesPool, SizeGiB: 10},
			{InstanceID: "i-1", Pool: instancesPool, SizeGiB: 10},
			{InstanceID: "i-1", SourceVolume: "img-base", SizeGiB: 10},
			{InstanceID: "i-1", SourceVolume: "img-base", Pool: instancesPool},
			{InstanceID: "i-1", SourceVolume: "img-base", Pool: instancesPool, SizeGiB: -1},
		} {
			if err := f.CreateRootDisk(ctx, spec); err == nil {
				t.Errorf("CreateRootDisk(%+v) error = nil, want an error", spec)
			}
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		importImage(t, f, "img-base")
		spec := hypervisor.RootDiskSpec{
			InstanceID:   "i-1",
			SourceVolume: "img-base",
			Pool:         instancesPool,
			SizeGiB:      10,
		}
		if err := f.CreateRootDisk(ctx, spec); err != nil {
			t.Fatalf("CreateRootDisk() error = %v", err)
		}
		if err := f.CreateRootDisk(ctx, spec); !errors.Is(err, hypervisor.ErrAlreadyExists) {
			t.Errorf("second CreateRootDisk() error = %v, want ErrAlreadyExists", err)
		}
	})
}

func TestCreateSeedISO(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()

	t.Run("goes into the pool it was told to use", func(t *testing.T) {
		// A seed is per-instance data and belongs beside the root disks. Filing
		// it with the images would let an image delete take a live guest's seed
		// with it, and the pool would slowly fill with dead ISOs.
		if err := f.CreateSeedISO(ctx, instancesPool, fake.SeedISOName("i-1"),
			hypervisor.CloudInit{MetaData: "instance-id: i-1\nlocal-hostname: web\n"}); err != nil {
			t.Fatalf("CreateSeedISO() error = %v", err)
		}

		volumes := f.Volumes()
		if len(volumes) != 1 {
			t.Fatalf("the fake holds %d volumes, want 1", len(volumes))
		}
		if volumes[0].Pool != instancesPool {
			t.Errorf("the seed is in pool %q, want %q", volumes[0].Pool, instancesPool)
		}
		if volumes[0].Seed != "i-1" {
			t.Errorf("the seed belongs to %q, want i-1", volumes[0].Seed)
		}
		if !strings.Contains(volumes[0].Content, "instance-id: i-1") {
			t.Errorf("the seed does not carry its instance id:\n%s", volumes[0].Content)
		}
	})

	t.Run("carries the documents it was given", func(t *testing.T) {
		seed := hypervisor.CloudInit{
			MetaData:      "instance-id: i-2\nlocal-hostname: web\n",
			UserData:      "#cloud-config\nusers: []\n",
			NetworkConfig: "version: 2\nethernets: {}\n",
		}
		if err := f.CreateSeedISO(ctx, instancesPool, fake.SeedISOName("i-2"), seed); err != nil {
			t.Fatalf("CreateSeedISO() error = %v", err)
		}

		var content string
		for _, v := range f.Volumes() {
			if v.Seed == "i-2" {
				content = v.Content
			}
		}
		for _, want := range []string{"instance-id: i-2", "local-hostname: web", "version: 2", "#cloud-config"} {
			if !strings.Contains(content, want) {
				t.Errorf("the seed does not carry %q:\n%s", want, content)
			}
		}
	})

	t.Run("invalid spec", func(t *testing.T) {
		for _, test := range []struct {
			name string
			pool string
			iso  string
			seed hypervisor.CloudInit
		}{
			{name: "no name", pool: instancesPool, iso: "", seed: hypervisor.CloudInit{MetaData: metaDataFor("i-3")}},
			{name: "no pool", pool: "", iso: "i-3-seed.iso", seed: hypervisor.CloudInit{MetaData: metaDataFor("i-3")}},
			{name: "no meta-data", pool: instancesPool, iso: "i-3-seed.iso"},
			{name: "meta-data without an instance id", pool: instancesPool, iso: "i-3-seed.iso",
				seed: hypervisor.CloudInit{MetaData: "local-hostname: web\n"}},
		} {
			if err := f.CreateSeedISO(ctx, test.pool, test.iso, test.seed); err == nil {
				t.Errorf("%s: CreateSeedISO() error = nil, want an error", test.name)
			}
		}
	})
}

// TestLeaseDelayHoldsTheAddressBack covers the window the launch workflow has to
// cope with: a domain that is running but has no address yet.
func TestLeaseDelayHoldsTheAddressBack(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.LeaseDelay(2)
	ctx := t.Context()

	info := launchInstance(t, f, "i-lease", "img-base")
	if info.IP != "" {
		t.Fatalf("DefineAndStart() reported the address %q before any poll", info.IP)
	}

	for poll := range 2 {
		got, err := f.GetDomain(ctx, "i-lease")
		if err != nil {
			t.Fatalf("GetDomain() error = %v", err)
		}
		if got.IP != "" {
			t.Errorf("poll %d reported the address %q, want none yet", poll+1, got.IP)
		}
		if got.State != hypervisor.DomainStateRunning {
			t.Errorf("poll %d reported state %q, want running", poll+1, got.State)
		}
	}

	got, err := f.GetDomain(ctx, "i-lease")
	if err != nil {
		t.Fatalf("GetDomain() error = %v", err)
	}
	if got.IP == "" {
		t.Error("GetDomain() still has no address after the delay")
	}
	if got.MAC == "" {
		t.Error("GetDomain() has no MAC address")
	}
}

// TestVolumesListsWhatIsLeft makes the rollback assertions in the service tests
// possible: they check the pool, not a return value.
func TestVolumesListsWhatIsLeft(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()

	if got := f.Volumes(); len(got) != 0 {
		t.Fatalf("a fresh fake holds %d volumes, want none", len(got))
	}

	importImage(t, f, "img-base")
	if err := f.CreateRootDisk(ctx, hypervisor.RootDiskSpec{
		InstanceID:   "i-1",
		SourceVolume: "img-base",
		Pool:         instancesPool,
		SizeGiB:      10,
	}); err != nil {
		t.Fatalf("CreateRootDisk() error = %v", err)
	}

	volumes := f.Volumes()
	if len(volumes) != 2 {
		t.Fatalf("the fake holds %d volumes, want 2", len(volumes))
	}
	// Ordered by pool then name: kumo-images before kumo-instances.
	if volumes[0].Pool != imagesPool || volumes[1].Pool != instancesPool {
		t.Errorf("volumes are ordered %q, %q; want the images pool first",
			volumes[0].Pool, volumes[1].Pool)
	}
	if volumes[1].Backing != "img-base" {
		t.Errorf("the root disk reads from %q, want img-base", volumes[1].Backing)
	}
	if volumes[1].VirtualSizeBytes != 10*fake.GiB {
		t.Errorf("the root disk is %d bytes, want 10 GiB", volumes[1].VirtualSizeBytes)
	}
}

func TestDefineAndStart(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()

	t.Run("without a root disk", func(t *testing.T) {
		_, err := f.DefineAndStart(ctx, hypervisor.DomainSpec{ID: "i-1", RootDiskPool: instancesPool})
		if !errors.Is(err, hypervisor.ErrNotFound) {
			t.Fatalf("DefineAndStart() error = %v, want ErrNotFound", err)
		}
	})

	t.Run("without an id", func(t *testing.T) {
		_, err := f.DefineAndStart(ctx, hypervisor.DomainSpec{RootDiskPool: instancesPool})
		if err == nil {
			t.Fatal("DefineAndStart() error = nil, want an error")
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		launchInstance(t, f, "i-1", "img-a")
		_, err := f.DefineAndStart(ctx, hypervisor.DomainSpec{
			ID:           "i-1",
			RootDiskPool: instancesPool,
		})
		if !errors.Is(err, hypervisor.ErrAlreadyExists) {
			t.Fatalf("DefineAndStart() error = %v, want ErrAlreadyExists", err)
		}
	})
}

// TestDefineAndStartMintsAUuid covers the case the launch workflow actually hits:
// libvirt assigns a uuid to a definition that has none, so a fake that left it
// empty would make a launched instance record no uuid against a real host.
func TestDefineAndStartMintsAUuid(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	launchInstance(t, f, "i-no-uuid", "img-base")

	// Defined a second time under another name, this time with no uuid of its own.
	const id = "i-minted"
	if err := f.CreateRootDisk(t.Context(), hypervisor.RootDiskSpec{
		InstanceID: id, SourceVolume: "img-base", Pool: instancesPool, SizeGiB: 10,
	}); err != nil {
		t.Fatalf("CreateRootDisk(): %v", err)
	}
	if err := f.CreateSeedISO(t.Context(), instancesPool, fake.SeedISOName(id), hypervisor.CloudInit{
		MetaData: "instance-id: " + id + "\n", NetworkConfig: "version: 2\n",
	}); err != nil {
		t.Fatalf("CreateSeedISO(): %v", err)
	}
	if _, err := f.DefineAndStart(t.Context(), hypervisor.DomainSpec{
		ID:           id,
		Name:         id,
		VCPUs:        1,
		MemoryMiB:    512,
		RootDiskPool: instancesPool,
	}); err != nil {
		t.Fatalf("DefineAndStart(): %v", err)
	}

	info, err := f.GetDomain(t.Context(), id)
	if err != nil {
		t.Fatalf("GetDomain(): %v", err)
	}
	if info.UUID != fake.UUIDFor(id) {
		t.Errorf("uuid = %q, want %q", info.UUID, fake.UUIDFor(id))
	}
}

// TestUUIDForIsStableAndDistinct covers the two things a test relies on: the
// same domain always gets the same uuid, and two domains never share one.
func TestUUIDForIsStableAndDistinct(t *testing.T) {
	t.Parallel()

	first := fake.UUIDFor("i-web")
	if first != fake.UUIDFor("i-web") {
		t.Errorf("UUIDFor() = %q then %q, want a stable value", first, fake.UUIDFor("i-web"))
	}
	if second := fake.UUIDFor("i-db"); second == first {
		t.Errorf("two domains share the uuid %q", first)
	}

	if len(first) != 36 || first[8] != '-' || first[13] != '-' || first[18] != '-' || first[23] != '-' {
		t.Errorf("uuid = %q, want the 8-4-4-4-12 form", first)
	}
	if first[14] != '5' {
		t.Errorf("uuid = %q, want version 5 in the first nibble after the second dash", first)
	}
	if c := first[19]; c != '8' && c != '9' && c != 'a' && c != 'b' {
		t.Errorf("uuid = %q, want an RFC 4122 variant nibble after the third dash", first)
	}
}

func TestDomainLifecycle(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()
	info := launchInstance(t, f, "i-web", "img-base")

	if info.State != hypervisor.DomainStateRunning {
		t.Errorf("state after launch = %q, want running", info.State)
	}
	if info.IP != "192.168.100.10" {
		t.Errorf("ip = %q, want 192.168.100.10", info.IP)
	}
	if info.MAC != fake.MACFor("i-web") {
		t.Errorf("mac = %q, want %q", info.MAC, fake.MACFor("i-web"))
	}
	// The spec carries a uuid here, and the fake keeps it: libvirt does the same
	// for a definition that names one. A spec without one is covered by
	// TestDefineAndStartMintsAUuid.
	if info.UUID != "6f1c0d64-6f3f-4c3f-8a1d-0a9f3d1b2c3d" {
		t.Errorf("uuid = %q, want the one the spec named", info.UUID)
	}
	if info.VCPUs != 2 || info.MemoryMiB != 2048 {
		t.Errorf("vcpus/memory = %d/%d, want 2/2048", info.VCPUs, info.MemoryMiB)
	}

	steps := []struct {
		name  string
		act   func() error
		state hypervisor.DomainState
	}{
		{name: "shutdown", act: func() error { return f.Shutdown(ctx, "i-web") }, state: hypervisor.DomainStateShutoff},
		{name: "start", act: func() error { return f.Start(ctx, "i-web") }, state: hypervisor.DomainStateRunning},
		{name: "reboot", act: func() error { return f.Reboot(ctx, "i-web") }, state: hypervisor.DomainStateRunning},
		{name: "reset", act: func() error { return f.Reset(ctx, "i-web") }, state: hypervisor.DomainStateRunning},
		{name: "destroy", act: func() error { return f.Destroy(ctx, "i-web") }, state: hypervisor.DomainStateShutoff},
		{name: "destroy again is a no-op", act: func() error { return f.Destroy(ctx, "i-web") }, state: hypervisor.DomainStateShutoff},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			if err := step.act(); err != nil {
				t.Fatalf("%s error = %v", step.name, err)
			}
			got, err := f.GetDomain(ctx, "i-web")
			if err != nil {
				t.Fatalf("GetDomain() error = %v", err)
			}
			if got.State != step.state {
				t.Errorf("state = %q, want %q", got.State, step.state)
			}
		})
	}
}

func TestDomainTransitionsAreGuarded(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()
	launchInstance(t, f, "i-web", "img-base")

	tests := []struct {
		name      string
		id        string
		act       func(id string) error
		wantIsErr error
	}{
		{
			name:      "start a running domain",
			id:        "i-web",
			act:       func(id string) error { return f.Start(ctx, id) },
			wantIsErr: hypervisor.ErrInvalidState,
		},
		{
			name:      "shutdown an unknown domain",
			id:        "i-unknown",
			act:       func(id string) error { return f.Shutdown(ctx, id) },
			wantIsErr: hypervisor.ErrNotFound,
		},
		{
			name:      "reboot an unknown domain",
			id:        "i-unknown",
			act:       func(id string) error { return f.Reboot(ctx, id) },
			wantIsErr: hypervisor.ErrNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.act(tt.id); !errors.Is(err, tt.wantIsErr) {
				t.Fatalf("error = %v, want it to wrap %v", err, tt.wantIsErr)
			}
		})
	}

	if err := f.Destroy(ctx, "i-web"); err != nil {
		t.Fatalf("Destroy() error = %v", err)
	}
	if err := f.Reboot(ctx, "i-web"); !errors.Is(err, hypervisor.ErrInvalidState) {
		t.Errorf("Reboot() on a stopped domain error = %v, want ErrInvalidState", err)
	}
}

func TestUndefine(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()
	launchInstance(t, f, "i-web", "img-base")

	if err := f.Undefine(ctx, "i-web"); err != nil {
		t.Fatalf("Undefine() error = %v", err)
	}
	if _, err := f.GetDomain(ctx, "i-web"); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("GetDomain() after undefine error = %v, want ErrNotFound", err)
	}
	if err := f.Undefine(ctx, "i-web"); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("second Undefine() error = %v, want ErrNotFound", err)
	}

	// The volumes are kept: only the domain definition is gone.
	if err := f.DeleteVolume(ctx, instancesPool, fake.RootDiskName("i-web")); err != nil {
		t.Errorf("the root disk should survive an undefine, DeleteVolume() error = %v", err)
	}
}

func TestListDomains(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()
	launchInstance(t, f, "i-b", "img-a")
	launchInstance(t, f, "i-a", "img-b")

	domains, err := f.ListDomains(ctx)
	if err != nil {
		t.Fatalf("ListDomains() error = %v", err)
	}
	if len(domains) != 2 {
		t.Fatalf("len(domains) = %d, want 2", len(domains))
	}
	if domains[0].ID != "i-a" || domains[1].ID != "i-b" {
		t.Errorf("ids = %q, %q, want i-a, i-b", domains[0].ID, domains[1].ID)
	}
}

func TestConsoleOutput(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()
	launchInstance(t, f, "i-web", "img-base")

	full, err := f.ConsoleOutput(ctx, "i-web", 0)
	if err != nil {
		t.Fatalf("ConsoleOutput() error = %v", err)
	}
	if !strings.Contains(string(full), "booting web") {
		t.Errorf("console = %q, want it to mention the boot", full)
	}

	tail, err := f.ConsoleOutput(ctx, "i-web", 8)
	if err != nil {
		t.Fatalf("ConsoleOutput() error = %v", err)
	}
	if len(tail) != 8 {
		t.Errorf("len(tail) = %d, want 8", len(tail))
	}
	if string(tail) != string(full[len(full)-8:]) {
		t.Errorf("tail = %q, want the last 8 bytes of the console", tail)
	}

	if _, err := f.ConsoleOutput(ctx, "i-unknown", 0); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("ConsoleOutput() on an unknown domain error = %v, want ErrNotFound", err)
	}
}

func TestCapacity(t *testing.T) {
	t.Parallel()

	f := fake.New(fake.Config{Capacity: hypervisor.Capacity{
		VCPUs:       16,
		MemoryBytes: 64 << 30,
		DiskBytes:   1 << 40,
	}})

	capacity, err := f.Capacity(t.Context())
	if err != nil {
		t.Fatalf("Capacity() error = %v", err)
	}
	want := hypervisor.Capacity{VCPUs: 16, MemoryBytes: 64 << 30, DiskBytes: 1 << 40}
	if capacity != want {
		t.Errorf("Capacity() = %+v, want %+v", capacity, want)
	}
}

func TestEvents(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	events, err := f.Events(ctx)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}

	launchInstance(t, f, "i-web", "img-base")
	if err := f.Shutdown(ctx, "i-web"); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	want := []hypervisor.DomainEvent{
		{ID: "i-web", Action: hypervisor.ActionDefined, State: hypervisor.DomainStateRunning},
		{ID: "i-web", Action: hypervisor.ActionStarted, State: hypervisor.DomainStateRunning},
		{ID: "i-web", Action: hypervisor.ActionShutdown, State: hypervisor.DomainStateShutoff},
	}
	for _, expected := range want {
		select {
		case got := <-events:
			if got != expected {
				t.Errorf("event = %+v, want %+v", got, expected)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event %+v", expected)
		}
	}
}

func TestEventsCloseOnCancel(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx, cancel := context.WithCancel(t.Context())

	events, err := f.Events(ctx)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	cancel()

	select {
	case _, open := <-events:
		if open {
			t.Error("the channel is still open after canceling the context")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the event channel was not closed after canceling the context")
	}
}

func TestInjectedFailures(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()

	t.Run("fail next", func(t *testing.T) {
		f.FailNext(hypervisor.OpImageInfo, errBoom)
		if _, err := f.ImageInfo(ctx, "img-base"); !errors.Is(err, errBoom) {
			t.Fatalf("error = %v, want %v", err, errBoom)
		}
		importImage(t, f, "img-base")
		if _, err := f.ImageInfo(ctx, "img-base"); err != nil {
			t.Fatalf("the failure was not one-shot, ImageInfo() error = %v", err)
		}
	})

	t.Run("fail always", func(t *testing.T) {
		f.FailAlways(hypervisor.OpDefineAndStart, errBoom)
		_, err := f.DefineAndStart(ctx, hypervisor.DomainSpec{
			ID:           "i-web",
			RootDiskPool: instancesPool,
		})
		if !errors.Is(err, errBoom) {
			t.Fatalf("error = %v, want %v", err, errBoom)
		}
	})
}

func TestLatencyIsHonoured(t *testing.T) {
	t.Parallel()

	f := fake.New(fake.Config{Latency: 40 * time.Millisecond})

	start := time.Now()
	if err := f.Ping(t.Context()); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("Ping() took %v, want at least the configured 40ms", elapsed)
	}
}

func TestCancelledContext(t *testing.T) {
	t.Parallel()

	f := fake.New(fake.Config{Latency: time.Second})

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := f.Ping(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Ping() error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Ping() took %v, want it to return as soon as the context is canceled", elapsed)
	}
}

func TestClearResetsState(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()
	launchInstance(t, f, "i-web", "img-base")

	f.Clear()

	if _, err := f.GetDomain(ctx, "i-web"); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("GetDomain() after Clear() error = %v, want ErrNotFound", err)
	}
	if _, err := f.ImageInfo(ctx, "img-base"); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("ImageInfo() after Clear() error = %v, want ErrNotFound", err)
	}
}

func TestConcurrentUse(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	ctx := t.Context()
	launchInstance(t, f, "i-web", "img-base")

	const goroutines = 8
	done := make(chan struct{}, goroutines)

	for i := range goroutines {
		go func() {
			defer func() { done <- struct{}{} }()

			if i%2 == 0 {
				_, _ = f.ListDomains(ctx) //nolint:errcheck // the race detector is the assertion
				return
			}
			_, _ = f.GetDomain(ctx, "i-web") //nolint:errcheck // the race detector is the assertion
		}()
	}
	for range goroutines {
		<-done
	}
}
