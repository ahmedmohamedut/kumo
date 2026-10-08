package libvirt

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
)

var _ hypervisor.Hypervisor = (*Client)(nil)

// defaultConsoleBytes is the tail of a console log a caller gets when it asks for
// none. The endpoint always asks for a size; the default is what a direct caller
// of the interface gets.
const defaultConsoleBytes = 64 << 10

// Start boots a domain from the definition Kumo wrote at launch.
//
// The definition is found by name, because the domain name is the instance id
// (docs/design/04-libvirt-integration.md §2): a start needs nothing but the id
// from the row, so there is no state to keep in step with the domain.
func (c *Client) Start(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: a domain id must not be empty", hypervisor.ErrInvalidSpec)
	}
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		dom, err := lookupDomain(conn, id)
		if err != nil {
			return err
		}
		if err := conn.DomainCreate(dom); err != nil {
			if isOperationInvalid(err) {
				// libvirt refuses to boot a domain that is already up or paused.
				// That is the one transition the state machine already rules out,
				// so it is a conflict rather than a failure of the host.
				return fmt.Errorf("the domain %q cannot be started: %w",
					id, hypervisor.ErrInvalidState)
			}
			return fmt.Errorf("start the domain %q: %w", id, err)
		}
		// DomainCreate answers as soon as the guest is launched, not when it is
		// running, and a guest that exits immediately leaves a handle to nothing.
		return checkDomainExists(conn, id)
	})
}

// Shutdown asks the guest to power off through ACPI.
//
// The request is a request: libvirt returns as soon as QEMU has been told, and the
// guest decides when to act on it. Waiting for the domain to go off is the
// caller's job, because only the caller knows how long to wait
// (docs/design/04-libvirt-integration.md §3).
func (c *Client) Shutdown(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: a domain id must not be empty", hypervisor.ErrInvalidSpec)
	}
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		dom, err := lookupDomain(conn, id)
		if err != nil {
			return err
		}
		if err := conn.DomainShutdown(dom); err != nil {
			if isOperationInvalid(err) {
				// A domain that is off, or one QEMU cannot signal (an unpausable
				// guest), cannot be asked to shut down.
				return fmt.Errorf("the domain %q cannot be shut down: %w",
					id, hypervisor.ErrInvalidState)
			}
			return fmt.Errorf("shut the domain %q down: %w", id, err)
		}
		return nil
	})
}

// Reboot asks the guest to restart through ACPI, leaving the definition alone.
func (c *Client) Reboot(ctx context.Context, id string) error {
	return c.power(ctx, id, "reboot", func(conn *golibvirt.Libvirt, dom golibvirt.Domain) error {
		// DomainRebootDefault is an ACPI request through the power button: the
		// guest shuts down and boots again, and the domain keeps its definition.
		return conn.DomainReboot(dom, golibvirt.DomainRebootDefault)
	})
}

// Reset reboots the domain without asking the guest, the equivalent of pressing
// the reset switch. The guest is not told and may lose whatever it had not
// written, which is why it is what ?force=true buys.
func (c *Client) Reset(ctx context.Context, id string) error {
	return c.power(ctx, id, "reset", func(conn *golibvirt.Libvirt, dom golibvirt.Domain) error {
		return conn.DomainReset(dom, 0)
	})
}

// power is the shared body of Reboot and Reset: both act on a running domain and
// both fail the same way when the domain is off.
func (c *Client) power(
	ctx context.Context, id, what string,
	call func(*golibvirt.Libvirt, golibvirt.Domain) error,
) error {
	if id == "" {
		return fmt.Errorf("%w: a domain id must not be empty", hypervisor.ErrInvalidSpec)
	}
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		dom, err := lookupDomain(conn, id)
		if err != nil {
			return err
		}
		if err := call(conn, dom); err != nil {
			if isOperationInvalid(err) {
				// Neither a reboot nor a reset does anything to a domain that is
				// not running, and libvirt says so with an operation error.
				return fmt.Errorf("the domain %q is not running: %w",
					id, hypervisor.ErrInvalidState)
			}
			return fmt.Errorf("%s the domain %q: %w", what, id, err)
		}
		return nil
	})
}

