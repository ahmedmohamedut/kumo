package libvirt

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
)

// Root disk sizing runs on the capacity libvirt reports for the image volume,
// which only a live daemon answers. The comparison itself has no daemon in it,
// so it gets a table test here.
func TestRootDiskHoldsImage(t *testing.T) {
	t.Parallel()

	const gib = uint64(1 << 30)

	tests := []struct {
		name     string
		root     uint64
		image    uint64
		expected bool
	}{
		// The reported capacity is the image's virtual size, not its footprint.
		{name: "larger root disk than image", root: 20 * gib, image: 2 * gib, expected: true},
		{name: "equal sizes", root: 2 * gib, image: 2 * gib, expected: true},
		{name: "root smaller than image", root: 1 * gib, image: 2 * gib, expected: false},
		{name: "image without a reported capacity", root: 1 * gib, image: 0, expected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := rootDiskHoldsImage(tt.root, tt.image); got != tt.expected {
				t.Errorf("rootDiskHoldsImage(%d, %d) = %t, want %t",
					tt.root, tt.image, got, tt.expected)
			}
		})
	}
}

// The local path of a file pool volume is reported as <target><path>; older
// libvirts used the <source file=…> attribute instead. Both must decode, and a
// volume on another filesystem must come back empty, because the caller then
// knows it has to be addressed through the API.
func TestVolumeSourcePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		descriptor string
		want       string
	}{
		{
			name: "modern dir pool reports target path",
			descriptor: `<volume type='file'><name>img-a.qcow2</name>` +
				`<target><path>/var/lib/kumo/pools/images/img-a.qcow2</path>` +
				`<format type='qcow2'/></target></volume>`,
			want: "/var/lib/kumo/pools/images/img-a.qcow2",
		},
		{
			name: "legacy source file attribute",
			descriptor: `<volume type='file'><name>img-a.qcow2</name>` +
				`<source file='/srv/images/img-a.qcow2'/><target>` +
				`<path>/srv/images/img-a.qcow2</path></target></volume>`,
			want: "/srv/images/img-a.qcow2",
		},
		{
			name:       "source device attribute",
			descriptor: `<volume type='block'><name>vda</name><source dev='/dev/vg0/vda'/></volume>`,
			want:       "/dev/vg0/vda",
		},
		{
			name: "source first wins over target",
			descriptor: `<volume type='block'><name>vda</name><source dev='/dev/vg0/vda'/>` +
				`<target><path>/dev/mapper/vg0-vda</path></target></volume>`,
			want: "/dev/vg0/vda",
		},
		{
			name:       "remote volume has no local path",
			descriptor: `<volume type='network'><name>f</name><source protocol='nbd' name='img'/></volume>`,
			want:       "",
		},
		{
			name:       "malformed descriptor",
			descriptor: "not xml <uh oh",
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := volumeSourcePath(tt.descriptor); got != tt.want {
				t.Errorf("volumeSourcePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

// libvirt's storage volume XML names the backing store <backingStore>, and
// hands its <path> to qemu-img as the backing file. The old <backing> (with a
// invented <volume_name>) was silently ignored by the daemon, producing an
// overlay that was a blank disk: this test pins the exact XML that works.
func TestRootDiskXMLUsesBackingStoreWithAnAbsolutePath(t *testing.T) {
	t.Parallel()

	document, err := xml.Marshal(rootDiskXML{
		Type:     "file",
		Name:     "i-f00d-root.qcow2",
		Capacity: capacityXML{Unit: "bytes", Bytes: 10 << 30},
		Target: &volumeTarget{
			Format:      volumeFormat{Type: string(hypervisor.FormatQCOW2)},
			Permissions: &volumePermission{Mode: "644"},
		},
		Backing: &rootDiskBacking{
			Path:   "/var/lib/kumo/pools/images/img-abc.qcow2",
			Format: &volumeFormat{Type: string(hypervisor.FormatQCOW2)},
		},
	})
	if err != nil {
		t.Fatalf("xml.Marshal(): %v", err)
	}

	got := string(document)
	if !strings.Contains(got, "<backingStore><path>/var/lib/kumo/pools/images/img-abc.qcow2</path>") {
		t.Errorf("root disk XML = %s, want a backingStore with the absolute image path", got)
	}
	if strings.Contains(got, "<backing>") {
		t.Errorf("root disk XML = %s, want the libvirt spelling <backingStore>, not <backing>", got)
	}
	if strings.Contains(got, "volume_name") {
		t.Errorf("root disk XML = %s, want no volume_name element", got)
	}
	// The target may carry <permissions>; the backing store must not, so only
	// look at the backingStore element itself.
	start := strings.Index(got, "<backingStore>")
	end := strings.Index(got, "</backingStore>") + len("</backingStore>")
	if start < 0 || end < start {
		t.Fatalf("root disk XML = %s, want a backingStore element", got)
	}
	if strings.Contains(got[start:end], "permissions") {
		t.Errorf("root disk XML = %s, want the backing store to not carry permissions", got)
	}
}
