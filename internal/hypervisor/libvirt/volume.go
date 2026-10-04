package libvirt

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
)

// dirPoolVolumePerms is the mode of an image volume in the pool directory.
// 04-libvirt-integration.md §2 says the file belongs to the libvirt QEMU user;
// the pool's own <permissions> does that, so the volume only has to be
// readable and not writable by the world.
const dirPoolVolumePerms os.FileMode = 0o644

// volumeXML is the StorageVolCreateXML document. The values are escaped by
// encoding/xml, never interpolated (AGENTS.md, security rules).
type volumeXML struct {
	XMLName    xml.Name       `xml:"volume"`
	Type       string         `xml:"type,attr"`
	Name       string         `xml:"name"`
	Capacity   capacityXML    `xml:"capacity"`
	Allocation allocationXML  `xml:"allocation"`
	Target     *volumeTarget  `xml:"target,omitempty"`
	Backing    *volumeBacking `xml:"backing,omitempty"`
}

// capacityXML is the virtual size of the volume.
type capacityXML struct {
	Unit  string `xml:"unit,attr"`
	Bytes uint64 `xml:",chardata"`
}

// allocationXML is the space reserved up front. Zero keeps the file sparse,
// which matters when the virtual size is 10 GiB and the file is 500 MiB.
type allocationXML struct {
	Unit  string `xml:"unit,attr"`
	Bytes uint64 `xml:",chardata"`
}

// volumeTarget carries the on-disk format and the directory permissions.
type volumeTarget struct {
	Format      volumeFormat      `xml:"format"`
	Permissions *volumePermission `xml:"permissions,omitempty"`
}

// volumeFormat is the on-disk format. Only qcow2 and raw are supported
// (docs/design/02-data-model.md).
type volumeFormat struct {
	Type string `xml:"type,attr"`
}

// volumePermission mirrors the pool's ownership so QEMU can read the volume.
type volumePermission struct {
	Mode string `xml:"mode"`
}

// volumeBacking is refused on import: a delta on top of another file would not
// survive a reboot of the pool. It exists so the parser can read one back and
// say why it is unsupported.
type volumeBacking struct {
	Path string `xml:"path"`
}

// volumeDesc is the subset of StorageVolGetXMLDesc that Kumo reads back.
type volumeDesc struct {
	XMLName xml.Name       `xml:"volume"`
	Name    string         `xml:"name"`
	Format  volumeFormat   `xml:"target>format"`
	Backing *volumeBacking `xml:"backing,omitempty"`
}

// ImportImage registers a downloaded image as a volume in the images pool.
//
// Both paths of 04-libvirt-integration.md §2 are implemented. A local directory
// pool takes the file as it is, which is a rename instead of a copy of a
// multi-gigabyte image; anything else falls back to creating the volume and
// streaming the bytes over the socket.
func (c *Client) ImportImage(ctx context.Context, name string, r io.Reader, spec hypervisor.ImportSpec) error {
	if name == "" {
		return fmt.Errorf("%w: the volume name must not be empty", hypervisor.ErrInvalidState)
	}
	if spec.SizeBytes <= 0 {
		return fmt.Errorf("%w: the image is empty", hypervisor.ErrInvalidState)
	}
	if err := validateFormat(spec.Format); err != nil {
		return err
	}

	capacity := spec.VirtualSizeBytes
	if capacity <= 0 {
		// A raw image without a declared virtual size is its own capacity; a
		// qcow2 without one would boot at a few megabytes, so refuse it.
		if spec.Format != hypervisor.FormatRaw {
			return fmt.Errorf("%w: a %s image must declare its virtual size",
				hypervisor.ErrInvalidState, spec.Format)
		}
		capacity = spec.SizeBytes
	}

	// Fast path: a local directory pool and a real file on disk.
	if file, ok := r.(*os.File); ok {
		if done, err := c.placeFile(ctx, name, file, spec.SizeBytes); err != nil {
			return err
		} else if done {
			return nil
		}
	}

	return c.uploadVolume(ctx, name, r, spec, capacity)
}

