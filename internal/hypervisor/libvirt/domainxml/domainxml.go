// Package domainxml writes the libvirt domain definition for an instance.
//
// It is a pure function over hypervisor.DomainSpec: the same spec always produces
// the same XML, and nothing here talks to libvirt. That is what makes the
// definition reviewable in a golden file and safe to test on a machine with no
// KVM.
//
// The document is built from encoding/xml structs, never from string formatting,
// so every value that reaches the XML is escaped by the encoder. An operator's
// name cannot break out of an element and inject another.
//
// The shape follows docs/design/04-libvirt-integration.md §5.
package domainxml

import (
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
)

// Defaults for the parts of a spec that may be empty. Machine and Arch are
// per-instance in the spec because a later milestone may add other
// architectures; x86_64 with q35 is what M4 launches.
const (
	DefaultMachine = "q35"
	DefaultArch    = "x86_64"
)

// DefaultNetwork is the fallback network an instance attaches to when the VPC
// service hands it no network name: an adopted operator network keeps its own
// name, a legacy install used "kumo-default", and a freshly bootstrapped VPC is
// named after its id. The instance service always passes the default VPC's
// libvirt network, so this only ever appears in tests.
const DefaultNetwork = "kumo-default"

// MetadataNamespace is Kumo's own namespace inside the domain metadata. The
// display name travels there rather than in the domain name, because the domain
// name is the instance id that libvirt uses as a key.
const MetadataNamespace = "https://kumo.example/xmlns/1"

// domain is the root element. Everything M4 always emits is a value; the parts
// that are genuinely optional are pointers or slices.
type domain struct {
	XMLName   xml.Name `xml:"domain"`
	Type      string   `xml:"type,attr"`
	Name      string   `xml:"name"`
	UUID      string   `xml:"uuid"`
	Metadata  metadata `xml:"metadata"`
	Memory    memory   `xml:"memory"`
	VCPU      vcpu     `xml:"vcpu"`
	OS        os       `xml:"os"`
	Features  features `xml:"features"`
	CPU       cpu      `xml:"cpu"`
	Clock     clock    `xml:"clock"`
	OnPowerof string   `xml:"on_poweroff"`
	OnReboot  string   `xml:"on_reboot"`
	OnCrash   string   `xml:"on_crash"`
	Devices   devices  `xml:"devices"`
}

// metadata carries the operator-facing name.
//
// libvirt ignores an unknown namespace, which is the point: the name is there for
// Kumo and for anyone reading the host, and it changes nothing about how the
// domain behaves.
//
// The content is innerxml because a namespaced element is the one thing a struct
// tag cannot express. That makes the fragment Kumo's to build, and so makes the
// escaping of the name its responsibility too, which instanceMetadata does with
// the same encoder the rest of the document goes through.
type metadata struct {
	Inner string `xml:",innerxml"`
}

type memory struct {
	Unit string `xml:"unit,attr"`
	MiB  int    `xml:",chardata"`
}

type vcpu struct {
	Placement string `xml:"placement,attr"`
	Count     int    `xml:",chardata"`
}

type os struct {
	// Firmware is an attribute of <os> in libvirt, and stays empty in M4: UEFI
	// and aarch64 arrive with their own milestone.
	Firmware string `xml:"firmware,attr,omitempty"`
	Type     osType `xml:"type"`
	Boot     []boot `xml:"boot"`
}

type osType struct {
	Arch    string `xml:"arch,attr"`
	Machine string `xml:"machine,attr"`
	Value   string `xml:",chardata"`
}

type boot struct {
	Dev string `xml:"dev,attr"`
}

type features struct {
	ACPI emptyStruct `xml:"acpi"`
	APIC emptyStruct `xml:"apic"`
}

// emptyStruct marshals as an element with no attributes and no content, which is
// how libvirt writes <acpi/>, <apic/> and <readonly/>.
type emptyStruct struct{}

type cpu struct {
	Mode  string `xml:"mode,attr"`
	Check string `xml:"check,attr"`
}

type clock struct {
	Offset string  `xml:"offset,attr"`
	Timers []timer `xml:"timer"`
}

type timer struct {
	Name       string `xml:"name,attr"`
	TickPolicy string `xml:"tickpolicy,attr,omitempty"`
	Present    string `xml:"present,attr,omitempty"`
}

type devices struct {
	Disks      []disk     `xml:"disk"`
	Interface  iface      `xml:"interface"`
	Serial     serial     `xml:"serial"`
	Console    console    `xml:"console"`
	Channel    channel    `xml:"channel"`
	Graphics   graphics   `xml:"graphics"`
	Video      video      `xml:"video"`
	RNG        rng        `xml:"rng"`
	MemBalloon memBalloon `xml:"memballoon"`
}

