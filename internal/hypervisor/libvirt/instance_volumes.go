package libvirt

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/seediso"
)

// gib is a gibibyte, the unit the API and the pool XML both use.
const gib = 1 << 30

// The per-instance volumes of a launch, from docs/design/04-libvirt-integration.md
// §2 and §3:
//
//	i-<id>-root.qcow2   a copy-on-write overlay on the image volume, in the
//	                    instances pool
//	i-<id>-seed.iso     the NoCloud seed, in the same pool
//
// The names come from the hypervisor package, which the service also uses, so a
// rollback deletes exactly what the launch created.

// RootDiskName is the volume name of an instance's root disk.
func RootDiskName(instanceID string) string { return hypervisor.RootDiskName(instanceID) }

// SeedISOName is the volume name of an instance's NoCloud seed.
func SeedISOName(instanceID string) string { return hypervisor.SeedISOName(instanceID) }

// rootDiskXML is the StorageVolCreateXML document of a per-instance overlay.
//
// The backing store is what makes the launch cheap: a 10 GiB root disk is a few
// megabytes of delta over an image that is already in the pool, so ten instances
// of a 2 GiB image cost ten deltas rather than twenty gigabytes.
type rootDiskXML struct {
	XMLName    xml.Name         `xml:"volume"`
	Type       string           `xml:"type,attr"`
	Name       string           `xml:"name"`
	Capacity   capacityXML      `xml:"capacity"`
	Allocation allocationXML    `xml:"allocation"`
	Target     *volumeTarget    `xml:"target,omitempty"`
	Backing    *rootDiskBacking `xml:"backing,omitempty"`
}

// rootDiskBacking names the image volume the overlay reads from. Both a path and
// a volume name are written: libvirt resolves the name inside the pool, and the
// path keeps the delta correct if the pool is ever rebuilt.
type rootDiskBacking struct {
	Path        string            `xml:"path,omitempty"`
	VolumeName  string            `xml:"volume_name,omitempty"`
	Format      *volumeFormat     `xml:"format,omitempty"`
	Permissions *volumePermission `xml:"permissions,omitempty"`
}

