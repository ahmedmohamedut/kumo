package libvirt

import (
	"encoding/xml"
	"errors"
	"slices"
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/libvirt/domainxml"
)

// errDestroyBoom is an unrelated failure for the destroyError test: the mapping
// has to keep a cause that is not a domain one, so a caller can still match it.
var errDestroyBoom = errors.New("the connection dropped")

func TestDomainStateMapsEveryLibvirtState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state golibvirt.DomainState
		want  hypervisor.DomainState
	}{
		{name: "running", state: golibvirt.DomainRunning, want: hypervisor.DomainStateRunning},
		{name: "blocked", state: golibvirt.DomainBlocked, want: hypervisor.DomainStateShutoff},
		{name: "paused", state: golibvirt.DomainPaused, want: hypervisor.DomainStatePaused},
		{name: "shutdown", state: golibvirt.DomainShutdown, want: hypervisor.DomainStateShutoff},
		{name: "shut off", state: golibvirt.DomainShutoff, want: hypervisor.DomainStateShutoff},
		{name: "crashed", state: golibvirt.DomainCrashed, want: hypervisor.DomainStateCrashed},
		{name: "pmsuspended", state: golibvirt.DomainPmsuspended, want: hypervisor.DomainStateShutoff},
		{name: "no state", state: golibvirt.DomainNostate, want: hypervisor.DomainStateShutoff},
		// A state from a newer libvirt than this build knows about still has to
		// resolve to something, or a guest would read as "unknown" in the console.
		{name: "from the future", state: golibvirt.DomainState(99), want: hypervisor.DomainStateShutoff},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := domainState(int32(test.state)); got != test.want {
				t.Errorf("domainState(%d) = %q, want %q", test.state, got, test.want)
			}
		})
	}
}

func TestFirstPrivateSkipsWhatKumoCannotReach(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		addrs []golibvirt.DomainIPAddr
		want  string
	}{
		{name: "nothing", want: ""},
		{
			name:  "a plain address",
			addrs: []golibvirt.DomainIPAddr{{Addr: "192.168.122.42"}},
			want:  "192.168.122.42",
		},
		{
			// libvirt hands back the host side of the bridge and the qemu-internal
			// addresses first; a guest reporting one of those looks reachable while
			// being unreachable.
			name: "loopback first",
			addrs: []golibvirt.DomainIPAddr{
				{Addr: "127.0.0.1"},
				{Addr: "192.168.122.42"},
			},
			want: "192.168.122.42",
		},
		{
			name:  "the unspecified address",
			addrs: []golibvirt.DomainIPAddr{{Addr: "0.0.0.0"}},
			want:  "",
		},
		{
			name: "multicast",
			addrs: []golibvirt.DomainIPAddr{
				{Addr: "224.0.0.1"},
				{Addr: "192.168.122.43"},
			},
			want: "192.168.122.43",
		},
		{
			// An IPv6-only guest is not useless, but the address the console links
			// to is the IPv4 one, so a v6 address alone is not what is wanted here.
			name:  "ipv6 only",
			addrs: []golibvirt.DomainIPAddr{{Addr: "fe80::1"}},
			want:  "",
		},
		{
			name: "not an address at all",
			addrs: []golibvirt.DomainIPAddr{
				{Addr: "not-an-address"},
				{Addr: "192.168.122.44"},
			},
			want: "192.168.122.44",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := firstPrivate(test.addrs); got != test.want {
				t.Errorf("firstPrivate(%v) = %q, want %q", test.addrs, got, test.want)
			}
		})
	}
}

