package image

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// ErrBadCompression means a download announced itself as gzip and is not a
// valid gzip stream.
var ErrBadCompression = errors.New("image: the download is not a valid gzip stream")

// gzipMagic is the two bytes every gzip member starts with (RFC 1952).
var gzipMagic = []byte{0x1f, 0x8b}

// gunzipIfCompressed is the compressed transport of docs/design/07-databases.md
// section 7. An image published gzipped -- the engine images are, because the
// Kubernetes one would not fit GitHub's 2 GiB release-asset limit otherwise --
// is decompressed here, after its checksum has been checked against the bytes
// that were downloaded and before anything inspects it as an image. What the
// pool stores stays uncompressed, which is the property section 7 cares about:
// the volume is the backing file of every guest's root overlay, and a
// compressed one would decompress on every cold read for the guest's life.
//
// Detection is by content, not by the URL's extension: a URL need not end in
// .gz, and an uncompressed qcow2 or raw image never starts with these bytes
// (qcow2 starts with "QFI\xfb"; a raw disk with a boot sector or zeros).
//
// The decompressed size is bounded by the same limit as a download, so a small
// file cannot expand without end into the staging directory. It returns the
// path of the file to import, which is path itself when nothing was compressed;
// otherwise path is removed and the new file replaces it.
func (s *Service) gunzipIfCompressed(row sqlc.Image, path string) (string, error) {
	in, err := os.Open(path) //nolint:gosec // the path is inside our own temp dir
	if err != nil {
		return "", fmt.Errorf("open the staged image %q: %w", row.ID, err)
	}
	defer in.Close() //nolint:errcheck // read-only handle

	head := make([]byte, len(gzipMagic))
	if n, _ := io.ReadFull(in, head); n < len(gzipMagic) || !bytes.Equal(head, gzipMagic) {
		return path, nil
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind the staged image %q: %w", row.ID, err)
	}

	zr, err := gzip.NewReader(in)
	if err != nil {
		return "", fmt.Errorf("%w: image %q: %w", ErrBadCompression, row.ID, err)
	}
	defer zr.Close() //nolint:errcheck // the copy's result is what matters

	out, err := os.CreateTemp(s.tempDir, "kumo-image-*.img")
	if err != nil {
		return "", fmt.Errorf("create a staging file in %s: %w", s.tempDir, err)
	}
	outPath := out.Name()
	keep := false
	defer func() {
		_ = out.Close()
		if !keep {
			_ = os.Remove(outPath)
		}
	}()

	// One byte past the limit is read so that "exactly the limit" and "more"
	// can be told apart.
	written, err := io.Copy(out, io.LimitReader(zr, s.maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: image %q: %w", ErrBadCompression, row.ID, err)
	}
	if written > s.maxBytes {
		return "", fmt.Errorf("%w: image %q decompresses to more than %d bytes",
			imagefetch.ErrTooLarge, row.ID, s.maxBytes)
	}
	if written == 0 {
		return "", fmt.Errorf("%w: image %q decompresses to nothing", ErrBadCompression, row.ID)
	}
	if err := out.Sync(); err != nil {
		return "", fmt.Errorf("flush the decompressed image %q: %w", row.ID, err)
	}

	keep = true
	_ = os.Remove(path)
	return outPath, nil
}