// CreateRootDisk makes the copy-on-write overlay an instance boots from.
//
// The source is a volume in the images pool, not a path: that is what the image
// service published, and looking the volume up is what makes a renamed or moved
// image volume fail here rather than at boot.
func (c *Client) CreateRootDisk(ctx context.Context, spec hypervisor.RootDiskSpec) error {
	if spec.InstanceID == "" {
		return fmt.Errorf("%w: a root disk needs an instance id", hypervisor.ErrInvalidSpec)
	}
	if spec.SourceVolume == "" {
		return fmt.Errorf("%w: the root disk of %s needs a source volume",
			hypervisor.ErrInvalidSpec, spec.InstanceID)
	}
	if spec.Pool == "" {
		return fmt.Errorf("%w: the root disk of %s needs a pool", hypervisor.ErrInvalidSpec, spec.InstanceID)
	}
	if spec.SizeGiB <= 0 {
		return fmt.Errorf("%w: the root disk of %s is %d GiB",
			hypervisor.ErrInvalidSpec, spec.InstanceID, spec.SizeGiB)
	}

	name := RootDiskName(spec.InstanceID)
	size := uint64(spec.SizeGiB) * gib

	// The backing volume has to be looked up first so a missing image is a clear
	// error here, instead of a volume libvirt refuses with its own wording.
	var backingPath string
	if err := c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		imagesPool, _, ok := c.imagesPool()
		if !ok {
			return fmt.Errorf("%w: the images pool is unknown; call EnsureInfra first",
				hypervisor.ErrInvalidState)
		}
		pool, err := conn.StoragePoolLookupByName(imagesPool)
		if err != nil {
			return fmt.Errorf("look the images pool %q up: %w", imagesPool, err)
		}
		vol, err := conn.StorageVolLookupByName(pool, spec.SourceVolume)
		if err != nil {
			if isNotFound(err) {
				return fmt.Errorf("the image volume %q: %w", spec.SourceVolume, hypervisor.ErrNotFound)
			}
			return fmt.Errorf("look the image volume %q up: %w", spec.SourceVolume, err)
		}

		descriptor, err := conn.StorageVolGetXMLDesc(vol, 0)
		if err != nil {
			return fmt.Errorf("describe the image volume %q: %w", spec.SourceVolume, err)
		}
		backingPath = volumeSourcePath(descriptor)

		// A root disk cannot be smaller than the image it is a delta of: the
		// guest would see a filesystem that ends before its own partition table.
		_, capacity, _, err := conn.StorageVolGetInfo(vol)
		if err != nil {
			return fmt.Errorf("read the info of the image volume %q: %w", spec.SourceVolume, err)
		}
		if capacity > 0 && capacity < size {
			return fmt.Errorf("%w: a %d GiB root disk cannot hold the %d GiB image %q",
				hypervisor.ErrInvalidSpec, spec.SizeGiB, capacity/gib, spec.SourceVolume)
		}
		return nil
	}); err != nil {
		return err
	}

	document, err := xml.Marshal(rootDiskXML{
		Type:     "file",
		Name:     name,
		Capacity: capacityXML{Unit: "bytes", Bytes: size},
		// Zero keeps the file sparse, which is the point of an overlay.
		Allocation: allocationXML{Unit: "bytes", Bytes: 0},
		Target: &volumeTarget{
			Format:      volumeFormat{Type: string(hypervisor.FormatQCOW2)},
			Permissions: &volumePermission{Mode: fmt.Sprintf("%03o", dirPoolVolumePerms)},
		},
		Backing: &rootDiskBacking{
			Path:       backingPath,
			VolumeName: spec.SourceVolume,
			Format:     &volumeFormat{Type: string(hypervisor.FormatQCOW2)},
			Permissions: &volumePermission{
				Mode: fmt.Sprintf("%03o", dirPoolVolumePerms),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("build the root disk XML for %q: %w", name, err)
	}

	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		pool, err := conn.StoragePoolLookupByName(spec.Pool)
		if err != nil {
			return fmt.Errorf("look the pool %q up: %w", spec.Pool, err)
		}

		if _, err := conn.StorageVolCreateXML(pool, string(document), 0); err != nil {
			if isAlreadyExists(err) {
				return fmt.Errorf("the root disk %q: %w", name, hypervisor.ErrAlreadyExists)
			}
			return fmt.Errorf("create the root disk %q in %q: %w", name, spec.Pool, err)
		}
		return nil
	})
}

// CreateSeedISO builds the NoCloud seed and puts it in the pool as a volume.
//
// The ISO is written straight into the pool directory and the pool is refreshed,
// which is the same path an imported image takes (docs/design/04-libvirt-integration.md
// §2): a 16 KiB seed is not worth an upload over the socket, and a rename is
// atomic so libvirt never sees a half-written file.
func (c *Client) CreateSeedISO(ctx context.Context, pool, name string, seed hypervisor.CloudInit) error {
	if name == "" {
		return fmt.Errorf("%w: a seed ISO needs a name", hypervisor.ErrInvalidSpec)
	}
	if pool == "" {
		return fmt.Errorf("%w: the seed ISO %q needs a pool", hypervisor.ErrInvalidSpec, name)
	}
	_, dir, ok := c.poolDir(pool)
	if !ok {
		return fmt.Errorf("%w: the local directory of the pool %q is unknown; call EnsureInfra first",
			hypervisor.ErrInvalidState, pool)
	}

	documents := seediso.Documents{
		MetaData:      seed.MetaData,
		UserData:      seed.UserData,
		NetworkConfig: seed.NetworkConfig,
	}
	if documents.Empty() {
		return fmt.Errorf("%w: the seed ISO %q would have no documents", hypervisor.ErrInvalidSpec, name)
	}

	target := filepath.Join(dir, name)
	if err := seediso.Build(target, documents); err != nil {
		return fmt.Errorf("build the seed ISO %q: %w", name, err)
	}
	// seediso.Build creates the file 0644; the pool's QEMU user only needs to
	// read it, and this makes that explicit where the pool is a directory.
	if err := os.Chmod(target, dirPoolVolumePerms); err != nil {
		return fmt.Errorf("chmod %s: %w", target, err)
	}

	if err := c.refreshPool(ctx, pool); err != nil {
		return err
	}

	// A seed ISO that libvirt cannot see is worse than none: the domain would
	// boot without the key and nobody would know why. So its absence is an error.
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		handle, err := conn.StoragePoolLookupByName(pool)
		if err != nil {
			return fmt.Errorf("look the pool %q up: %w", pool, err)
		}
		if _, err := conn.StorageVolLookupByName(handle, name); err != nil {
			if isNotFound(err) {
				return fmt.Errorf("the seed ISO %q is not visible in %q: %w",
					name, pool, hypervisor.ErrNotFound)
			}
			return fmt.Errorf("look the seed ISO %q up: %w", name, err)
		}
		return nil
	})
}

