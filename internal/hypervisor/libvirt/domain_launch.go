package libvirt

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/libvirt/domainxml"
)

// DefineAndStart writes the domain definition and boots it.
//
// The two steps are separate libvirt calls, and the caller has to be able to
// undo both: a domain that was defined but never started is an orphan the
// reconciler would otherwise report forever. The service treats DefineAndStart
// as one step and calls Undefine on failure, which is why the domain name is the
// instance id (docs/design/04-libvirt-integration.md §3).
//
// Because the name is the instance id, a domain that already resolves to it
// belongs to an earlier attempt that was not rolled back (a crashed launch, or
// an attempt whose ephemeral volumes were already cleaned up). libvirt refuses
// to redefine a domain that is already defined, even when it is inactive, so
// such a definition is adopted instead of replaced: an inactive one is started
// right here, and a running one is reported as hypervisor.ErrAlreadyExists,
// which the service adopts by reading it back.
func (c *Client) DefineAndStart(ctx context.Context, spec hypervisor.DomainSpec) (hypervisor.DomainInfo, error) {
	document, err := domainxml.Build(spec)
	if err != nil {
		return hypervisor.DomainInfo{}, err
	}

	var info hypervisor.DomainInfo
	err = c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		dom, err := lookupDomain(conn, spec.ID)
		switch {
		case errors.Is(err, hypervisor.ErrNotFound):
			dom, err = conn.DomainDefineXML(string(document))
			if err != nil {
				return fmt.Errorf("define the domain %q: %w", spec.ID, err)
			}
			// The definition succeeded, so from here a failure leaves a domain
			// behind and has to be undone by the caller.
		case err != nil:
			return err
		default:
			state, _, err := conn.DomainGetState(dom, 0)
			if err != nil {
				return fmt.Errorf("read the state of the domain %q: %w", spec.ID, err)
			}
			if state == int32(golibvirt.DomainRunning) {
				// A launch is genuinely already under way; let the service adopt
				// the running domain instead of starting a second one.
				return fmt.Errorf("the domain %q: %w", spec.ID, hypervisor.ErrAlreadyExists)
			}
			if state != int32(golibvirt.DomainShutoff) {
				// Paused, crashed or finishing domains mesh badly with the
				// launch workflow; failing here lets the service's rollback
				// probe pick the domain up and clean it up.
				return fmt.Errorf("the domain %q is in state %d and cannot be adopted: %w",
					spec.ID, state, hypervisor.ErrInvalidState)
			}
			c.logger.InfoContext(ctx, "libvirt: adopting the definition of an earlier attempt",
				"domain", spec.ID)
		}

		if err := conn.DomainCreate(dom); err != nil {
			return fmt.Errorf("start the domain %q: %w", spec.ID, err)
		}
		if err := checkDomainExists(conn, spec.ID); err != nil {
			// libvirt answered with a handle for a domain it has already lost,
			// which happens when the guest exits immediately. Reporting it beats
			// returning a handle to nothing.
			return err
		}

		info = hypervisor.DomainInfo{
			ID:        spec.ID,
			Name:      spec.Name,
			UUID:      spec.UUID,
			State:     hypervisor.DomainStateRunning,
			VCPUs:     spec.VCPUs,
			MemoryMiB: spec.MemoryMiB,
		}

		// The MAC is allocated by QEMU, so it only exists once the domain is
		// running. It is read from the description libvirt hands back rather than
		// assumed, because the row keeps it for the console's VNC link.
		descriptor, err := conn.DomainGetXMLDesc(dom, 0)
		if err != nil {
			return fmt.Errorf("describe the domain %q after starting it: %w", spec.ID, err)
		}
		var doc domainDesc
		if err := xml.Unmarshal([]byte(descriptor), &doc); err != nil {
			return fmt.Errorf("parse the description of the domain %q: %w", spec.ID, err)
		}
		info.MAC = doc.MAC()
		info.UUID = doc.UUID
		return nil
	})
	if err != nil {
		return hypervisor.DomainInfo{}, err
	}

	return info, nil
}

