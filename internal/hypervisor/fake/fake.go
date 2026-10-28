// Package fake provides an in-memory hypervisor: a domain state machine with
// volumes, console output, host capacity, configurable latency and injectable
// failures. It backs `KUMO_HYPERVISOR=fake` and every service test, so tests
// never need real libvirt.
package fake

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
)

// Defaults for the fake host.
const (
	firstHostOffset = 10

	gib = 1 << 30

	// GiB is a byte count of one gibibyte, exported so a test can assert on the
	// size of a volume without repeating the constant.
	GiB = gib

	// consoleLimit caps the console buffer kept per domain.
	consoleLimit = 64 << 10
	// eventBuffer is the per-subscriber queue depth. Events are dropped rather
	// than blocking a lifecycle transition when a subscriber falls behind.
	eventBuffer = 64
)

// Sentinel errors for calls the fake rejects outright. They mark a programming
// error in the caller rather than a backend failure.
var (
	// ErrInvalidSpec is returned when a caller passes an incomplete spec. It is
	// the shared sentinel so a caller can match it without knowing which backend
	// answered.
	ErrInvalidSpec = hypervisor.ErrInvalidSpec
	// ErrEmptyImage is returned when an image carries no bytes.
	ErrEmptyImage = errors.New("fake: image is empty")
	// ErrImageTooLarge is returned when an image is bigger than the declared
	// size.
	ErrImageTooLarge = errors.New("fake: image is larger than the declared size")
	// ErrFormatMismatch is returned when the declared format disagrees with the
	// header, which would otherwise produce a volume that cannot boot.
	ErrFormatMismatch = errors.New("fake: image format mismatch")
)

// Config configures a Fake. The zero value is usable.
type Config struct {
	// Subnet the fake hands out private addresses from. Default 192.168.100.0/24.
	// An empty or unparsable value falls back to the default.
	Subnet string
	// Capacity reported by Capacity. Defaults to 8 vCPUs, 16 GiB, 250 GiB.
	Capacity hypervisor.Capacity
	// Latency is simulated per operation, for timeout and ordering tests.
	Latency time.Duration
	// IgnoreShutdown makes Shutdown accept the request and leave the domain
	// running, the way a guest without an ACPI handler does. libvirt answers the
	// same way: the request is delivered and nothing happens. It exists so the
	// watchdog of a graceful stop can be tested without waiting out the real one.
	IgnoreShutdown bool
}

// Fake is an in-memory hypervisor.Hypervisor. It is safe for concurrent use.
type Fake struct {
	mu sync.Mutex

	subnet         netip.Prefix
	capacity       hypervisor.Capacity
	latency        time.Duration
	ignoreShutdown bool
	nextHost       uint32
	leaseDelay     int
	volumes        map[string]volume
	domains        map[string]*domain
	networks       map[string]*fakeNetwork
	infrastructure hypervisor.InfraConfig
	failOnce       map[hypervisor.Op]error
	failAlways     map[hypervisor.Op]error
	subs           map[*subscriber]struct{}
}

type volume struct {
	pool             string
	name             string
	format           hypervisor.ImageFormat
	sizeBytes        int64
	virtualSizeBytes int64
	backing          string
	seed             string
	content          []byte
}

type domain struct {
	info    hypervisor.DomainInfo
	console []byte
	// leasePolls counts the GetDomain calls this domain has answered without an
	// address yet. It is only used when LeaseDelay is set.
	leasePolls int
	// lease is the address this domain gets once its polls run out, empty when
	// the fake hands out addresses at definition time.
	lease string
}

// fakeNetwork is one defined libvirt network, with the static dhcp hosts Kumo
// has told dnsmasq to serve.
type fakeNetwork struct {
	info   hypervisor.NetworkInfo
	active bool
	// dhcpHosts is keyed by MAC, the way DeleteDHCPHost finds a host.
	dhcpHosts map[string]hypervisor.DHCPHost
}

// subscriber is one Events() consumer.
type subscriber struct {
	ch chan hypervisor.DomainEvent
}

// New returns a Fake with the given configuration applied.
func New(cfg Config) *Fake {
	subnet := defaultPrefix()
	if cfg.Subnet != "" {
		if parsed, err := netip.ParsePrefix(cfg.Subnet); err == nil {
			subnet = parsed
		}
	}

	capacity := cfg.Capacity
	if capacity.VCPUs <= 0 || capacity.MemoryBytes <= 0 || capacity.DiskBytes <= 0 {
		capacity = hypervisor.Capacity{
			VCPUs:       8,
			MemoryBytes: 16 * gib,
			DiskBytes:   250 * gib,
		}
	}

	return &Fake{
		subnet:         subnet,
		capacity:       capacity,
		latency:        cfg.Latency,
		ignoreShutdown: cfg.IgnoreShutdown,
		nextHost:       firstHostOffset,
		volumes:        make(map[string]volume),
		domains:        make(map[string]*domain),
		networks:       make(map[string]*fakeNetwork),
		failOnce:       make(map[hypervisor.Op]error),
		failAlways:     make(map[hypervisor.Op]error),
		subs:           make(map[*subscriber]struct{}),
	}
}