type disk struct {
	Type     string       `xml:"type,attr"`
	Device   string       `xml:"device,attr"`
	Driver   driver       `xml:"driver"`
	Source   diskSource   `xml:"source"`
	Target   target       `xml:"target"`
	ReadOnly *emptyStruct `xml:"readonly,omitempty"`
}

type driver struct {
	Name    string `xml:"name,attr"`
	Type    string `xml:"type,attr"`
	Cache   string `xml:"cache,attr,omitempty"`
	Discard string `xml:"discard,attr,omitempty"`
}

type diskSource struct {
	File string `xml:"file,attr"`
}

type target struct {
	Dev string `xml:"dev,attr"`
	Bus string `xml:"bus,attr"`
}

type iface struct {
	Type   string      `xml:"type,attr"`
	Source ifaceSource `xml:"source"`
	Model  ifaceModel  `xml:"model"`
}

type ifaceSource struct {
	Network string `xml:"network,attr"`
}

type ifaceModel struct {
	Type string `xml:"type,attr"`
}

// serial is a pty so a guest that writes to the console never blocks on a full
// file, with a log file alongside it so the console-output endpoint has something
// to read. This is the choice §5 leaves to the implementation.
type serial struct {
	Type   string       `xml:"type,attr"`
	Target serialTarget `xml:"target"`
	Log    *log         `xml:"log,omitempty"`
}

type serialTarget struct {
	Port string `xml:"port,attr"`
}

// log is a child of <serial> since libvirt 1.3.
type log struct {
	File   string `xml:"file,attr"`
	Append string `xml:"append,attr"`
}

type console struct {
	Type   string        `xml:"type,attr"`
	Target consoleTarget `xml:"target"`
}

type consoleTarget struct {
	Type string `xml:"type,attr"`
	Port string `xml:"port,attr"`
}

// channel is the qemu-guest-agent socket, which is where the IP fallback of
// docs/design/04-libvirt-integration.md §6 reads from.
type channel struct {
	Type   string        `xml:"type,attr"`
	Target channelTarget `xml:"target"`
}

type channelTarget struct {
	Type string `xml:"type,attr"`
	Name string `xml:"name,attr"`
}

// graphics binds the display to loopback. Kumo reaches it through the
// authenticated proxy, never from the network.
type graphics struct {
	Type     string         `xml:"type,attr"`
	Port     string         `xml:"port,attr"`
	Autoport string         `xml:"autoport,attr"`
	Listen   graphicsListen `xml:"listen"`
}

type graphicsListen struct {
	Type    string `xml:"type,attr"`
	Address string `xml:"address,attr"`
}

type video struct {
	Model videoModel `xml:"model"`
}

type videoModel struct {
	Type string `xml:"type,attr"`
}

// rng gives the guest entropy without a hardware device.
type rng struct {
	Model   string     `xml:"model,attr"`
	Backend rngBackend `xml:"backend"`
}

type rngBackend struct {
	Model string `xml:"model,attr"`
	Value string `xml:",chardata"`
}

// memBalloon lets the host reclaim memory from an idle guest.
type memBalloon struct {
	Model string `xml:"model,attr"`
}

