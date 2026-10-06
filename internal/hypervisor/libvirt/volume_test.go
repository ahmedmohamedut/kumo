package libvirt

import (
	"encoding/xml"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
)

// These are the parts of volume.go that need no libvirtd: the XML that libvirt
// would receive, the guards that run before a connection is opened, and the
// error text matching. Everything that talks to a daemon is covered by the
// `libvirt`-tagged integration test.

func TestVolumeXMLEscapesItsValues(t *testing.T) {
	t.Parallel()

	// A name is generated from an image id, but the marshaller is the only thing
	// standing between a future caller and XML injection.
	document, err := xml.Marshal(volumeXML{
		Type:       "file",
		Name:       "img-\"<script>&`.qcow2",
		Capacity:   capacityXML{Unit: "bytes", Bytes: 3 << 30},
		Allocation: allocationXML{Unit: "bytes", Bytes: 0},
		Target: &volumeTarget{
			Format:      volumeFormat{Type: "qcow2"},
			Permissions: &volumePermission{Mode: "644"},
		},
	})
	if err != nil {
		t.Fatalf("xml.Marshal(): %v", err)
	}

	got := string(document)
	// encoding/xml escapes a quote as a numeric reference and never self-closes an
	// empty element, so the expected document is what Go emits, not what a human
	// would write.
	want := "<volume type=\"file\"><name>img-&#34;&lt;script&gt;&amp;`.qcow2</name>" +
		"<capacity unit=\"bytes\">3221225472</capacity><allocation unit=\"bytes\">0</allocation>" +
		"<target><format type=\"qcow2\"></format><permissions><mode>644</mode></permissions></target></volume>"
	if got != want {
		t.Errorf("volume XML =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "<script>") {
		t.Error("the volume XML contains an unescaped value")
	}
}

func TestVolumeXMLOmitsBackingAndKeepsAllocationSparse(t *testing.T) {
	t.Parallel()

	document, err := xml.Marshal(volumeXML{
		Type:       "file",
		Name:       "img-abc.qcow2",
		Capacity:   capacityXML{Unit: "bytes", Bytes: 10 << 30},
		Allocation: allocationXML{Unit: "bytes", Bytes: 0},
		Target:     &volumeTarget{Format: volumeFormat{Type: "qcow2"}},
	})
	if err != nil {
		t.Fatalf("xml.Marshal(): %v", err)
	}

	got := string(document)
	if strings.Contains(got, "backing") {
		t.Errorf("volume XML = %s, want no backing element", got)
	}
	if !strings.Contains(got, "<allocation unit=\"bytes\">0</allocation>") {
		t.Errorf("volume XML = %s, want a zero allocation so the file stays sparse", got)
	}
	if !strings.Contains(got, ">10737418240<") {
		t.Errorf("volume XML = %s, want the virtual size as the capacity", got)
	}
}

func TestVolumeDescReadsFormatAndBacking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		xml     string
		format  string
		backing string
	}{
		{
			name:   "plain qcow2",
			xml:    `<volume><name>img-abc.qcow2</name><target><format type='qcow2'/></target></volume>`,
			format: "qcow2",
		},
		{
			name:    "delta chain",
			xml:     `<volume><name>img-abc.qcow2</name><backingStore><path>/pools/images/base.qcow2</path></backingStore><target><format type='qcow2'/></target></volume>`,
			format:  "qcow2",
			backing: "/pools/images/base.qcow2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var desc volumeDesc
			if err := xml.Unmarshal([]byte(tt.xml), &desc); err != nil {
				t.Fatalf("xml.Unmarshal(): %v", err)
			}
			if desc.Name != "img-abc.qcow2" {
				t.Errorf("Name = %q", desc.Name)
			}
			if desc.Format.Type != tt.format {
				t.Errorf("Format = %q, want %q", desc.Format.Type, tt.format)
			}
			if tt.backing == "" {
				if desc.Backing != nil {
					t.Errorf("Backing = %+v, want nil", desc.Backing)
				}
				return
			}
			if desc.Backing == nil || desc.Backing.Path != tt.backing {
				t.Errorf("Backing = %+v, want the path %q", desc.Backing, tt.backing)
			}
		})
	}
}

