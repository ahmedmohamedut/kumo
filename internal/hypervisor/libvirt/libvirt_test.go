package libvirt

import "testing"

// capsDoc wraps guest elements in the shape `virsh capabilities` prints, with
// the host section libvirt always puts first, so the parser is exercised on a
// document like the real one rather than on the fragment it reads.
func capsDoc(guests string) []byte {
	return []byte(`<capabilities>
  <host>
    <uuid>00000000-0000-0000-0000-000000000000</uuid>
    <cpu><arch>x86_64</arch></cpu>
  </host>` + guests + `
</capabilities>`)
}

const hvmGuest = `
  <guest>
    <os_type>hvm</os_type>
    <arch name='x86_64'>
      <wordsize>64</wordsize>
      <emulator>/usr/bin/qemu-system-x86_64</emulator>
      <machine maxCpus='255'>pc-q35-9.2</machine>
      <domain type='qemu'/>
      <domain type='kvm'/>
    </arch>
  </guest>`

// tcgGuest is what a host without usable KVM lists: the same guest, with only
// software emulation.
const tcgGuest = `
  <guest>
    <os_type>hvm</os_type>
    <arch name='x86_64'>
      <emulator>/usr/bin/qemu-system-x86_64</emulator>
      <domain type='qemu'/>
    </arch>
  </guest>`

func TestCapabilitiesOfferKVM(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		doc     []byte
		want    bool
		wantErr bool
	}{
		{name: "an hvm guest with a kvm domain", doc: capsDoc(hvmGuest), want: true},
		{name: "software emulation only", doc: capsDoc(tcgGuest), want: false},
		{
			name: "kvm on a second architecture still counts",
			doc: capsDoc(tcgGuest + `
  <guest>
    <os_type>hvm</os_type>
    <arch name='i686'><domain type='qemu'/><domain type='kvm'/></arch>
  </guest>`),
			want: true,
		},
		{
			// Only hvm guests are domains Kumo launches; a kvm domain type
			// under another os_type is not evidence for them.
			name: "a kvm domain on a non-hvm guest",
			doc: capsDoc(`
  <guest>
    <os_type>xen</os_type>
    <arch name='x86_64'><domain type='kvm'/></arch>
  </guest>`),
			want: false,
		},
		{name: "no guests at all", doc: capsDoc(""), want: false},
		{name: "not XML", doc: []byte("libvirt said no"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := capabilitiesOfferKVM(tt.doc)
			if (err != nil) != tt.wantErr {
				t.Fatalf("capabilitiesOfferKVM() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("capabilitiesOfferKVM() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDevKVMPresent(t *testing.T) {
	t.Parallel()
	// The answer depends on the machine running the test; what is pinned here
	// is that asking never panics or blocks.
	_ = DevKVMPresent()
}