// Build renders the domain XML for a spec.
func Build(spec hypervisor.DomainSpec) ([]byte, error) {
	if err := Validate(spec); err != nil {
		return nil, err
	}

	doc := domain{
		Type: "kvm",
		Name: spec.ID,
		UUID: spec.UUID,
		Memory: memory{
			Unit: "MiB",
			MiB:  spec.MemoryMiB,
		},
		VCPU: vcpu{
			Placement: "static",
			Count:     spec.VCPUs,
		},
		OS: os{
			Firmware: spec.Firmware,
			Type: osType{
				Arch:    arch(spec),
				Machine: machine(spec),
				Value:   "hvm",
			},
			Boot: []boot{{Dev: "hd"}},
		},
		Features: features{},
		CPU:      cpu{Mode: "host-passthrough", Check: "none"},
		Clock: clock{
			Offset: "utc",
			Timers: []timer{
				{Name: "rtc", TickPolicy: "catchup"},
				{Name: "pit", TickPolicy: "delay"},
				{Name: "hpet", Present: "no"},
			},
		},
		OnPowerof: "destroy",
		OnReboot:  "restart",
		OnCrash:   "destroy",
		Devices: devices{
			Disks: []disk{
				{
					Type:   "file",
					Device: "disk",
					Driver: driver{
						Name:    "qemu",
						Type:    "qcow2",
						Cache:   "none",
						Discard: "unmap",
					},
					Source: diskSource{File: spec.RootDiskPath},
					Target: target{Dev: "vda", Bus: "virtio"},
				},
				{
					Type:   "file",
					Device: "cdrom",
					Driver: driver{Name: "qemu", Type: "raw"},
					Source: diskSource{File: spec.SeedISOPath},
					Target: target{Dev: "sda", Bus: "sata"},
					// The seed is read once at boot. A writable seed is a seed a
					// guest can corrupt with a write to its CD-ROM.
					ReadOnly: &emptyStruct{},
				},
			},
			Interface: iface{
				Type:   "network",
				Source: ifaceSource{Network: network(spec)},
				Model:  ifaceModel{Type: "virtio"},
			},
			Serial: serial{
				Type:   "pty",
				Target: serialTarget{Port: "0"},
				Log:    consoleLog(spec.ConsoleLogPath),
			},
			Console: console{
				Type:   "pty",
				Target: consoleTarget{Type: "serial", Port: "0"},
			},
			Channel: channel{
				Type:   "unix",
				Target: channelTarget{Type: "virtio", Name: "org.qemu.guest_agent.0"},
			},
			Graphics: graphics{
				Type:     "vnc",
				Port:     "-1",
				Autoport: "yes",
				Listen:   graphicsListen{Type: "address", Address: "127.0.0.1"},
			},
			Video:      video{Model: videoModel{Type: "virtio"}},
			RNG:        rng{Model: "virtio", Backend: rngBackend{Model: "random", Value: "/dev/urandom"}},
			MemBalloon: memBalloon{Model: "virtio"},
		},
	}

	doc.Metadata = metadata{Inner: instanceMetadataXML(spec.Name)}

	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("domainxml: marshal the domain: %w", err)
	}
	return []byte(xml.Header + string(out) + "\n"), nil
}

// instanceMetadataXML renders the kumo:instance element.
//
// The name goes through the encoder, so a name containing markup is data and not
// structure.
func instanceMetadataXML(instanceName string) string {
	var escaped strings.Builder
	if err := xml.EscapeText(&escaped, []byte(instanceName)); err != nil {
		// EscapeText cannot fail on a strings.Builder, and returning empty would
		// be worse than the panic of assuming it never happens: build returns an
		// error instead, and the caller retries with a different name.
		return ""
	}

	return fmt.Sprintf("<kumo:instance xmlns:kumo=%q><kumo:name>%s</kumo:name></kumo:instance>",
		MetadataNamespace, escaped.String())
}

// Validate refuses a spec that could only produce a domain libvirt would reject,
// or one that would launch something nobody asked for.
//
// A missing path is refused rather than defaulted: a domain with no root disk is
// a domain that boots to nothing, and the failure would surface minutes later as
// an instance stuck in pending.
func Validate(spec hypervisor.DomainSpec) error {
	switch {
	case spec.ID == "":
		return fmt.Errorf("%w: the domain needs an id", hypervisor.ErrInvalidSpec)
	case spec.Name == "":
		return fmt.Errorf("%w: the domain %s needs a name", hypervisor.ErrInvalidSpec, spec.ID)
	case spec.VCPUs < 1:
		return fmt.Errorf("%w: the domain %s needs at least one vCPU, got %d",
			hypervisor.ErrInvalidSpec, spec.ID, spec.VCPUs)
	case spec.MemoryMiB < 1:
		return fmt.Errorf("%w: the domain %s needs at least one MiB of memory, got %d",
			hypervisor.ErrInvalidSpec, spec.ID, spec.MemoryMiB)
	case spec.RootDiskPath == "":
		return fmt.Errorf("%w: the domain %s needs a root disk path", hypervisor.ErrInvalidSpec, spec.ID)
	case spec.SeedISOPath == "":
		return fmt.Errorf("%w: the domain %s needs a seed ISO path", hypervisor.ErrInvalidSpec, spec.ID)
	}
	return nil
}

// consoleLog returns the console log element, or nil when no path was given: a
// serial with no log is valid, a serial logging to an empty path is not.
func consoleLog(path string) *log {
	if path == "" {
		return nil
	}
	return &log{File: path, Append: "on"}
}

// arch returns the spec's architecture, or the M4 default.
func arch(spec hypervisor.DomainSpec) string {
	if spec.Arch == "" {
		return DefaultArch
	}
	return spec.Arch
}

// machine returns the spec's machine type, or the M4 default.
func machine(spec hypervisor.DomainSpec) string {
	if spec.Machine == "" {
		return DefaultMachine
	}
	return spec.Machine
}

// network returns the spec's network, or the one EnsureInfra creates.
func network(spec hypervisor.DomainSpec) string {
	if spec.Network == "" {
		return DefaultNetwork
	}
	return spec.Network
}
