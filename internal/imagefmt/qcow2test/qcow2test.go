// Package qcow2test builds small, structurally valid qcow2 headers for tests.
// It exists so the imagefmt tests and the image service tests agree on what a
// "tiny qcow2" looks like, without either of them shelling out to qemu-img.
package qcow2test

import "encoding/binary"

// HeaderLen is the size of the header the builder writes, matching the
// imagefmt parser.
const HeaderLen = 104

// Header returns a valid qcow2 header declaring the given virtual size.
func Header(virtualSize uint64) []byte {
	header := make([]byte, HeaderLen)

	copy(header[0:4], "QFI\xfb")                  // magic
	binary.BigEndian.PutUint32(header[4:8], 3)    // version 3
	binary.BigEndian.PutUint64(header[8:16], 0)   // no backing file
	binary.BigEndian.PutUint32(header[16:20], 0)  // mtime
	binary.BigEndian.PutUint32(header[20:24], 16) // 2^16 = 64 KiB clusters
	binary.BigEndian.PutUint64(header[24:32], virtualSize)
	binary.BigEndian.PutUint32(header[32:36], 2) // l1 size
	binary.BigEndian.PutUint32(header[36:40], 1) // l1 table offset
	binary.BigEndian.PutUint64(header[40:48], 0) // refcount table
	binary.BigEndian.PutUint32(header[48:52], 1) // refcount table clusters
	binary.BigEndian.PutUint32(header[52:56], 0) // nb snapshots
	binary.BigEndian.PutUint64(header[56:64], 0) // snapshots offset

	return header
}

// Tiny returns a small qcow2 file: a valid header plus body, long enough to be
// a plausible image and small enough to keep in memory.
func Tiny(virtualSize uint64) []byte {
	body := make([]byte, 1024)
	for i := range body {
		body[i] = byte(i % 251)
	}

	image := Header(virtualSize)
	return append(image, body...)
}

// Raw returns a raw image of the given size, which must be a multiple of 512.
func Raw(size int64) []byte {
	image := make([]byte, size)
	for i := range image {
		image[i] = byte(i % 251)
	}
	return image
}