func TestValidateFormat(t *testing.T) {
	t.Parallel()

	if err := validateFormat(hypervisor.FormatQCOW2); err != nil {
		t.Errorf("validateFormat(qcow2) = %v, want nil", err)
	}
	if err := validateFormat(hypervisor.FormatRaw); err != nil {
		t.Errorf("validateFormat(raw) = %v, want nil", err)
	}

	err := validateFormat(hypervisor.ImageFormat("vmdk"))
	if !errors.Is(err, hypervisor.ErrInvalidState) {
		t.Errorf("validateFormat(vmdk) = %v, want ErrInvalidState", err)
	}
	if !strings.Contains(err.Error(), "vmdk") {
		t.Errorf("validateFormat(vmdk) = %q, want it to name the format", err)
	}
}

func TestImagesPoolNeedsARecordedInfra(t *testing.T) {
	t.Parallel()

	// A client that never bootstrapped has no pool to import into. Reporting
	// that before opening a connection keeps the fake and the real backend's
	// failure modes comparable.
	client, err := New(slog.New(slog.DiscardHandler), "unix:///nonexistent/libvirt-sock")
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	if _, _, ok := client.imagesPool(); ok {
		t.Error("imagesPool() = ok, want false before EnsureInfra")
	}

	err = client.ImportImage(t.Context(), "img-abc.qcow2",
		strings.NewReader("x"), hypervisor.ImportSpec{
			SizeBytes:        1,
			VirtualSizeBytes: 1 << 20,
			Format:           hypervisor.FormatQCOW2,
		})
	if !errors.Is(err, hypervisor.ErrInvalidState) {
		t.Errorf("ImportImage() = %v, want ErrInvalidState", err)
	}
}

func TestImportImageRejectsBadInput(t *testing.T) {
	t.Parallel()

	client, err := New(slog.New(slog.DiscardHandler), "unix:///nonexistent/libvirt-sock")
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	tests := []struct {
		name string
		vol  string
		spec hypervisor.ImportSpec
	}{
		{name: "no name", vol: "", spec: hypervisor.ImportSpec{SizeBytes: 1, Format: hypervisor.FormatQCOW2}},
		{
			name: "empty image",
			vol:  "img-abc.qcow2",
			spec: hypervisor.ImportSpec{SizeBytes: 0, VirtualSizeBytes: 1 << 20, Format: hypervisor.FormatQCOW2},
		},
		{
			name: "qcow2 without a virtual size",
			vol:  "img-abc.qcow2",
			spec: hypervisor.ImportSpec{SizeBytes: 1, Format: hypervisor.FormatQCOW2},
		},
		{
			name: "unsupported format",
			vol:  "img-abc.vmdk",
			spec: hypervisor.ImportSpec{SizeBytes: 1, VirtualSizeBytes: 1 << 20, Format: hypervisor.ImageFormat("vmdk")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := client.ImportImage(t.Context(), tt.vol, strings.NewReader("x"), tt.spec)
			if !errors.Is(err, hypervisor.ErrInvalidState) {
				t.Errorf("ImportImage() = %v, want ErrInvalidState", err)
			}
		})
	}
}

func TestRawWithoutAVirtualSizeIsItsOwnCapacity(t *testing.T) {
	t.Parallel()

	// A raw image has no header to read, so its size is the capacity. This must
	// not reach a connection: the guard is what keeps a 0600 file out of the
	// pool, and a raw file with no size is rejected before that.
	client, err := New(slog.New(slog.DiscardHandler), "unix:///nonexistent/libvirt-sock")
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	err = client.ImportImage(t.Context(), "img-abc.img", strings.NewReader("payload"), hypervisor.ImportSpec{
		SizeBytes: 7,
		Format:    hypervisor.FormatRaw,
	})
	if !errors.Is(err, hypervisor.ErrInvalidState) {
		t.Fatalf("ImportImage() = %v, want ErrInvalidState without a pool", err)
	}
}