// GetDomain reports the runtime state of a domain, including its address.
//
// The address is not part of DomainGetState: it comes from the DHCP lease libvirt
// tracks for the domain, and only exists once the guest has asked for one. A
// caller polling for an address therefore has to poll, which is what the launch
// workflow does.
func (c *Client) GetDomain(ctx context.Context, id string) (hypervisor.DomainInfo, error) {
	if id == "" {
		return hypervisor.DomainInfo{}, fmt.Errorf("%w: a domain id must not be empty", hypervisor.ErrInvalidSpec)
	}

	var info hypervisor.DomainInfo
	err := c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		dom, err := lookupDomain(conn, id)
		if err != nil {
			return err
		}

		state, _, err := conn.DomainGetState(dom, 0)
		if err != nil {
			return fmt.Errorf("read the state of the domain %q: %w", id, err)
		}

		descriptor, err := conn.DomainGetXMLDesc(dom, 0)
		if err != nil {
			return fmt.Errorf("describe the domain %q: %w", id, err)
		}
		var doc domainDesc
		if err := xml.Unmarshal([]byte(descriptor), &doc); err != nil {
			return fmt.Errorf("parse the description of the domain %q: %w", id, err)
		}

		info = hypervisor.DomainInfo{
			ID:        id,
			Name:      doc.Name,
			UUID:      doc.UUID,
			State:     domainState(state),
			MAC:       doc.MAC(),
			VCPUs:     doc.VCPUs(),
			MemoryMiB: doc.MemoryMiB(),
		}

		address, err := domainAddress(conn, dom)
		if err != nil {
			// An address is not worth failing a state read over: a guest with no
			// lease yet is running, and the next poll may have one.
			c.logger.DebugContext(ctx, "libvirt: the domain has no address yet",
				"domain", id, "error", err)
			return nil
		}
		info.IP = address
		return nil
	})
	if err != nil {
		return hypervisor.DomainInfo{}, err
	}

	return info, nil
}

// ListDomains returns every domain libvirt knows about. The reconciler uses it to
// find the ones Kumo has no row for.
func (c *Client) ListDomains(ctx context.Context) ([]hypervisor.DomainInfo, error) {
	var out []hypervisor.DomainInfo

	err := c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		domains, _, err := conn.ConnectListAllDomains(0, 0)
		if err != nil {
			return fmt.Errorf("list the domains: %w", err)
		}

		out = make([]hypervisor.DomainInfo, 0, len(domains))
		for _, dom := range domains {
			state, _, err := conn.DomainGetState(dom, 0)
			if err != nil {
				// One domain that cannot be read must not hide the rest.
				c.logger.WarnContext(ctx, "libvirt: could not read a domain, skipping it",
					"domain", dom.Name, "error", err)
				continue
			}
			out = append(out, hypervisor.DomainInfo{
				ID:    dom.Name,
				Name:  dom.Name,
				UUID:  uuidString(dom.UUID),
				State: domainState(state),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// Destroy stops a domain without asking the guest, the way the rollback of a
// failed launch needs: an instance that never finished booting has no ACPI
// channel to shut down through.
func (c *Client) Destroy(ctx context.Context, id string) error {
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		dom, err := lookupDomain(conn, id)
		if err != nil {
			return err
		}
		if err := conn.DomainDestroyFlags(dom, 0); err != nil {
			return fmt.Errorf("destroy the domain %q: %w", id, err)
		}
		return nil
	})
}

// Undefine removes a definition and everything the definition owns: the NVRAM
// file and every storage volume the domain declares. It is the equivalent of
// `virsh undefine --remove-all-storage --nvram`, which has no single libvirt
// call behind it either — virsh undefines with VIR_DOMAIN_UNDEFINE_NVRAM and
// then deletes the volumes it read out of the domain XML, and so does this.
//
// A domain that is still running is stopped first, because libvirt refuses to
// undefine a live domain and a rollback must not stop at that.
func (c *Client) Undefine(ctx context.Context, id string) error {
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		dom, err := lookupDomain(conn, id)
		if err != nil {
			return err
		}

		state, _, err := conn.DomainGetState(dom, 0)
		if err != nil {
			return fmt.Errorf("read the state of the domain %q: %w", id, err)
		}
		if state != int32(golibvirt.DomainShutoff) {
			if err := conn.DomainDestroyFlags(dom, 0); err != nil && !isNotFound(err) {
				return fmt.Errorf("stop the domain %q before undefining it: %w", id, err)
			}
		}

		// The volumes are read while the definition still exists: a domain that
		// is gone describes nothing, and the names of its disks are the only
		// thing that says which files are about to be orphaned.
		document, err := conn.DomainGetXMLDesc(dom, 0)
		if err != nil {
			return fmt.Errorf("describe the domain %q before undefining it: %w", id, err)
		}
		var desc domainDesc
		if err := xml.Unmarshal([]byte(document), &desc); err != nil {
			return fmt.Errorf("parse the description of the domain %q: %w", id, err)
		}

		if err := conn.DomainUndefineFlags(dom, golibvirt.DomainUndefineNvram); err != nil {
			if isNoDomain(err) || isNotFound(err) {
				return fmt.Errorf("the domain %q: %w", id, hypervisor.ErrNotFound)
			}
			return fmt.Errorf("undefine the domain %q: %w", id, err)
		}

		return c.deleteVolumes(ctx, conn, id, desc.StoragePaths())
	})
}