// placeFile moves an already downloaded file into the images pool directory and
// asks libvirt to rescan the pool. It reports false when the pool is not a local
// directory we know about, so the caller can fall back to an upload.
func (c *Client) placeFile(ctx context.Context, name string, file *os.File, size int64) (bool, error) {
	pool, dir, ok := c.imagesPool()
	if !ok {
		return false, nil
	}

	target := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return true, fmt.Errorf("create the images pool directory %s: %w", dir, err)
	}

	// Rename is atomic and does not copy, but it fails across filesystems
	// (a staging dir on another mount), so fall back to a copy.
	if err := os.Rename(file.Name(), target); err != nil {
		if copyErr := copyFile(file, target, size); copyErr != nil {
			// Both failures matter: the rename says why the copy was needed, the
			// copy says why the volume is not there.
			return true, fmt.Errorf("copy the image into %s after the rename failed: %w",
				dir, errors.Join(err, copyErr))
		}
		_ = file.Close()
		_ = os.Remove(file.Name())
	}

	// The mode has to be set here, after the file exists: a staged download is
	// created 0600, and libvirt runs the volume as its own QEMU user, which
	// would then fail to open it.
	if err := os.Chmod(target, dirPoolVolumePerms); err != nil {
		return true, fmt.Errorf("chmod %s: %w", target, err)
	}

	// A refresh is what makes libvirt notice a file that was not created
	// through the API.
	return true, c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		handle, err := conn.StoragePoolLookupByName(pool)
		if err != nil {
			return fmt.Errorf("look up the pool %q: %w", pool, err)
		}
		if err := conn.StoragePoolRefresh(handle, 0); err != nil {
			return fmt.Errorf("refresh the pool %q: %w", pool, err)
		}
		return nil
	})
}

// uploadVolume creates the volume in the pool and streams the bytes into it.
func (c *Client) uploadVolume(
	ctx context.Context, name string, r io.Reader, spec hypervisor.ImportSpec, capacity int64,
) error {
	pool, _, ok := c.imagesPool()
	if !ok {
		return fmt.Errorf("%w: the images pool is unknown; call EnsureInfra first",
			hypervisor.ErrInvalidState)
	}

	document, err := xml.Marshal(volumeXML{
		Type:       "file",
		Name:       name,
		Capacity:   capacityXML{Unit: "bytes", Bytes: uint64(capacity)},
		Allocation: allocationXML{Unit: "bytes", Bytes: 0},
		Target: &volumeTarget{
			Format: volumeFormat{Type: string(spec.Format)},
			Permissions: &volumePermission{
				Mode: fmt.Sprintf("%03o", dirPoolVolumePerms),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("build the volume XML for %q: %w", name, err)
	}

	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		handle, err := conn.StoragePoolLookupByName(pool)
		if err != nil {
			return fmt.Errorf("look up the pool %q: %w", pool, err)
		}

		vol, err := conn.StorageVolCreateXML(handle, string(document), 0)
		if err != nil {
			if isAlreadyExists(err) {
				return fmt.Errorf("volume %q: %w", name, hypervisor.ErrAlreadyExists)
			}
			return fmt.Errorf("create the volume %q in %q: %w", name, pool, err)
		}

		if err := conn.StorageVolUpload(vol, r, 0, uint64(spec.SizeBytes), 0); err != nil {
			// A half written volume is worse than none: remove it so a retry
			// starts from a clean pool.
			if delErr := conn.StorageVolDelete(vol, 0); delErr != nil {
				c.logger.ErrorContext(ctx, "libvirt: could not remove the partial volume",
					"volume", name, "pool", pool, "error", delErr)
			}
			return fmt.Errorf("upload %d bytes to the volume %q: %w", spec.SizeBytes, name, err)
		}

		return nil
	})
}

// ImageInfo returns the format and sizes of a volume in the images pool.
func (c *Client) ImageInfo(ctx context.Context, name string) (hypervisor.ImageInfo, error) {
	pool, _, ok := c.imagesPool()
	if !ok {
		return hypervisor.ImageInfo{}, fmt.Errorf("%w: the images pool is unknown; call EnsureInfra first",
			hypervisor.ErrInvalidState)
	}

	info := hypervisor.ImageInfo{Name: name, Pool: pool}

	err := c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		handle, err := conn.StoragePoolLookupByName(pool)
		if err != nil {
			return fmt.Errorf("look up the pool %q: %w", pool, err)
		}

		vol, err := conn.StorageVolLookupByName(handle, name)
		if err != nil {
			if isNotFound(err) {
				return fmt.Errorf("volume %q in %q: %w", name, pool, hypervisor.ErrNotFound)
			}
			return fmt.Errorf("look up the volume %q: %w", name, err)
		}

		_, capacity, allocation, err := conn.StorageVolGetInfo(vol)
		if err != nil {
			return fmt.Errorf("get the info of the volume %q: %w", name, err)
		}
		info.VirtualSizeBytes = int64(capacity)
		info.SizeBytes = int64(allocation)

		descriptor, err := conn.StorageVolGetXMLDesc(vol, 0)
		if err != nil {
			return fmt.Errorf("describe the volume %q: %w", name, err)
		}

		var desc volumeDesc
		if err := xml.Unmarshal([]byte(descriptor), &desc); err != nil {
			return fmt.Errorf("parse the description of the volume %q: %w", name, err)
		}
		if desc.Backing != nil && desc.Backing.Path != "" {
			return fmt.Errorf("%w: the volume %q reads from %q", hypervisor.ErrInvalidState,
				name, desc.Backing.Path)
		}

		info.Format = hypervisor.ImageFormat(desc.Format.Type)
		return validateFormat(info.Format)
	})
	if err != nil {
		return hypervisor.ImageInfo{}, err
	}

	return info, nil
}