// Clear returns the fake to its initial state, keeping the configuration. It
// exists so tests can share one Fake across cases.
func (f *Fake) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.volumes = make(map[string]volume)
	f.domains = make(map[string]*domain)
	f.networks = make(map[string]*fakeNetwork)
	f.infrastructure = hypervisor.InfraConfig{}
	f.nextHost = firstHostOffset
	f.failOnce = make(map[hypervisor.Op]error)
	f.failAlways = make(map[hypervisor.Op]error)
}

// FailNext makes the next call to op return err.
func (f *Fake) FailNext(op hypervisor.Op, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failOnce[op] = err
}

// FailAlways makes every call to op return err.
func (f *Fake) FailAlways(op hypervisor.Op, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failAlways[op] = err
}

// Ping reports whether the fake is reachable. It only fails when configured to.
func (f *Fake) Ping(ctx context.Context) error {
	return f.begin(ctx, hypervisor.OpPing)
}

// EnsureInfra records the pools and network it was asked to create. It is
// idempotent and does not touch the host filesystem.
func (f *Fake) EnsureInfra(ctx context.Context, cfg hypervisor.InfraConfig) error {
	if err := f.begin(ctx, hypervisor.OpEnsureInfra); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.infrastructure = cfg
	return nil
}

// ImportImage copies r into a volume of the images pool, enforcing SizeBytes as
// the upper bound. The declared format is cross-checked against the header, so a
// workflow that lies about the format is caught here rather than at boot.
func (f *Fake) ImportImage(ctx context.Context, name string, r io.Reader, spec hypervisor.ImportSpec) error {
	if name == "" {
		return fmt.Errorf("%w: image name must not be empty", ErrInvalidSpec)
	}
	if err := f.begin(ctx, hypervisor.OpImportImage); err != nil {
		return err
	}

	content, err := readImage(r, spec.SizeBytes)
	if err != nil {
		return err
	}
	if len(content) == 0 {
		return fmt.Errorf("%w: image %q", ErrEmptyImage, name)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	key := volumeKey(f.infrastructure.ImagesPool, name)
	if _, exists := f.volumes[key]; exists {
		return fmt.Errorf("fake: volume %q: %w", name, hypervisor.ErrAlreadyExists)
	}
	detected := detectFormat(content)
	if spec.Format != "" && detected != spec.Format {
		return fmt.Errorf("%w: the image is %s, the caller declared %s",
			ErrFormatMismatch, detected, spec.Format)
	}

	f.volumes[key] = volume{
		pool:             f.infrastructure.ImagesPool,
		name:             name,
		format:           detected,
		sizeBytes:        int64(len(content)),
		virtualSizeBytes: virtualSize(spec.VirtualSizeBytes, int64(len(content))),
		content:          content,
	}
	return nil
}

// ImageInfo returns the format and sizes of an image volume.
func (f *Fake) ImageInfo(ctx context.Context, name string) (hypervisor.ImageInfo, error) {
	if err := f.begin(ctx, hypervisor.OpImageInfo); err != nil {
		return hypervisor.ImageInfo{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	vol, ok := f.volumes[volumeKey(f.infrastructure.ImagesPool, name)]
	if !ok {
		return hypervisor.ImageInfo{}, fmt.Errorf("fake: volume %q: %w", name, hypervisor.ErrNotFound)
	}
	return hypervisor.ImageInfo{
		Name:             vol.name,
		Pool:             vol.pool,
		Format:           vol.format,
		SizeBytes:        vol.sizeBytes,
		VirtualSizeBytes: vol.virtualSizeBytes,
	}, nil
}

// DeleteVolume removes a volume. It reports hypervisor.ErrNotFound when the
// volume is already gone; services decide whether that is tolerated.
func (f *Fake) DeleteVolume(ctx context.Context, pool, name string) error {
	if err := f.begin(ctx, hypervisor.OpDeleteVolume); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	key := volumeKey(pool, name)
	if _, ok := f.volumes[key]; !ok {
		return fmt.Errorf("fake: volume %q in pool %q: %w", name, pool, hypervisor.ErrNotFound)
	}
	delete(f.volumes, key)
	return nil
}

// CreateRootDisk creates the copy-on-write overlay of an instance on top of an
// image volume.
func (f *Fake) CreateRootDisk(ctx context.Context, spec hypervisor.RootDiskSpec) error {
	if spec.InstanceID == "" || spec.SourceVolume == "" || spec.Pool == "" {
		return fmt.Errorf("%w: a root disk needs an instance id, a source volume and a pool", ErrInvalidSpec)
	}
	if spec.SizeGiB <= 0 {
		return fmt.Errorf("%w: root disk size must be positive, got %d GiB", ErrInvalidSpec, spec.SizeGiB)
	}
	if err := f.begin(ctx, hypervisor.OpCreateRootDisk); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.volumes[volumeKey(f.infrastructure.ImagesPool, spec.SourceVolume)]; !ok {
		return fmt.Errorf("fake: source volume %q: %w", spec.SourceVolume, hypervisor.ErrNotFound)
	}

	name := RootDiskName(spec.InstanceID)
	key := volumeKey(spec.Pool, name)
	if _, ok := f.volumes[key]; ok {
		return fmt.Errorf("fake: root disk %q: %w", name, hypervisor.ErrAlreadyExists)
	}
	f.volumes[key] = volume{
		pool:             spec.Pool,
		name:             name,
		format:           hypervisor.FormatQCOW2,
		sizeBytes:        1 << 20,
		virtualSizeBytes: spec.SizeGiB * gib,
		backing:          spec.SourceVolume,
	}
	return nil
}

// CreateDataDisk creates the blank data disk of a DB instance. It has no
// source volume, so unlike a root disk there is nothing to look up first.
func (f *Fake) CreateDataDisk(ctx context.Context, spec hypervisor.DataDiskSpec) error {
	if spec.DBInstanceID == "" || spec.Pool == "" {
		return fmt.Errorf("%w: a data disk needs a DB instance id and a pool", ErrInvalidSpec)
	}
	if spec.SizeGiB <= 0 {
		return fmt.Errorf("%w: data disk size must be positive, got %d GiB", ErrInvalidSpec, spec.SizeGiB)
	}
	if err := f.begin(ctx, hypervisor.OpCreateDataDisk); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	name := hypervisor.DataDiskName(spec.DBInstanceID)
	key := volumeKey(spec.Pool, name)
	if _, ok := f.volumes[key]; ok {
		return fmt.Errorf("fake: data disk %q: %w", name, hypervisor.ErrAlreadyExists)
	}
	f.volumes[key] = volume{
		pool:             spec.Pool,
		name:             name,
		format:           hypervisor.FormatQCOW2,
		sizeBytes:        1 << 20,
		virtualSizeBytes: spec.SizeGiB * gib,
		// No backing: the guest formats it, nothing is cloned in.
	}
	return nil
}

// CreateSeedISO stores the rendered NoCloud seed for an instance in the pool it
// was told to use, which is the instances pool beside the root disks.
func (f *Fake) CreateSeedISO(ctx context.Context, pool, name string, seed hypervisor.CloudInit) error {
	if name == "" {
		return fmt.Errorf("%w: seed iso name must not be empty", ErrInvalidSpec)
	}
	if pool == "" {
		return fmt.Errorf("%w: a seed iso needs a pool", ErrInvalidSpec)
	}
	instanceID := instanceIDFromMetaData(seed.MetaData)
	if instanceID == "" {
		return fmt.Errorf("%w: a seed iso needs meta-data with an instance-id", ErrInvalidSpec)
	}
	if err := f.begin(ctx, hypervisor.OpCreateSeedISO); err != nil {
		return err
	}

	rendered := []byte(RenderSeed(seed))

	f.mu.Lock()
	defer f.mu.Unlock()

	key := volumeKey(pool, name)
	if _, ok := f.volumes[key]; ok {
		return fmt.Errorf("fake: seed iso %q: %w", name, hypervisor.ErrAlreadyExists)
	}
	f.volumes[key] = volume{
		pool:             pool,
		name:             name,
		format:           hypervisor.FormatRaw,
		sizeBytes:        int64(len(rendered)),
		virtualSizeBytes: int64(len(rendered)),
		seed:             instanceID,
		content:          rendered,
	}
	return nil
}

// DefineAndStart defines a domain and boots it. It reports
// hypervisor.ErrAlreadyExists when the id is known and hypervisor.ErrNotFound
// when the instance root disk is missing.
func (f *Fake) DefineAndStart(ctx context.Context, spec hypervisor.DomainSpec) (hypervisor.DomainInfo, error) {
	if spec.ID == "" {
		return hypervisor.DomainInfo{}, fmt.Errorf("%w: domain id must not be empty", ErrInvalidSpec)
	}
	if err := f.begin(ctx, hypervisor.OpDefineAndStart); err != nil {
		return hypervisor.DomainInfo{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.domains[spec.ID]; ok {
		return hypervisor.DomainInfo{}, fmt.Errorf("fake: domain %q: %w", spec.ID, hypervisor.ErrAlreadyExists)
	}
	if _, ok := f.volumes[volumeKey(spec.RootDiskPool, RootDiskName(spec.ID))]; !ok {
		return hypervisor.DomainInfo{}, fmt.Errorf("fake: root disk for domain %q: %w",
			spec.ID, hypervisor.ErrNotFound)
	}

	// libvirt assigns a uuid when the XML does not carry one, so the fake does
	// too: a launched instance that records no uuid would look wrong against a
	// real backend.
	uuid := spec.UUID
	if uuid == "" {
		uuid = UUIDFor(spec.ID)
	}

	// The MAC is the one the launch allocated: the domain must come up with
	// exactly the MAC dnsmasq serves for its static host entry, or the guest
	// lease never arrives. A launch that predates interfaces lets libvirt pick,
	// so the fake derives one the way MACFor does.
	mac := spec.MACAddress
	if mac == "" {
		mac = MACFor(spec.ID)
	}

	// The address is allocated once, at definition time. When the spec names a
	// network with a static host for this domain's MAC, the lease is that host's
	// address: dnsmasq serves exactly what Kumo allocated. Otherwise the fake
	// hands out the next subnet address. Either way the address is held back
	// from the domain until enough polls have passed: a caller that waits for a
	// lease has to actually wait, without anyone else taking the address
	// meanwhile.
	address := f.allocateIPLocked()
	if net, ok := f.networks[spec.Network]; ok && net.active {
		if host, ok := net.dhcpHosts[mac]; ok {
			address = host.IP
		}
	}
	lease := address
	if f.leaseDelay > 0 {
		lease = ""
	}

	d := &domain{
		info: hypervisor.DomainInfo{
			ID:        spec.ID,
			Name:      spec.Name,
			UUID:      uuid,
			State:     hypervisor.DomainStateRunning,
			IP:        lease,
			MAC:       mac,
			VCPUs:     spec.VCPUs,
			MemoryMiB: spec.MemoryMiB,
		},
		lease: address,
	}
	d.appendConsoleLocked(fmt.Sprintf("[    0.000000] Linux version 6.6.0 (kumo-fake) booting %s\n", spec.Name))
	f.domains[spec.ID] = d

	f.emitLocked(hypervisor.DomainEvent{
		ID:     spec.ID,
		Action: hypervisor.ActionDefined,
		State:  hypervisor.DomainStateRunning,
	})
	f.emitLocked(hypervisor.DomainEvent{
		ID:     spec.ID,
		Action: hypervisor.ActionStarted,
		State:  hypervisor.DomainStateRunning,
	})

	return d.info, nil
}

// Start boots a shut-off domain.
func (f *Fake) Start(ctx context.Context, id string) error {
	if err := f.begin(ctx, hypervisor.OpStart); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	d, ok := f.domains[id]
	if !ok {
		return fmt.Errorf("fake: domain %q: %w", id, hypervisor.ErrNotFound)
	}
	if d.info.State != hypervisor.DomainStateShutoff {
		return fmt.Errorf("fake: domain %q is %s, not shut off: %w",
			id, d.info.State, hypervisor.ErrInvalidState)
	}

	d.info.State = hypervisor.DomainStateRunning
	d.appendConsoleLocked("[    1.204881] kumo-fake: domain resumed\n")
	f.emitLocked(hypervisor.DomainEvent{
		ID:     id,
		Action: hypervisor.ActionStarted,
		State:  hypervisor.DomainStateRunning,
	})
	return nil
}

// Shutdown asks a running domain to power off. It reports
// hypervisor.ErrInvalidState for a domain that is not running, mirroring the
// "domain is not running" error the libvirt backend returns.
func (f *Fake) Shutdown(ctx context.Context, id string) error {
	if err := f.begin(ctx, hypervisor.OpShutdown); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	d, ok := f.domains[id]
	if !ok {
		return fmt.Errorf("fake: domain %q: %w", id, hypervisor.ErrNotFound)
	}
	if d.info.State != hypervisor.DomainStateRunning {
		return fmt.Errorf("fake: domain %q is %s, not running: %w",
			id, d.info.State, hypervisor.ErrInvalidState)
	}
	if f.ignoreShutdown {
		// The request was accepted and nothing happened, which is what libvirt
		// answers for a guest that does not listen on the ACPI channel.
		d.appendConsoleLocked("[    2.884120] kumo-fake: poweroff requested by ACPI (ignored)\n")
		return nil
	}

	d.info.State = hypervisor.DomainStateShutoff
	d.appendConsoleLocked("[    2.884120] kumo-fake: poweroff requested by ACPI\n")
	f.emitLocked(hypervisor.DomainEvent{
		ID:     id,
		Action: hypervisor.ActionShutdown,
		State:  hypervisor.DomainStateShutoff,
	})
	return nil
}

// Destroy stops a domain immediately. Stopping an already stopped domain
// succeeds, like libvirt.
func (f *Fake) Destroy(ctx context.Context, id string) error {
	if err := f.begin(ctx, hypervisor.OpDestroy); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	d, ok := f.domains[id]
	if !ok {
		return fmt.Errorf("fake: domain %q: %w", id, hypervisor.ErrNotFound)
	}
	if d.info.State == hypervisor.DomainStateShutoff {
		return nil
	}

	d.info.State = hypervisor.DomainStateShutoff
	d.appendConsoleLocked("[    0.512004] kumo-fake: power forced off\n")
	f.emitLocked(hypervisor.DomainEvent{
		ID:     id,
		Action: hypervisor.ActionStopped,
		State:  hypervisor.DomainStateShutoff,
	})
	return nil
}

// Reboot asks a running domain to restart.
func (f *Fake) Reboot(ctx context.Context, id string) error {
	return f.reboot(ctx, hypervisor.OpReboot, id, "guest reboot requested")
}

// Reset forces a running domain to restart.
func (f *Fake) Reset(ctx context.Context, id string) error {
	return f.reboot(ctx, hypervisor.OpReset, id, "domain reset")
}

func (f *Fake) reboot(ctx context.Context, op hypervisor.Op, id, message string) error {
	if err := f.begin(ctx, op); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	d, ok := f.domains[id]
	if !ok {
		return fmt.Errorf("fake: domain %q: %w", id, hypervisor.ErrNotFound)
	}
	if d.info.State != hypervisor.DomainStateRunning {
		return fmt.Errorf("fake: domain %q is %s, not running: %w",
			id, d.info.State, hypervisor.ErrInvalidState)
	}

	d.appendConsoleLocked(fmt.Sprintf("[    3.112004] kumo-fake: %s\n", message))
	f.emitLocked(hypervisor.DomainEvent{
		ID:     id,
		Action: hypervisor.ActionReboot,
		State:  hypervisor.DomainStateRunning,
	})
	return nil
}

// Undefine removes a domain definition and the storage volumes the domain
// declares: the root disk and the seed ISO go with it, the way
// `virsh undefine --remove-all-storage --nvram` does. The image volume a root
// disk reads from is shared, never the domain's own, so it stays.
func (f *Fake) Undefine(ctx context.Context, id string) error {
	if err := f.begin(ctx, hypervisor.OpUndefine); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.domains[id]; !ok {
		return fmt.Errorf("fake: domain %q: %w", id, hypervisor.ErrNotFound)
	}
	delete(f.domains, id)
	for _, name := range []string{hypervisor.RootDiskName(id), hypervisor.SeedISOName(id)} {
		delete(f.volumes, volumeKey(f.infrastructure.InstancesPool, name))
	}
	f.emitLocked(hypervisor.DomainEvent{
		ID:     id,
		Action: hypervisor.ActionUndefine,
		State:  hypervisor.DomainStateShutoff,
	})
	return nil
}

// GetDomain returns the runtime state of a domain.
func (f *Fake) GetDomain(ctx context.Context, id string) (hypervisor.DomainInfo, error) {
	if err := f.begin(ctx, hypervisor.OpGetDomain); err != nil {
		return hypervisor.DomainInfo{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	d, ok := f.domains[id]
	if !ok {
		return hypervisor.DomainInfo{}, fmt.Errorf("fake: domain %q: %w", id, hypervisor.ErrNotFound)
	}

	info := d.info
	if info.IP == "" && d.lease != "" {
		d.leasePolls++
		if d.leasePolls > f.leaseDelay {
			info.IP = d.lease
		}
	}
	return info, nil
}

// ListDomains returns every domain, ordered by id.
func (f *Fake) ListDomains(ctx context.Context) ([]hypervisor.DomainInfo, error) {
	if err := f.begin(ctx, hypervisor.OpListDomains); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	domains := make([]hypervisor.DomainInfo, 0, len(f.domains))
	for _, d := range f.domains {
		domains = append(domains, d.info)
	}
	sort.Slice(domains, func(i, j int) bool { return domains[i].ID < domains[j].ID })
	return domains, nil
}

// ConsoleOutput returns the tail of a domain's console log.
func (f *Fake) ConsoleOutput(ctx context.Context, id string, maxBytes int) ([]byte, error) {
	if err := f.begin(ctx, hypervisor.OpConsoleOutput); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	d, ok := f.domains[id]
	if !ok {
		return nil, fmt.Errorf("fake: domain %q: %w", id, hypervisor.ErrNotFound)
	}
	if maxBytes <= 0 || maxBytes > len(d.console) {
		return append([]byte(nil), d.console...), nil
	}
	return append([]byte(nil), d.console[len(d.console)-maxBytes:]...), nil
}

// WriteConsole appends to the console buffer of a domain, the way a guest writing
// to its serial port would.
//
// The lifecycle operations write a line or two each, which is not enough to test
// anything about a console that serves the tail of a long log: the real thing is
// a file that has been growing for days.
func (f *Fake) WriteConsole(_ context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	d, ok := f.domains[id]
	if !ok {
		return fmt.Errorf("fake: domain %q: %w", id, hypervisor.ErrNotFound)
	}
	d.appendConsoleLocked(text)
	return nil
}

// Crash puts a domain into the crashed state, which is a guest whose qemu died
// rather than one anybody asked to stop.
//
// Nothing in Kumo produces it: it arrives from the hypervisor, which is exactly
// why the reconciler has to be testable against it
// (docs/design/00-architecture.md section 4).
func (f *Fake) Crash(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	d, ok := f.domains[id]
	if !ok {
		return fmt.Errorf("fake: domain %q: %w", id, hypervisor.ErrNotFound)
	}

	d.info.State = hypervisor.DomainStateCrashed
	d.appendConsoleLocked("[    0.904112] kumo-fake: guest crashed\n")
	f.emitLocked(hypervisor.DomainEvent{
		ID:     id,
		Action: hypervisor.ActionCrashed,
		State:  hypervisor.DomainStateCrashed,
	})
	return nil
}

// SetAddress replaces the address a domain reports, the way a guest that renews
// its lease into a different address would. An empty ip drops the address, which
// is a guest whose lease expired.
func (f *Fake) SetAddress(_ context.Context, id, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	d, ok := f.domains[id]
	if !ok {
		return fmt.Errorf("fake: domain %q: %w", id, hypervisor.ErrNotFound)
	}

	d.info.IP = ip
	d.lease = ip
	return nil
}

// Capacity returns the fake host's totals.
func (f *Fake) Capacity(ctx context.Context) (hypervisor.Capacity, error) {
	if err := f.begin(ctx, hypervisor.OpCapacity); err != nil {
		return hypervisor.Capacity{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.capacity, nil
}

// Events streams domain lifecycle events until ctx is canceled.
func (f *Fake) Events(ctx context.Context) (<-chan hypervisor.DomainEvent, error) {
	if err := f.begin(ctx, hypervisor.OpEvents); err != nil {
		return nil, err
	}

	sub := &subscriber{ch: make(chan hypervisor.DomainEvent, eventBuffer)}

	f.mu.Lock()
	f.subs[sub] = struct{}{}
	f.mu.Unlock()

	go func() {
		<-ctx.Done()

		f.mu.Lock()
		defer f.mu.Unlock()

		if _, ok := f.subs[sub]; ok {
			delete(f.subs, sub)
			close(sub.ch)
		}
	}()

	return sub.ch, nil
}

// Networks: one VPC is one defined libvirt network with its static dhcp hosts,
// mirroring docs/design/04-libvirt-integration.md §6.

// DefineNetwork records a network definition. It reports
// hypervisor.ErrAlreadyExists for a name that is already known, like libvirt.
func (f *Fake) DefineNetwork(ctx context.Context, spec hypervisor.NetworkSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("%w: a network name must not be empty", ErrInvalidSpec)
	}
	if spec.Bridge == "" {
		return fmt.Errorf("%w: the network %s needs a bridge interface", ErrInvalidSpec, spec.Name)
	}
	if err := f.begin(ctx, hypervisor.OpDefineNetwork); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.networks[spec.Name]; ok {
		return fmt.Errorf("fake: the network %q: %w", spec.Name, hypervisor.ErrAlreadyExists)
	}
	f.networks[spec.Name] = &fakeNetwork{
		info: hypervisor.NetworkInfo{
			Name:    spec.Name,
			Bridge:  spec.Bridge,
			CIDR:    spec.CIDR,
			Gateway: spec.Gateway,
			Mode:    networkMode(spec.Mode),
			Domain:  spec.Domain,
		},
		dhcpHosts: make(map[string]hypervisor.DHCPHost),
	}
	return nil
}

// StartNetwork activates a defined network. Activating an active one is an
// invalid state, the same answer virNetworkCreate gives for a running network.
func (f *Fake) StartNetwork(ctx context.Context, name string) error {
	if err := f.begin(ctx, hypervisor.OpStartNetwork); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	net, ok := f.networks[name]
	if !ok {
		return fmt.Errorf("fake: the network %q: %w", name, hypervisor.ErrNotFound)
	}
	if net.active {
		return fmt.Errorf("fake: the network %q is already active: %w", name, hypervisor.ErrInvalidState)
	}
	net.active = true
	return nil
}

// DestroyNetwork stops an active network. Stopping one that is not active is an
// invalid state.
func (f *Fake) DestroyNetwork(ctx context.Context, name string) error {
	if err := f.begin(ctx, hypervisor.OpDestroyNetwork); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	net, ok := f.networks[name]
	if !ok {
		return fmt.Errorf("fake: the network %q: %w", name, hypervisor.ErrNotFound)
	}
	if !net.active {
		return fmt.Errorf("fake: the network %q is not active: %w", name, hypervisor.ErrInvalidState)
	}
	net.active = false
	return nil
}

// UndefineNetwork removes a network's definition. An active network cannot be
// undefined, so the delete task destroys it first.
func (f *Fake) UndefineNetwork(ctx context.Context, name string) error {
	if err := f.begin(ctx, hypervisor.OpUndefineNetwork); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	net, ok := f.networks[name]
	if !ok {
		return fmt.Errorf("fake: the network %q: %w", name, hypervisor.ErrNotFound)
	}
	if net.active {
		return fmt.Errorf("fake: the network %q is still active: %w", name, hypervisor.ErrInvalidState)
	}
	delete(f.networks, name)
	return nil
}

// GetNetwork returns the state of a network, static hosts included.
func (f *Fake) GetNetwork(ctx context.Context, name string) (hypervisor.NetworkInfo, error) {
	if err := f.begin(ctx, hypervisor.OpGetNetwork); err != nil {
		return hypervisor.NetworkInfo{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	net, ok := f.networks[name]
	if !ok {
		return hypervisor.NetworkInfo{}, fmt.Errorf("fake: the network %q: %w", name, hypervisor.ErrNotFound)
	}
	return f.networkInfoLocked(net), nil
}

// AddDHCPHost installs a static host entry in an active network. A duplicate
// MAC or a MAC-in-the-same-network whose address is already spoken for is
// refused: dnsmasq would have to choose between two answers.
func (f *Fake) AddDHCPHost(ctx context.Context, name string, host hypervisor.DHCPHost) error {
	if host.MAC == "" {
		return fmt.Errorf("%w: a dhcp host needs a mac", ErrInvalidSpec)
	}
	if err := f.begin(ctx, hypervisor.OpAddDHCPHost); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	net, ok := f.networks[name]
	if !ok {
		return fmt.Errorf("fake: the network %q: %w", name, hypervisor.ErrNotFound)
	}
	if !net.active {
		return fmt.Errorf("fake: the network %q is not active: %w", name, hypervisor.ErrInvalidState)
	}
	if _, taken := net.dhcpHosts[host.MAC]; taken {
		return fmt.Errorf("fake: the dhcp host %s is already on the network %q: %w",
			host.MAC, name, hypervisor.ErrAlreadyExists)
	}
	for _, existing := range net.dhcpHosts {
		if existing.IP == host.IP {
			return fmt.Errorf("fake: the address %s is already on the network %q: %w",
				host.IP, name, hypervisor.ErrAlreadyExists)
		}
	}
	net.dhcpHosts[host.MAC] = host
	return nil
}

// DeleteDHCPHost removes a static host entry by MAC. An entry that is already
// gone is a not-found error; the terminate workflow tolerates it.
func (f *Fake) DeleteDHCPHost(ctx context.Context, name, mac string) error {
	if err := f.begin(ctx, hypervisor.OpDeleteDHCPHost); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	net, ok := f.networks[name]
	if !ok {
		return fmt.Errorf("fake: the network %q: %w", name, hypervisor.ErrNotFound)
	}
	if _, ok := net.dhcpHosts[mac]; !ok {
		return fmt.Errorf("fake: the dhcp host %s of the network %q: %w", mac, name, hypervisor.ErrNotFound)
	}
	delete(net.dhcpHosts, mac)
	return nil
}

// networkInfoLocked snapshots a network with its hosts in a stable order.
// Callers must hold the lock.
func (f *Fake) networkInfoLocked(net *fakeNetwork) hypervisor.NetworkInfo {
	info := net.info
	info.Active = net.active
	info.DHCPHosts = make([]hypervisor.DHCPHost, 0, len(net.dhcpHosts))
	for _, host := range net.dhcpHosts {
		info.DHCPHosts = append(info.DHCPHosts, host)
	}
	sort.Slice(info.DHCPHosts, func(i, j int) bool {
		if info.DHCPHosts[i].MAC != info.DHCPHosts[j].MAC {
			return info.DHCPHosts[i].MAC < info.DHCPHosts[j].MAC
		}
		return info.DHCPHosts[i].IP < info.DHCPHosts[j].IP
	})
	return info
}

// Networks lists every defined network with its state, ordered by name, so a
// test can assert the whole set at once.
func (f *Fake) Networks() []hypervisor.NetworkInfo {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]hypervisor.NetworkInfo, 0, len(f.networks))
	for _, net := range f.networks {
		out = append(out, f.networkInfoLocked(net))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DHCPHosts lists the static host entries of a network, ordered by MAC, the way
// a launch test asserts what the service told dnsmasq.
func (f *Fake) DHCPHosts(network string) []hypervisor.DHCPHost {
	f.mu.Lock()
	defer f.mu.Unlock()

	net, ok := f.networks[network]
	if !ok {
		return nil
	}
	return f.networkInfoLocked(net).DHCPHosts
}

// networkMode normalizes an empty mode to the API default, nat, the same
// default the real builder applies.
func networkMode(mode hypervisor.NetworkMode) hypervisor.NetworkMode {
	if mode == "" {
		return hypervisor.NetworkModeNAT
	}
	return mode
}

// begin runs the preamble of every operation: simulated latency, context
// cancellation and injected failures.
func (f *Fake) begin(ctx context.Context, op hypervisor.Op) error {
	f.mu.Lock()
	latency := f.latency
	f.mu.Unlock()

	if latency > 0 {
		timer := time.NewTimer(latency)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-ctx.Done():
			return fmt.Errorf("fake: %s: %w", op, ctx.Err())
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fake: %s: %w", op, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.failAlways[op]; ok {
		return err
	}
	if err, ok := f.failOnce[op]; ok {
		delete(f.failOnce, op)
		return err
	}
	return nil
}

// emitLocked queues an event for every subscriber, dropping it if a subscriber
// is full. Callers must hold the lock.
func (f *Fake) emitLocked(event hypervisor.DomainEvent) {
	for sub := range f.subs {
		select {
		case sub.ch <- event:
		default:
		}
	}
}

// allocateIPLocked hands out the next address of the subnet. Callers must hold
// the lock.
func (f *Fake) allocateIPLocked() string {
	base := f.subnet.Addr().As4()
	host := f.nextHost
	f.nextHost++

	var ip [4]byte
	copy(ip[:], base[:])
	ip[3] = byte(host)
	return netip.AddrFrom4(ip).String()
}

// appendConsoleLocked keeps the tail of the console log. Callers must hold the
// lock.
func (d *domain) appendConsoleLocked(line string) {
	d.console = append(d.console, line...)
	if len(d.console) > consoleLimit {
		d.console = d.console[len(d.console)-consoleLimit:]
	}
}

// VolumePath reports where a volume lives, as the libvirt backend does. The fake
// has no filesystem, so it hands back the path a directory pool would use: a
// service test asserts the domain spec it passed, not a file.
func (f *Fake) VolumePath(_ context.Context, pool, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := volumeKey(pool, name)
	if _, ok := f.volumes[key]; !ok {
		return "", fmt.Errorf("fake: the volume %q in %q: %w", name, pool, hypervisor.ErrNotFound)
	}

	cfg := f.infrastructure
	dir := filepath.Join(cfg.DataDir, "pools", "instances")
	if pool == cfg.ImagesPool {
		dir = filepath.Join(cfg.DataDir, "pools", "images")
	}
	return filepath.Join(dir, name), nil
}

// RootDiskName is the volume name of an instance's root disk, as
// docs/design/04-libvirt-integration.md §2 names it.
func RootDiskName(instanceID string) string { return hypervisor.RootDiskName(instanceID) }

// SeedISOName is the volume name of an instance's NoCloud seed.
func SeedISOName(instanceID string) string { return hypervisor.SeedISOName(instanceID) }

// Volume is one volume the fake holds. It exists so a service test can assert
// what a failed launch left behind, which is the whole point of the rollback.
type Volume struct {
	Pool             string
	Name             string
	Format           hypervisor.ImageFormat
	SizeBytes        int64
	VirtualSizeBytes int64
	// Backing is the image volume a root disk is a delta of, empty otherwise.
	Backing string
	// Seed is the instance id a NoCloud seed belongs to, empty otherwise.
	Seed string
	// Content is the rendered seed document, empty otherwise.
	Content string
}

// Volumes lists the volumes the fake holds, ordered by pool and then name, so a
// test can compare a whole set without sorting it itself.
func (f *Fake) Volumes() []Volume {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Volume, 0, len(f.volumes))
	for _, v := range f.volumes {
		out = append(out, Volume{
			Pool:             v.pool,
			Name:             v.name,
			Format:           v.format,
			SizeBytes:        v.sizeBytes,
			VirtualSizeBytes: v.virtualSizeBytes,
			Backing:          v.backing,
			Seed:             v.seed,
			Content:          string(v.content),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pool != out[j].Pool {
			return out[i].Pool < out[j].Pool
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// LeaseDelay makes a domain report no address for the first n GetDomain calls,
// which is what a guest that has not finished its DHCP handshake looks like. It
// exists so the launch workflow's wait for an address can be tested without
// sleeping: the delay counts polls, not seconds.
func (f *Fake) LeaseDelay(polls int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.leaseDelay = polls
	for _, d := range f.domains {
		d.leasePolls = 0
	}
}

// defaultPrefix is the subnet a zero-value Config falls back to.
func defaultPrefix() netip.Prefix {
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, 100, 0}), 24)
}

// UUIDFor derives a stable UUID from a domain id. It is not random, so a test can
// assert the uuid a launch recorded without depending on a value that changes on
// every run.
func UUIDFor(domainID string) string {
	sum := sha256.Sum256([]byte("kumo-fake-domain:" + domainID))

	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50 // version 5, derived from a name
	b[8] = (b[8] & 0x3f) | 0x80 // the RFC 4122 variant

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// MACFor derives a stable, locally administered MAC from an instance id.
func MACFor(instanceID string) string {
	mac := [6]byte{0x52, 0x54, 0x00}
	for i, b := range []byte(instanceID) {
		mac[3+i%3] ^= b
	}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", mac[0], mac[1], mac[2], mac[3], mac[4], mac[5])
}

// RenderSeed concatenates the seed documents the way the ISO builder in
// internal/seediso lays them out, but as plain text: the fake stores what a
// service test can read, not a real ISO.
//
// The documents arrive already rendered, so this only joins them. The
// YAML separators are what would separate the files inside the image.
func RenderSeed(seed hypervisor.CloudInit) string {
	var b strings.Builder

	for _, document := range []string{seed.MetaData, seed.NetworkConfig, seed.UserData} {
		if document == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("---\n")
		}
		b.WriteString(document)
		if !strings.HasSuffix(document, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// instanceIDFromMetaData reads the instance-id out of a meta-data document. The
// fake does it the way cloud-init does, by looking at the first lines, because a
// seed that names no instance is one it cannot attribute to a guest.
func instanceIDFromMetaData(metaData string) string {
	for line := range strings.Lines(metaData) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "instance-id:"); ok {
			return strings.TrimSpace(rest)
		}
		return ""
	}
	return ""
}

// readImage reads r, refusing to store more than size bytes when size is
// positive.
func readImage(r io.Reader, size int64) ([]byte, error) {
	if size <= 0 {
		content, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("fake: read image: %w", err)
		}
		return content, nil
	}

	content, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil {
		return nil, fmt.Errorf("fake: read image: %w", err)
	}
	if int64(len(content)) > size {
		return nil, fmt.Errorf("%w: got more than %d bytes", ErrImageTooLarge, size)
	}
	return content, nil
}

// virtualSize prefers the caller-declared size and falls back to what was read.
func virtualSize(declared, actual int64) int64 {
	if declared > 0 {
		return declared
	}
	return actual
}

func volumeKey(pool, name string) string {
	return pool + "/" + name
}

// detectFormat mirrors the qcow2 header sniffing the real backend does.
func detectFormat(content []byte) hypervisor.ImageFormat {
	if len(content) >= 4 && string(content[:4]) == "QFI\xfb" {
		return hypervisor.FormatQCOW2
	}
	return hypervisor.FormatRaw
}