// deleteVolumes removes the volumes the disks of an undefined domain point at.
//
// A source libvirt does not manage — a block device, a file in a directory no
// pool covers — is reported and skipped, the way virsh prints "not managed by
// libvirt, remove it manually" and carries on: neither of us can delete what we
// cannot look up. A volume that is found but refuses to go is an error, because
// leaving a guest's disk behind is exactly what --remove-all-storage promises
// not to do.
func (c *Client) deleteVolumes(
	ctx context.Context, conn *golibvirt.Libvirt, id string, paths []string,
) error {
	var refused []error
	for _, path := range paths {
		vol, err := conn.StorageVolLookupByPath(path)
		if err != nil {
			c.logger.InfoContext(ctx, "libvirt: a disk of the domain is not a managed volume",
				"domain", id, "source", path, "error", err)
			continue
		}
		if err := conn.StorageVolDelete(vol, golibvirt.StorageVolDeleteNormal); err != nil {
			refused = append(refused, fmt.Errorf("the volume at %q: %w", path, err))
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("remove the storage of the domain %q: %w", id, errors.Join(refused...))
	}
	return nil
}

// lookupDomain resolves an instance id to a libvirt handle. The domain name is
// the instance id (docs/design/04-libvirt-integration.md §2), which is what makes
// an instance recoverable from its row alone, with no extra index to keep.
func lookupDomain(conn *golibvirt.Libvirt, id string) (golibvirt.Domain, error) {
	dom, err := conn.DomainLookupByName(id)
	if err != nil {
		if isNoDomain(err) || isNotFound(err) {
			return golibvirt.Domain{}, fmt.Errorf("the domain %q: %w", id, hypervisor.ErrNotFound)
		}
		return golibvirt.Domain{}, fmt.Errorf("look the domain %q up: %w", id, err)
	}
	return dom, nil
}

// checkDomainExists confirms a handle from DomainCreate still resolves. It
// catches a guest that exited while its definition was being written.
func checkDomainExists(conn *golibvirt.Libvirt, id string) error {
	_, err := conn.DomainLookupByName(id)
	if err != nil {
		if isNoDomain(err) || isNotFound(err) {
			return fmt.Errorf("the domain %q disappeared right after it was started: %w",
				id, hypervisor.ErrNotFound)
		}
		return fmt.Errorf("look the domain %q up after starting it: %w", id, err)
	}
	return nil
}

// domainAddress returns the first usable address of a domain.
//
// The lease is tried first and the qemu-guest-agent second, as
// docs/design/04-libvirt-integration.md §6 says: a lease needs only DHCP, the
// agent needs a guest package, and the DHCP range of the NAT network is what
// every cloud image does on first boot.
func domainAddress(conn *golibvirt.Libvirt, dom golibvirt.Domain) (string, error) {
	sources := []struct {
		name   string
		source golibvirt.DomainInterfaceAddressesSource
	}{
		{"dhcp lease", golibvirt.DomainInterfaceAddressesSrcLease},
		{"guest agent", golibvirt.DomainInterfaceAddressesSrcAgent},
	}

	for _, src := range sources {
		ifaces, err := conn.DomainInterfaceAddresses(dom, uint32(src.source), 0)
		if err != nil {
			// The agent is usually absent, so its failure is expected and the
			// lease has already been tried.
			continue
		}
		for _, iface := range ifaces {
			if address := firstPrivate(iface.Addrs); address != "" {
				return address, nil
			}
		}
	}

	return "", fmt.Errorf("%w: the domain has no address", hypervisor.ErrNotFound)
}

// firstPrivate returns the first IPv4 address of a libvirt interface that Kumo
// can route to.
//
// Loopback is skipped: it is the address of the host side of a bridge, and an
// instance that reported one would look reachable while being unreachable.
func firstPrivate(addrs []golibvirt.DomainIPAddr) string {
	for _, addr := range addrs {
		parsed, err := netip.ParseAddr(addr.Addr)
		if err != nil || !parsed.Is4() {
			continue
		}
		if parsed.IsLoopback() || parsed.IsMulticast() || parsed.IsUnspecified() {
			continue
		}
		return parsed.String()
	}
	return ""
}

// uuidString renders a libvirt UUID as the canonical text form Kumo stores in the
// instances row.
//
// The zero UUID is rendered as no uuid at all: libvirt leaves it zeroed when a
// domain could not be described, and storing 00000000-0000-0000-0000-000000000000
// would put a value in the row that matches no domain.
func uuidString(id golibvirt.UUID) string {
	if id == (golibvirt.UUID{}) {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}

// domainDesc is the subset of a domain description the backend reads back.
type domainDesc struct {
	XMLName xml.Name   `xml:"domain"`
	Name    string     `xml:"name"`
	UUID    string     `xml:"uuid"`
	Memory  memoryDesc `xml:"memory"`
	VCPU    vcpuDesc   `xml:"vcpu"`
	Serial  serialDesc `xml:"serial"`
	Devices struct {
		Interfaces []interfaceDesc `xml:"interface"`
		Disks      []diskDesc      `xml:"disk"`
	} `xml:"devices"`
}

// diskDesc is one <disk> of a domain. Only the source path matters here: it is
// how a volume the guest reads from is found in the storage pools, so Undefine
// can delete it with the definition (docs/design/04-libvirt-integration.md §3).
type diskDesc struct {
	Source struct {
		File string `xml:"file,attr"`
		Dev  string `xml:"dev,attr"`
	} `xml:"source"`
}

// StoragePaths returns the sources of the disks the domain declares, in the
// order the definition lists them. A network disk or a filesystem has no path
// here and is not a storage volume, so it is left out rather than looked up and
// missed.
func (d domainDesc) StoragePaths() []string {
	paths := make([]string, 0, len(d.Devices.Disks))
	for _, disk := range d.Devices.Disks {
		path := strings.TrimSpace(disk.Source.File)
		if path == "" {
			path = strings.TrimSpace(disk.Source.Dev)
		}
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

// serialDesc is the first serial port of a domain, which is the one Kumo uses for
// the guest console: <serial type='pty'><log file='…' append='on'/></serial>.
//
// The log element is what says where the bytes go; the pty has no path of its
// own, so a description without a log element has no console file to read.
type serialDesc struct {
	Type string `xml:"type,attr"`
	Log  struct {
		File   string `xml:"file,attr"`
		Append string `xml:"append,attr"`
	} `xml:"log"`
}

// ConsoleLog is the file libvirt writes the serial console of this domain to.
//
// Reading the path back from the definition is what lets the console-output
// endpoint serve the log without Kumo having to know where its own domain builder
// put it, and it keeps working for a definition an operator edited.
func (d domainDesc) ConsoleLog() string { return strings.TrimSpace(d.Serial.Log.File) }

// memoryDesc is a memory figure with the unit libvirt wrote it in.
type memoryDesc struct {
	Unit  string `xml:"unit,attr"`
	Value int    `xml:",chardata"`
}

// MiB converts the figure to MiB.
func (m memoryDesc) MiB() int {
	// libvirt reports memory in whatever unit the domain was defined with, and a
	// description that left the unit out means KiB, which is what the libvirt
	// schema defaults to. Kumo writes MiB, but an instance defined elsewhere, or
	// one an operator edited, can come back in any of these.
	switch strings.ToLower(strings.TrimSpace(m.Unit)) {
	case "b", "bytes":
		return m.Value / (1024 * 1024)
	case "mib", "m":
		return m.Value
	case "gib", "g":
		return m.Value * 1024
	case "", "kib", "k", "kb", "kilobytes":
		return m.Value / 1024
	default:
		return m.Value
	}
}

// vcpuDesc is the vCPU count: Value is the maximum, Current what the guest is
// using right now.
type vcpuDesc struct {
	Current string `xml:"current,attr"`
	Value   int    `xml:",chardata"`
}

// interfaceDesc is one interface of a domain. The MAC is a child element with an
// address attribute, not an attribute of the interface, which is the shape libvirt
// writes and the only shape worth reading back.
type interfaceDesc struct {
	Type   string  `xml:"type,attr"`
	MAC    macDesc `xml:"mac"`
	Source struct {
		Network string `xml:"network,attr"`
		Bridge  string `xml:"bridge,attr"`
	} `xml:"source"`
}

// macDesc is the address of one interface.
type macDesc struct {
	Address string `xml:"address,attr"`
}

// MAC returns the address of the first interface that has one.
func (d domainDesc) MAC() string {
	for _, iface := range d.Devices.Interfaces {
		if iface.MAC.Address != "" {
			return iface.MAC.Address
		}
	}
	return ""
}

// VCPUs returns the current vCPU count, falling back to the maximum.
func (d domainDesc) VCPUs() int {
	if current, err := strconv.Atoi(d.VCPU.Current); err == nil && current > 0 {
		return current
	}
	return d.VCPU.Value
}

// MemoryMiB returns the memory in MiB, whichever unit the description used.
func (d domainDesc) MemoryMiB() int { return d.Memory.MiB() }

// domainState maps a libvirt state onto the four Kumo reports.
func domainState(state int32) hypervisor.DomainState {
	switch golibvirt.DomainState(state) {
	case golibvirt.DomainRunning:
		return hypervisor.DomainStateRunning
	case golibvirt.DomainPaused:
		return hypervisor.DomainStatePaused
	case golibvirt.DomainCrashed:
		return hypervisor.DomainStateCrashed
	default:
		// Shutoff, blocked, pmsuspended and crashed-with-no-state all mean the
		// guest is not running, which is what an instance row can act on.
		return hypervisor.DomainStateShutoff
	}
}