// DeleteVolume removes a volume. A volume that is already gone reports
// hypervisor.ErrNotFound, so the caller decides whether to tolerate it.
func (c *Client) DeleteVolume(ctx context.Context, poolName, name string) error {
	if poolName == "" {
		if pool, _, ok := c.imagesPool(); ok {
			poolName = pool
		}
	}

	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		handle, err := conn.StoragePoolLookupByName(poolName)
		if err != nil {
			return fmt.Errorf("look up the pool %q: %w", poolName, err)
		}

		vol, err := conn.StorageVolLookupByName(handle, name)
		if err != nil {
			if isNotFound(err) {
				return fmt.Errorf("volume %q in %q: %w", name, poolName, hypervisor.ErrNotFound)
			}
			return fmt.Errorf("look up the volume %q: %w", name, err)
		}

		if err := conn.StorageVolDelete(vol, 0); err != nil {
			if isNotFound(err) {
				return fmt.Errorf("volume %q in %q: %w", name, poolName, hypervisor.ErrNotFound)
			}
			return fmt.Errorf("delete the volume %q from %q: %w", name, poolName, err)
		}
		return nil
	})
}

// imagesPool returns the images pool name and its local directory, as recorded
// by EnsureInfra.
func (c *Client) imagesPool() (pool, dir string, ok bool) {
	cfg := c.recordedInfra()
	if cfg.ImagesPool == "" || cfg.DataDir == "" {
		return "", "", false
	}
	return cfg.ImagesPool, filepath.Join(cfg.DataDir, "pools", "images"), true
}

// validateFormat keeps an unsupported format from reaching libvirt, which would
// accept it and fail later at boot.
func validateFormat(format hypervisor.ImageFormat) error {
	switch format {
	case hypervisor.FormatQCOW2, hypervisor.FormatRaw:
		return nil
	default:
		return fmt.Errorf("%w: unsupported image format %q", hypervisor.ErrInvalidState, format)
	}
}

// copyFile copies exactly size bytes from src to dst, which is the cross-device
// fallback for placing an image in a pool.
func copyFile(src *os.File, dst string, size int64) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind %s: %w", src.Name(), err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, dirPoolVolumePerms) //nolint:gosec // a pool volume
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer out.Close() //nolint:errcheck // the write result is checked below

	if _, err := io.CopyN(out, src, size); err != nil {
		return fmt.Errorf("copy the image to %s: %w", dst, err)
	}
	return out.Sync()
}

// notFoundTexts are the ways libvirt words "it is not there". go-libvirt returns
// them as text, so the message is the only handle there is: the daemon says
// "Storage vol not found" for a missing volume and "no such network" for a
// missing network, and both have to map onto ErrNotFound for the cleanup paths
// to treat a vanished resource as success.
var notFoundTexts = []string{
	"no such storage pool",
	"no such storage vol",
	"no such volume",
	"no such network",
	"storage pool not found",
	"storage vol not found",
	"storage volume not found",
}

// isNotFound reports a libvirt "no such volume/pool/network" error.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return slices.ContainsFunc(notFoundTexts, func(text string) bool {
		return strings.Contains(message, text)
	})
}

// isAlreadyExists reports a libvirt "volume already exists" error.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "already exists")
}
