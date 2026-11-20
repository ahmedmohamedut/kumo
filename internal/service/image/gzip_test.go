package image_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/imagefmt/qcow2test"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
)

// gzipped compresses body the way the engine-images workflow publishes an
// image.
func gzipped(t *testing.T, body []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A gzipped qcow2, checked against the checksum of what was published (the
// compressed file), imports as an ordinary qcow2 volume: the pool holds it
// uncompressed, and no staging file is left behind -- neither the download nor
// the decompressed copy.
func TestImportHandlerDecompressesAGzippedImage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	raw := qcow2test.Tiny(virtualDiskGB << 30)
	url, sum := h.serveImage(t, gzipped(t, raw))

	imported, err := h.svc.Import(t.Context(), image.ImportInput{
		Name:      "engine-gz",
		SourceURL: url,
		Checksum:  image.Checksum{Algorithm: image.ChecksumSHA256, Value: sum},
	})
	if err != nil {
		t.Fatalf("Import(): %v", err)
	}
	if err := h.runImport(t, imported.ID); err != nil {
		t.Fatalf("ImportHandler(): %v", err)
	}

	got, err := h.svc.Get(t.Context(), imported.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.State != image.StateAvailable {
		t.Fatalf("State = %q, want available (reason %v)", got.State, got.StateReason)
	}
	if string(got.Format) != string(hypervisor.FormatQCOW2) {
		t.Errorf("Format = %q, want qcow2: the volume must be the decompressed image", got.Format)
	}
	// The recorded size is the image's, not the download's.
	if got.SizeBytes == nil || *got.SizeBytes != int64(len(raw)) {
		t.Errorf("SizeBytes = %v, want %d (the decompressed size)", got.SizeBytes, len(raw))
	}
	info, err := h.hv.ImageInfo(t.Context(), h.volumeOf(t, got.ID))
	if err != nil {
		t.Fatalf("ImageInfo(): %v", err)
	}
	if info.VirtualSizeBytes != virtualDiskGB<<30 {
		t.Errorf("virtual size = %d, want %d", info.VirtualSizeBytes, int64(virtualDiskGB)<<30)
	}

	assertEmptyDir(t, h.temp)
}

func TestImportHandlerGzipFailures(t *testing.T) {
	t.Parallel()

	raw := qcow2test.Tiny(virtualDiskGB << 30)

	tests := []struct {
		name string
		// body is what the server publishes.
		body []byte
		// checksum is the expected value the import is created with; empty
		// means none.
		checksum string
		wantErr  error
	}{
		{
			// The checksum is of what was published. One taken over the
			// decompressed image is a different file, and refusing it is what
			// keeps the check meaningful.
			name:     "a checksum of the decompressed image is refused",
			body:     gzipped(t, raw),
			checksum: sha256Hex(raw),
			wantErr:  image.ErrChecksumMismatch,
		},
		{
			// 8 MiB of zeros compresses to a few KiB, well inside the
			// harness's 4 MiB download limit, and would expand past it.
			name:    "a stream that decompresses past the size limit is refused",
			body:    gzipped(t, make([]byte, 8<<20)),
			wantErr: imagefetch.ErrTooLarge,
		},
		{
			name:    "a stream with the gzip magic and a broken body is refused",
			body:    append([]byte{0x1f, 0x8b}, bytes.Repeat([]byte("not gzip"), 64)...),
			wantErr: image.ErrBadCompression,
		},
		{
			name:    "a truncated gzip stream is refused",
			body:    gzipped(t, raw)[:200],
			wantErr: image.ErrBadCompression,
		},
		{
			name:    "a gzip stream of nothing is refused",
			body:    gzipped(t, nil),
			wantErr: image.ErrBadCompression,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			url, _ := h.serveImage(t, tt.body)
			in := image.ImportInput{Name: "engine-gz", SourceURL: url}
			if tt.checksum != "" {
				in.Checksum = image.Checksum{Algorithm: image.ChecksumSHA256, Value: tt.checksum}
			}
			imported, err := h.svc.Import(t.Context(), in)
			if err != nil {
				t.Fatalf("Import(): %v", err)
			}

			err = h.runImport(t, imported.ID)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ImportHandler() error = %v, want %v", err, tt.wantErr)
			}
			// No volume, the row failed with a reason, and nothing left in the
			// staging directory: neither the download nor a partial copy.
			assertFailedAndClean(t, h, imported.ID)
			assertEmptyDir(t, h.temp)
		})
	}
}