// ConsoleOutput returns the tail of the serial console libvirt writes for a
// domain.
//
// The path comes from the domain's own description rather than from a directory
// Kumo remembers, because that is where the path in use came from: a definition
// an operator edited still reports the log it writes, and nothing here has to
// agree with the domain builder about a location.
func (c *Client) ConsoleOutput(ctx context.Context, id string, maxBytes int) ([]byte, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: a domain id must not be empty", hypervisor.ErrInvalidSpec)
	}
	if maxBytes <= 0 {
		maxBytes = defaultConsoleBytes
	}

	var path string
	err := c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		dom, err := lookupDomain(conn, id)
		if err != nil {
			return err
		}
		descriptor, err := conn.DomainGetXMLDesc(dom, 0)
		if err != nil {
			return fmt.Errorf("describe the domain %q: %w", id, err)
		}
		var doc domainDesc
		if err := xml.Unmarshal([]byte(descriptor), &doc); err != nil {
			return fmt.Errorf("read the description of the domain %q: %w", id, err)
		}
		path = doc.ConsoleLog()
		if path == "" {
			return fmt.Errorf("the domain %q writes no console log: %w",
				id, hypervisor.ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	tail, err := tailFile(path, maxBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// libvirt has not created the file yet. A guest that has not reached
			// the kernel has nothing to say, which is a 404 to the caller rather
			// than an error of the host.
			return nil, fmt.Errorf("the console log of the domain %q: %w",
				id, hypervisor.ErrNotFound)
		}
		return nil, fmt.Errorf("read the console log of the domain %q: %w", id, err)
	}
	return tail, nil
}

// tailFile returns at most the last maxBytes of a file.
//
// The end of the file is reached with a seek rather than by reading the whole
// thing: a serial console of a guest that has been up for a month is tens of
// megabytes, and the endpoint only ever shows the end of it. A file smaller than
// the request is read whole, which is what a fresh console log always is.
func tailFile(path string, maxBytes int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // a read-only handle has nothing to flush

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	size := info.Size()
	if maxBytes > 0 && size > int64(maxBytes) {
		if _, err := file.Seek(-int64(maxBytes), io.SeekEnd); err != nil {
			return nil, err
		}
		size = int64(maxBytes)
	}

	out := make([]byte, size)
	read, err := file.Read(out)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return out[:read], nil
}

// isNoDomain reports a libvirt "no such domain" error.
//
// The error code is the reliable part. The text is checked as well because the
// domain lookups answer with a message that does not always carry the code, and
// the cleanup paths have to treat a vanished domain as success
// (docs/design/04-libvirt-integration.md §3).
func isNoDomain(err error) bool {
	if err == nil {
		return false
	}
	if golibvirt.IsNotFound(err) {
		return true
	}
	message := []string{
		"no domain with name",
		"no such domain",
		"domain not found",
	}
	return slices.ContainsFunc(message, func(text string) bool {
		return contains(err.Error(), text)
	})
}

// isOperationInvalid reports a libvirt error the operation does not allow in the
// current state: booting a running domain, shutting down one that is off, and so
// on. libvirt answers with the same code for all of them, so the caller decides
// which transition this was.
func isOperationInvalid(err error) bool {
	for err != nil {
		var libErr golibvirt.Error
		if errors.As(err, &libErr) && libErr.Code == uint32(golibvirt.ErrOperationInvalid) {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

// Capacity reports what the host has: the vCPUs and memory of the node, and
// the size of the instances pool.
//
// It is deliberately the totals and nothing else. What is already taken is
// summed from the instances table by the service layer, because libvirt cannot
// tell the difference between an overlay on top of an image and a stale one, and
// a terminate removes the row that said which overlays were live.
func (c *Client) Capacity(ctx context.Context) (hypervisor.Capacity, error) {
	var out hypervisor.Capacity

	if err := c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		_, memory, cpus, _, _, _, _, _, err := conn.NodeGetInfo()
		if err != nil {
			return fmt.Errorf("read the node info: %w", err)
		}

		out.VCPUs = int(cpus)
		// NodeGetInfo reports memory in KiB.
		out.MemoryBytes = int64(memory) * 1024
		return nil
	}); err != nil {
		return hypervisor.Capacity{}, err
	}

	pool := c.recordedInfra().InstancesPool
	if pool == "" {
		// EnsureInfra has not run, so there is no pool to measure yet. Zero disk
		// reads as a node that cannot take a disk, which is the safe direction.
		return out, nil
	}

	if err := c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		handle, err := conn.StoragePoolLookupByName(pool)
		if err != nil {
			return fmt.Errorf("look the instances pool %q up: %w", pool, err)
		}
		_, capacity, _, _, err := conn.StoragePoolGetInfo(handle)
		if err != nil {
			return fmt.Errorf("read the instances pool %q: %w", pool, err)
		}
		out.DiskBytes = int64(capacity)
		return nil
	}); err != nil {
		return hypervisor.Capacity{}, err
	}

	return out, nil
}

// Events is the libvirt event stream the reconciler of M4.5 consumes. Until then
// it refuses rather than pretend: a caller that reaches it gets
// hypervisor.ErrNotImplemented and degrades instead of corrupting the host.
func (c *Client) Events(context.Context) (<-chan hypervisor.DomainEvent, error) {
	return nil, hypervisor.ErrNotImplemented
}
