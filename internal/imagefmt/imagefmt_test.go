package imagefmt_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/imagefmt"
	"github.com/ahmedmohamedut/kumo/internal/imagefmt/qcow2test"
)

func TestInspect(t *testing.T) {
	t.Parallel()

	const virtualSize = 10 << 30 // 10 GiB

	tests := []struct {
		name      string
		image     []byte
		want      imagefmt.Info
		wantErr   error
		wantWords string
	}{
		{
			name:  "qcow2",
			image: qcow2test.Tiny(virtualSize),
			want: imagefmt.Info{
				Format:           imagefmt.FormatQCOW2,
				SizeBytes:        qcow2test.HeaderLen + 1024,
				VirtualSizeBytes: virtualSize,
			},
		},
		{
			name:  "raw",
			image: qcow2test.Raw(2 << 20),
			want: imagefmt.Info{
				Format:           imagefmt.FormatRaw,
				SizeBytes:        2 << 20,
				VirtualSizeBytes: 2 << 20, // a raw image is its own virtual size
			},
		},
		{
			// A file that is neither qcow2 nor a whole number of sectors is
			// almost certainly not a disk.
			name:      "truncated raw",
			image:     qcow2test.Raw(1000),
			wantErr:   imagefmt.ErrUnknownFormat,
			wantWords: "multiple of 512",
		},
		{
			name:      "too short to hold a magic",
			image:     qcow2test.Raw(2),
			wantErr:   imagefmt.ErrTruncated,
			wantWords: "2 bytes",
		},
		{
			name:      "qcow2 with a backing file",
			image:     withBackingFile(t),
			wantErr:   imagefmt.ErrBackingFile,
			wantWords: "backing file",
		},
		{
			name:      "qcow2 v1",
			image:     withVersion(t, 1),
			wantErr:   imagefmt.ErrUnsupportedVersion,
			wantWords: "unsupported qcow2 version: 1",
		},
		{
			name:      "qcow2 with an absurd cluster size",
			image:     withClusterBits(t, 30),
			wantErr:   imagefmt.ErrUnsupportedVersion,
			wantWords: "cluster bits 30",
		},
		{
			name:      "qcow2 claiming a 512 byte disk",
			image:     withVirtualSize(t, 4096),
			wantErr:   imagefmt.ErrImplausibleSize,
			wantWords: "smaller than 1 MiB",
		},
		{
			name:      "qcow2 claiming an absurd disk",
			image:     withVirtualSize(t, 1<<57),
			wantErr:   imagefmt.ErrImplausibleSize,
			wantWords: "larger than 64 PiB",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "image")
			if err := os.WriteFile(path, tt.image, 0o600); err != nil {
				t.Fatalf("write the image: %v", err)
			}

			got, err := imagefmt.Inspect(path)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Inspect() error = %v, want %v", err, tt.wantErr)
				}
				if tt.wantWords != "" && !strings.Contains(err.Error(), tt.wantWords) {
					t.Errorf("Inspect() error = %q, want it to mention %q", err, tt.wantWords)
				}
				return
			}
			if err != nil {
				t.Fatalf("Inspect(): %v", err)
			}
			if got != tt.want {
				t.Errorf("Inspect() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// A qcow2 header must be read without loading the whole file, so a large
// virtual size over a small file is the normal case and must work.
func TestInspectAcceptsASparseFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sparse.qcow2")
	if err := os.WriteFile(path, qcow2test.Header(500<<30), 0o600); err != nil {
		t.Fatalf("write the header: %v", err)
	}

	got, err := imagefmt.Inspect(path)
	if err != nil {
		t.Fatalf("Inspect(): %v", err)
	}
	if got.VirtualSizeBytes != 500<<30 {
		t.Errorf("virtual size = %d, want %d", got.VirtualSizeBytes, int64(500<<30))
	}
	if got.SizeBytes != qcow2test.HeaderLen {
		t.Errorf("size on disk = %d, want %d", got.SizeBytes, qcow2test.HeaderLen)
	}
}

func TestInspectReportsAMissingFile(t *testing.T) {
	t.Parallel()

	if _, err := imagefmt.Inspect(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("Inspect() error = nil, want an open error")
	}
}

// withBackingFile returns a header that points at a backing file.
func withBackingFile(t *testing.T) []byte {
	t.Helper()

	image := qcow2test.Tiny(10 << 30)
	image[8] = 0x40 // backing file name at offset 0x40
	return image
}

func withVersion(t *testing.T, version uint32) []byte {
	t.Helper()

	image := qcow2test.Tiny(10 << 30)
	for i := range 4 {
		image[4+i] = byte(version >> (24 - 8*i))
	}
	return image
}

func withClusterBits(t *testing.T, bits uint32) []byte {
	t.Helper()

	image := qcow2test.Tiny(10 << 30)
	image[20], image[21], image[22], image[23] = 0, 0, 0, byte(bits)
	return image
}

func withVirtualSize(t *testing.T, size uint64) []byte {
	t.Helper()

	image := qcow2test.Tiny(10 << 30)
	for i := 0; i < 8; i++ {
		image[24+i] = byte(size >> (56 - 8*i))
	}
	return image
}