func TestUUIDStringRendersTheCanonicalForm(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		uuid golibvirt.UUID
		want string
	}{
		// The zero uuid is what libvirt hands back when a domain could not be
		// described, and it matches no domain, so it is rendered as nothing.
		{name: "the zero uuid", uuid: golibvirt.UUID{}, want: ""},
		{
			name: "the uuid of a launched guest",
			uuid: golibvirt.UUID{
				0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
				0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
			},
			want: "01234567-89ab-cdef-0123-456789abcdef",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := uuidString(test.uuid); got != test.want {
				t.Errorf("uuidString() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMemoryDescConvertsEveryUnitLibvirtWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		unit string
		// value as libvirt writes it
		value int
		want  int
	}{
		{name: "mib, the unit kumo writes", unit: "MiB", value: 2048, want: 2048},
		{name: "kib", unit: "KiB", value: 4 * 1024 * 1024, want: 4096},
		{name: "k", unit: "K", value: 2 * 1024 * 1024, want: 2048},
		{name: "gib", unit: "GiB", value: 4, want: 4096},
		{name: "mib spelled the short way", unit: "M", value: 3072, want: 3072},
		// A definition that left the unit out means bytes, and a MiB figure read as
		// bytes would be a million times too small.
		{name: "bytes spelled out", unit: "bytes", value: 2 << 30, want: 2048},
		// libvirt's schema defaults the unit to KiB, so a description without one
		// is read as KiB rather than as MiB.
		{name: "no unit means kib", unit: "", value: 2 * 1024 * 1024, want: 2048},
		{name: "no unit, too small for a mib", unit: "", value: 512, want: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := memoryDesc{Unit: test.unit, Value: test.value}.MiB()
			if got != test.want {
				t.Errorf("memoryDesc{%q, %d}.MiB() = %d, want %d", test.unit, test.value, got, test.want)
			}
		})
	}
}

// TestDomainDescReadsWhatKumoWrote closes the loop between the template and the
// reader: whatever the backend writes into a domain it has to be able to read
// back, or a launch would record an empty uuid, MAC or memory figure.
func TestDomainDescReadsWhatKumoWrote(t *testing.T) {
	t.Parallel()

	spec := hypervisor.DomainSpec{
		ID:             "i-0a1b2c3d4e5f60718",
		Name:           "i-0a1b2c3d4e5f60718",
		RootDiskPool:   "kumo-instances",
		RootDiskPath:   "/var/lib/libvirt/images/kumo-instances/root.qcow2",
		SeedISOPath:    "/var/lib/libvirt/images/kumo-instances/seed.iso",
		ConsoleLogPath: "/var/lib/kumo/console/i-0a1b2c3d4e5f60718.log",
		VCPUs:          2,
		MemoryMiB:      4096,
		Network:        "default",
	}

	document, err := domainxml.Build(spec)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	var doc domainDesc
	if err := xml.Unmarshal(document, &doc); err != nil {
		t.Fatalf("Unmarshal(): %v", err)
	}

	if doc.Name != spec.Name {
		t.Errorf("name = %q, want %q", doc.Name, spec.Name)
	}
	if doc.MemoryMiB() != spec.MemoryMiB {
		t.Errorf("memory = %d MiB, want %d", doc.MemoryMiB(), spec.MemoryMiB)
	}
	if doc.VCPUs() != spec.VCPUs {
		t.Errorf("vcpus = %d, want %d", doc.VCPUs(), spec.VCPUs)
	}
	// No MAC is written, because QEMU allocates one: libvirt hands it back on the
	// description and the reader has to find it there.
	if doc.MAC() != "" {
		t.Errorf("mac = %q, want none in the template", doc.MAC())
	}
	if got := doc.Devices.Interfaces; len(got) != 1 || got[0].Source.Network != spec.Network {
		t.Errorf("interfaces = %+v, want one on the network %q", got, spec.Network)
	}
}

// TestDomainDescReadsWhatLibvirtAddsBack covers the fields libvirt rewrites: the
// uuid and the MAC of a running guest, plus a vcpu count that is capped rather
// than fixed.
func TestDomainDescReadsWhatLibvirtAddsBack(t *testing.T) {
	t.Parallel()

	const described = `<domain type="kvm">
  <name>i-0a1b2c3d4e5f60718</name>
  <uuid>01020345-6789-abcd-ef01-23456789abcdef</uuid>
  <memory unit="KiB">4194304</memory>
  <vcpu current="1" placement="static">4</vcpu>
  <devices>
    <interface type="network">
      <mac address="52:54:00:ab:cd:ef"/>
      <source network="default"/>
    </interface>
    <interface type="bridge">
      <source bridge="virbr0"/>
    </interface>
  </devices>
</domain>`

	var doc domainDesc
	if err := xml.Unmarshal([]byte(described), &doc); err != nil {
		t.Fatalf("Unmarshal(): %v", err)
	}

	if doc.UUID != "01020345-6789-abcd-ef01-23456789abcdef" {
		t.Errorf("uuid = %q", doc.UUID)
	}
	if doc.MemoryMiB() != 4096 {
		t.Errorf("memory = %d MiB, want 4096", doc.MemoryMiB())
	}
	if doc.VCPUs() != 1 {
		t.Errorf("vcpus = %d, want the current 1 rather than the maximum 4", doc.VCPUs())
	}
	if doc.MAC() != "52:54:00:ab:cd:ef" {
		t.Errorf("mac = %q, want the first interface that has one", doc.MAC())
	}
}

// TestStoragePathsReadsEveryDiskUndefineHasToRemove covers the two disks Kumo
// defines and the shapes a description can carry: a block device, a network disk
// whose source is a name rather than a path, and a disk with no source at all.
// Undefine deletes what these paths name, so a path they fail to read is a file
// left behind after the instance is gone.
func TestStoragePathsReadsEveryDiskUndefineHasToRemove(t *testing.T) {
	t.Parallel()

	spec := hypervisor.DomainSpec{
		ID:             "i-0a1b2c3d4e5f60718",
		Name:           "i-0a1b2c3d4e5f60718",
		RootDiskPool:   "kumo-instances",
		RootDiskPath:   "/var/lib/libvirt/images/kumo-instances/root.qcow2",
		SeedISOPath:    "/var/lib/libvirt/images/kumo-instances/seed.iso",
		ConsoleLogPath: "/var/lib/kumo/console/i-0a1b2c3d4e5f60718.log",
		VCPUs:          2,
		MemoryMiB:      4096,
		Network:        "default",
	}

	document, err := domainxml.Build(spec)
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	var doc domainDesc
	if err := xml.Unmarshal(document, &doc); err != nil {
		t.Fatalf("Unmarshal(): %v", err)
	}
	if got, want := doc.StoragePaths(), []string{spec.RootDiskPath, spec.SeedISOPath}; !slices.Equal(got, want) {
		t.Errorf("StoragePaths() = %v, want %v", got, want)
	}

	const described = `<domain type="kvm">
  <name>i-0a1b2c3d4e5f60718</name>
  <devices>
    <disk type="file" device="disk">
      <source file="/var/lib/libvirt/images/kumo-instances/root.qcow2"/>
    </disk>
    <disk type="block" device="disk">
      <source dev="/dev/vg/data"/>
    </disk>
    <disk type="network" device="disk">
      <source protocol="iscsi" name="iqn.2026-01.example:disk"/>
    </disk>
    <disk type="file" device="cdrom">
      <target dev="sda" bus="sata"/>
    </disk>
  </devices>
</domain>`

	var rewritten domainDesc
	if err := xml.Unmarshal([]byte(described), &rewritten); err != nil {
		t.Fatalf("Unmarshal(): %v", err)
	}
	want := []string{
		"/var/lib/libvirt/images/kumo-instances/root.qcow2",
		"/dev/vg/data",
	}
	if got := rewritten.StoragePaths(); !slices.Equal(got, want) {
		t.Errorf("StoragePaths() = %v, want %v", got, want)
	}
}

// TestVCPUsFallsBackToTheMaximum covers a description with no current attribute,
// which an older libvirt writes.
func TestVCPUsFallsBackToTheMaximum(t *testing.T) {
	t.Parallel()

	const described = `<domain type="kvm"><vcpu>3</vcpu></domain>`

	var doc domainDesc
	if err := xml.Unmarshal([]byte(described), &doc); err != nil {
		t.Fatalf("Unmarshal(): %v", err)
	}
	if doc.VCPUs() != 3 {
		t.Errorf("vcpus = %d, want 3", doc.VCPUs())
	}
	if doc.MAC() != "" {
		t.Errorf("mac = %q, want none", doc.MAC())
	}
	if doc.MemoryMiB() != 0 {
		t.Errorf("memory = %d MiB, want 0", doc.MemoryMiB())
	}
}

// TestDestroyErrorTreatsAnOffDomainAsSuccess is the regression test for a
// terminate of a stopped instance: libvirt answers DomainDestroyFlags for a
// domain that is not running with VIR_ERR_OPERATION_INVALID ("Requested
// operation is not valid: domain is not running"), and a hard stop of something
// already off is the state the caller wanted, not a failure. A domain that
// vanished between the lookup and the call reads as ErrNotFound, which every
// cleanup path already tolerates; anything else is still an error.
func TestDestroyErrorTreatsAnOffDomainAsSuccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          error
		wantNil      bool
		wantNotFound bool
	}{
		{name: "no error", err: nil, wantNil: true},
		{
			name: "already off",
			err: golibvirt.Error{
				Code:    uint32(golibvirt.ErrOperationInvalid),
				Message: "Requested operation is not valid: domain is not running",
			},
			wantNil: true,
		},
		{
			name: "gone between the lookup and the call",
			err: golibvirt.Error{
				Code:    uint32(golibvirt.ErrNoDomain),
				Message: "no domain with name 'i-x'",
			},
			wantNotFound: true,
		},
		{name: "a real failure", err: errDestroyBoom},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := destroyError("i-x", test.err)
			switch {
			case test.wantNil:
				if got != nil {
					t.Errorf("destroyError() = %v, want nil", got)
				}
			case test.wantNotFound:
				if !errors.Is(got, hypervisor.ErrNotFound) {
					t.Errorf("destroyError() = %v, want ErrNotFound", got)
				}
			default:
				if got == nil {
					t.Fatal("destroyError() = nil, want an error")
				}
				// The cause is preserved, so a caller can still match on it.
				if !errors.Is(got, test.err) {
					t.Errorf("destroyError() = %v, want it to wrap %v", got, test.err)
				}
			}
		})
	}
}