func TestPlaceFileMakesTheVolumeReadable(t *testing.T) {
	t.Parallel()

	// The pool path is derived from the recorded infra, so the fast path can be
	// exercised without a daemon up to the refresh, which is the one call that
	// needs one.
	client, err := New(slog.New(slog.DiscardHandler), "unix:///nonexistent/libvirt-sock")
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	dataDir := t.TempDir()
	client.setInfra(hypervisor.InfraConfig{
		ImagesPool: "kumo-images", InstancesPool: "kumo-instances",
		Network: "kumo-default", DataDir: dataDir,
	})

	staging := t.TempDir()
	staged := filepath.Join(staging, "download")
	if err := os.WriteFile(staged, []byte("image bytes"), 0o600); err != nil {
		t.Fatalf("write the staged file: %v", err)
	}

	file, err := os.Open(staged)
	if err != nil {
		t.Fatalf("open the staged file: %v", err)
	}
	defer file.Close() //nolint:errcheck // test cleanup

	done, err := client.placeFile(t.Context(), "img-abc.qcow2", file, int64(len("image bytes")))
	if err == nil {
		t.Fatal("placeFile() = nil, want the refresh to fail without a daemon")
	}
	if !done {
		t.Error("placeFile() = false, want true for a local directory pool")
	}

	target := filepath.Join(dataDir, "pools", "images", "img-abc.qcow2")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat the placed volume: %v", err)
	}
	// A staged download is 0600; libvirt would fail to open the volume as its
	// own QEMU user if the mode were left alone.
	perm := info.Mode().Perm()
	if perm&0o444 == 0 {
		t.Errorf("volume mode = %04o, want it readable after placement", perm)
	}
	if perm&0o022 != 0 {
		t.Errorf("volume mode = %04o, want it not group or world writable", perm)
	}

	contents, err := os.ReadFile(target) //nolint:gosec // reading back a test fixture
	if err != nil {
		t.Fatalf("read the placed volume: %v", err)
	}
	if string(contents) != "image bytes" {
		t.Errorf("placed contents = %q, want the staged bytes", contents)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat the staged file = %v, want it gone after the rename", err)
	}
}

func TestCopyFileCopiesExactlyTheDeclaredSize(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("0123456789"), 0o600); err != nil {
		t.Fatalf("write the source: %v", err)
	}

	file, err := os.Open(src)
	if err != nil {
		t.Fatalf("open the source: %v", err)
	}
	defer file.Close() //nolint:errcheck // test cleanup

	dst := filepath.Join(dir, "dst")
	if err := copyFile(file, dst, 4); err != nil {
		t.Fatalf("copyFile(): %v", err)
	}

	contents, err := os.ReadFile(dst) //nolint:gosec // reading back a test fixture
	if err != nil {
		t.Fatalf("read the copy: %v", err)
	}
	if string(contents) != "0123" {
		t.Errorf("copied %q, want %q: the upload length is the file size, not the virtual size",
			contents, "0123")
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat the copy: %v", err)
	}
	if perm := info.Mode().Perm(); perm != dirPoolVolumePerms {
		t.Errorf("copy mode = %04o, want %04o", perm, dirPoolVolumePerms)
	}

	// Fewer bytes than declared is a truncated download, not a successful copy.
	if err := copyFile(file, dst, 100); err == nil {
		t.Error("copyFile() = nil, want an error when the source is short")
	}
}

// The daemon words a missing resource differently depending on what it is and on
// which libvirt version answers, so each shape gets its own case.
var (
	//nolint:staticcheck // ST1005: this is libvirt's own wording, matched verbatim
	errVolNotFound      = errors.New("Storage vol not found")
	errNoSuchVolume     = errors.New("no such volume")
	errNoSuchPool       = errors.New("no such storage pool")
	errNoSuchNetwork    = errors.New("no such network")
	errVolAlreadyExists = errors.New("volume already exists")
	errOther            = errors.New("authentication unavailable")
)

func TestLibvirtErrorTextIsRecognised(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          error
		notFound     bool
		alreadyExist bool
	}{
		{name: "nil", err: nil},
		{name: "missing volume", err: errVolNotFound, notFound: true},
		{name: "missing volume, other spelling", err: errNoSuchVolume, notFound: true},
		{name: "missing pool", err: errNoSuchPool, notFound: true},
		{name: "missing network", err: errNoSuchNetwork, notFound: true},
		{name: "already exists", err: errVolAlreadyExists, alreadyExist: true},
		{name: "other", err: errOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isNotFound(tt.err); got != tt.notFound {
				t.Errorf("isNotFound(%v) = %t, want %t", tt.err, got, tt.notFound)
			}
			if got := isAlreadyExists(tt.err); got != tt.alreadyExist {
				t.Errorf("isAlreadyExists(%v) = %t, want %t", tt.err, got, tt.alreadyExist)
			}
		})
	}
}