// DeleteRootDisk and DeleteSeedISO are the rollback of a launch. They go through
// DeleteVolume, which reports hypervisor.ErrNotFound for a volume that is already
// gone, so a caller can treat a missing volume as success.

// poolDir returns the local directory of a pool Kumo created, if it is one of the
// two the client knows about. Only those two have a path: a pool defined by
// somebody else has to be reached through the API instead.
func (c *Client) poolDir(pool string) (name, dir string, ok bool) {
	cfg := c.recordedInfra()
	if cfg.DataDir == "" {
		return "", "", false
	}

	switch pool {
	case cfg.ImagesPool:
		return pool, filepath.Join(cfg.DataDir, "pools", "images"), cfg.ImagesPool != ""
	case cfg.InstancesPool:
		return pool, filepath.Join(cfg.DataDir, "pools", "instances"), cfg.InstancesPool != ""
	default:
		return "", "", false
	}
}

// refreshPool asks libvirt to rescan a directory pool.
func (c *Client) refreshPool(ctx context.Context, pool string) error {
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		handle, err := conn.StoragePoolLookupByName(pool)
		if err != nil {
			return fmt.Errorf("look the pool %q up: %w", pool, err)
		}
		if err := conn.StoragePoolRefresh(handle, 0); err != nil {
			return fmt.Errorf("refresh the pool %q: %w", pool, err)
		}
		return nil
	})
}

// volumeSourcePath pulls the <source file=…> path out of a volume description.
func volumeSourcePath(descriptor string) string {
	var desc struct {
		Source *struct {
			File   string `xml:"file,attr"`
			Device string `xml:"dev,attr"`
		} `xml:"source"`
	}
	if err := xml.Unmarshal([]byte(descriptor), &desc); err != nil {
		return ""
	}
	if desc.Source == nil {
		return ""
	}
	return desc.Source.File
}

// VolumePath reports where a volume lives on the host.
//
// The domain XML attaches disks by path rather than by volume name
// (docs/design/04-libvirt-integration.md §5), so whoever writes that XML needs a
// path. Asking libvirt for it keeps the pool layout in one place: a service that
// rebuilt the path from KUMO_DATA_DIR would be a second copy of the rule
// filepath.Join(DataDir, "pools", …) has to agree with.
func (c *Client) VolumePath(ctx context.Context, pool, name string) (string, error) {
	if pool == "" || name == "" {
		return "", fmt.Errorf("%w: a volume path needs a pool and a name", hypervisor.ErrInvalidSpec)
	}

	var path string
	err := c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		handle, err := conn.StoragePoolLookupByName(pool)
		if err != nil {
			if isNotFound(err) {
				return fmt.Errorf("the pool %q: %w", pool, hypervisor.ErrNotFound)
			}
			return fmt.Errorf("look the pool %q up: %w", pool, err)
		}

		vol, err := conn.StorageVolLookupByName(handle, name)
		if err != nil {
			if isNotFound(err) {
				return fmt.Errorf("the volume %q in %q: %w", name, pool, hypervisor.ErrNotFound)
			}
			return fmt.Errorf("look the volume %q up in %q: %w", name, pool, err)
		}

		descriptor, err := conn.StorageVolGetXMLDesc(vol, 0)
		if err != nil {
			return fmt.Errorf("describe the volume %q: %w", name, err)
		}

		path = volumeSourcePath(descriptor)
		if path == "" {
			return fmt.Errorf("%w: the volume %q in %q has no local path; a pool that is "+
				"not on this filesystem has to be reached through the API", hypervisor.ErrNotFound, name, pool)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	return path, nil
}
