// Package imagefmt inspects a downloaded disk image without shelling out. The
// import workflow of docs/design/04-libvirt-integration.md §2 needs the format
// and the virtual size before the volume is registered with libvirt, and it
// needs them to be right: a wrong virtual size either truncates a guest's disk
// or lets one boot past the quota.
//
// Only the qcow2 header is parsed (spec: docs.qemu.org/docs/master/interop/qcow2.html),
// which is enough for Kumo's two supported formats. qemu-img is deliberately
// not used: it would add a runtime dependency for two integers.
package imagefmt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// Format is the on-disk format of an image.
type Format string

const (
	// FormatQCOW2 is a qcow2 copy-on-write image.
	FormatQCOW2 Format = "qcow2"
	// FormatRaw is a plain sector image.
	FormatRaw Format = "raw"
)

// qcow2 header layout, in bytes from the start of the file. Only the fields
// Kumo acts on are named.
const (
	qcow2HeaderLen = 104

	qcow2MagicOffset    = 0
	qcow2VersionOffset  = 4
	qcow2BackingOffset  = 8  // 8-byte big-endian offset of the backing file name
	qcow2SizeOffset     = 24 // 8-byte big-endian virtual size in bytes
	qcow2ClusterOffset  = 20 // 4-byte big-endian cluster bits
	qcow2Magic          = "QFI\xfb"
	qcow2Version2OrMore = 2
)

// rawAlignment is the sector size a raw image must be a multiple of. A file
// that is not is truncated mid-sector and cannot boot.
const rawAlignment = 512

// Sentinel errors of Inspect, so the import workflow can explain a refusal.
var (
	// ErrUnknownFormat means the file is neither qcow2 nor a plausible raw
	// image.
	ErrUnknownFormat = errors.New("imagefmt: unknown disk image format")
	// ErrBackingFile means the qcow2 image is a delta on top of another file.
	// Importing it would leave the guest reading a path that does not exist in
	// the images pool, so it is refused rather than silently flattened.
	ErrBackingFile = errors.New("imagefmt: the image is a qcow2 delta on top of a backing file")
	// ErrTruncated means the file is shorter than the format needs.
	ErrTruncated = errors.New("imagefmt: the file is too short to be a disk image")
	// ErrUnsupportedVersion means the qcow2 version is newer than this parser
	// knows, so its header cannot be trusted.
	ErrUnsupportedVersion = errors.New("imagefmt: unsupported qcow2 version")
	// ErrImplausibleSize means the virtual size is not a usable disk.
	ErrImplausibleSize = errors.New("imagefmt: implausible virtual size")
)

// Info is what the import workflow records on the image row.
type Info struct {
	Format           Format
	SizeBytes        int64 // bytes in the file
	VirtualSizeBytes int64 // bytes the guest sees
}

// Inspect reads the header of the image at path. Only the first few hundred
// bytes are read, so this is cheap even for a 20 GiB file.
func Inspect(path string) (Info, error) {
	file, err := os.Open(path) //nolint:gosec // the path comes from our own temp dir
	if err != nil {
		return Info{}, fmt.Errorf("imagefmt: open the image: %w", err)
	}
	defer file.Close() //nolint:errcheck // a read-only handle

	return InspectReader(file)
}

// InspectReader reads the header of an image stream. It rewinds first, so the
// caller may pass a stream it has already read from.
func InspectReader(r io.ReadSeeker) (Info, error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return Info{}, fmt.Errorf("imagefmt: measure the image: %w", err)
	}
	// The magic is only 4 bytes, so a short file is checked before the rest of
	// the header: a 104-byte file may still be a valid, tiny qcow2.
	if size < int64(qcow2MagicOffset+len(qcow2Magic)) {
		return Info{}, fmt.Errorf("%w: %d bytes", ErrTruncated, size)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return Info{}, fmt.Errorf("imagefmt: rewind the image: %w", err)
	}

	header := make([]byte, qcow2HeaderLen)
	if _, err := io.ReadFull(r, header); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return Info{}, fmt.Errorf("imagefmt: read the image header: %w", err)
	}

	if string(header[qcow2MagicOffset:qcow2MagicOffset+len(qcow2Magic)]) != qcow2Magic {
		return inspectRaw(size)
	}
	return inspectQCOW2(header, size)
}

// inspectQCOW2 parses a qcow2 header, see the package comment.
func inspectQCOW2(header []byte, size int64) (Info, error) {
	version := binary.BigEndian.Uint32(header[qcow2VersionOffset : qcow2VersionOffset+4])
	if version < qcow2Version2OrMore {
		// v1 never had a cluster size field this parser understands, and no
		// cloud image ships it any more.
		return Info{}, fmt.Errorf("%w: %d", ErrUnsupportedVersion, version)
	}

	clusterBits := binary.BigEndian.Uint32(header[qcow2ClusterOffset : qcow2ClusterOffset+4])
	if clusterBits < 9 || clusterBits > 21 {
		// qcow2 allows 2^9 (512 B) to 2^21 (2 MiB) clusters.
		return Info{}, fmt.Errorf("%w: cluster bits %d are out of range", ErrUnsupportedVersion, clusterBits)
	}

	if backingOffset := binary.BigEndian.Uint64(
		header[qcow2BackingOffset : qcow2BackingOffset+8]); backingOffset != 0 {
		return Info{}, fmt.Errorf("%w: the backing file starts at offset %d",
			ErrBackingFile, backingOffset)
	}

	virtualSize := binary.BigEndian.Uint64(header[qcow2SizeOffset : qcow2SizeOffset+8])
	if err := checkVirtualSize(virtualSize); err != nil {
		return Info{}, err
	}

	return Info{
		Format:           FormatQCOW2,
		SizeBytes:        size,
		VirtualSizeBytes: int64(virtualSize),
	}, nil
}

// inspectRaw accepts a plain image whose length is a whole number of sectors.
// Its virtual size is its file size, which is the only honest answer without a
// header.
func inspectRaw(size int64) (Info, error) {
	if size%rawAlignment != 0 {
		return Info{}, fmt.Errorf(
			"%w: neither qcow2 nor a raw image (a raw image must be a multiple of %d bytes, this one is %d)",
			ErrUnknownFormat, rawAlignment, size)
	}

	return Info{
		Format:           FormatRaw,
		SizeBytes:        size,
		VirtualSizeBytes: size,
	}, nil
}

// checkVirtualSize rejects sizes that cannot describe a disk, so a corrupt
// header is caught at import instead of at boot.
func checkVirtualSize(virtualSize uint64) error {
	const (
		minimum = 1 << 20 // 1 MiB
		maximum = 1 << 56 // 64 PiB
	)

	switch {
	case virtualSize < minimum:
		return fmt.Errorf("%w: %d bytes is smaller than 1 MiB", ErrImplausibleSize, virtualSize)
	case virtualSize > maximum:
		return fmt.Errorf("%w: %d bytes is larger than 64 PiB", ErrImplausibleSize, virtualSize)
	default:
		return nil
	}
}
