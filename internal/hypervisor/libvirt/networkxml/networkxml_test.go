package networkxml_test

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/libvirt/networkxml"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// natSpec is the network a NAT VPC becomes: everything set, nothing derived.
func natSpec() hypervisor.NetworkSpec {
	return hypervisor.NetworkSpec{
		Name:    "vpc-0a1b2c3d4e5f60718",
		Bridge:  "kmb0a1b2c3d",
		CIDR:    "10.0.0.0/16",
		Gateway: "10.0.0.1",
		Mode:    hypervisor.NetworkModeNAT,
		Domain:  "production",
	}
}

func TestBuildMatchesGolden(t *testing.T) {
	tests := map[string]struct {
		golden string
		spec   hypervisor.NetworkSpec
	}{
		"a nat vpc": {golden: "network.xml", spec: natSpec()},
		"an isolated vpc": {golden: "network-isolated.xml", spec: hypervisor.NetworkSpec{
			Name:    "vpc-11111111111111111",
			Bridge:  "kmb11111111",
			CIDR:    "192.168.0.0/16",
			Gateway: "192.168.0.1",
			Mode:    hypervisor.NetworkModeIsolated,
			Domain:  "lab",
		}},
		// The API defaults mode to nat; the builder must not lose that default.
		"an unset mode means nat": {golden: "network-default-mode.xml", spec: hypervisor.NetworkSpec{
			Name:    "vpc-22222222222222222",
			Bridge:  "kmb22222222",
			CIDR:    "10.1.0.0/16",
			Gateway: "10.1.0.1",
			Domain:  "stage",
		}},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := networkxml.Build(test.spec)
			if err != nil {
				t.Fatalf("Build() = %v", err)
			}
			assertGolden(t, test.golden, got)
		})
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	first, err := networkxml.Build(natSpec())
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	second, err := networkxml.Build(natSpec())
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if string(first) != string(second) {
		t.Error("Build() produced two different documents for one spec")
	}
}

func TestBuildEscapesOperatorInput(t *testing.T) {
	spec := natSpec()
	spec.Domain = `prod"><forward mode='nat'/><script>`

	got, err := networkxml.Build(spec)
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	out := string(got)

	if strings.Contains(out, "<script>") {
		t.Error("Build() let a domain name become structure")
	}
	// Only one forward element may exist: the one the mode requested, escaped
	// markup cannot inject another.
	if strings.Count(out, "<forward") != 1 {
		t.Errorf("Build() did not escape the name as data:\n%s", out)
	}

	// And the whole document must still parse as XML.
	if !strings.Contains(out, `prod&#34;&gt;&lt;forward mode=`) &&
		!strings.Contains(out, `prod&quot;&gt;&lt;forward mode=`) {
		t.Errorf("Build() did not escape the quote of the domain name:\n%s", out)
	}
}

func TestBuildEmitsNoForwardForIsolated(t *testing.T) {
	spec := natSpec()
	spec.Mode = hypervisor.NetworkModeIsolated

	got, err := networkxml.Build(spec)
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if strings.Contains(string(got), "<forward") {
		t.Errorf("Build() wrote a forward element for an isolated network:\n%s", got)
	}
}

func TestBuildEmitsNoRange(t *testing.T) {
	got, err := networkxml.Build(natSpec())
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if strings.Contains(string(got), "<range") {
		t.Errorf("Build() wrote a dhcp range; Kumo is the sole allocator:\n%s", got)
	}
}

func TestParseRoundTripsBuild(t *testing.T) {
	built, err := networkxml.Build(natSpec())
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	got, err := networkxml.Parse(built)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	want := hypervisor.NetworkInfo{
		Name:    "vpc-0a1b2c3d4e5f60718",
		Bridge:  "kmb0a1b2c3d",
		CIDR:    "10.0.0.0/16",
		Gateway: "10.0.0.1",
		Mode:    hypervisor.NetworkModeNAT,
		Domain:  "production",
	}
	if got.Name != want.Name || got.Bridge != want.Bridge || got.CIDR != want.CIDR ||
		got.Gateway != want.Gateway || got.Mode != want.Mode || got.Domain != want.Domain {
		t.Errorf("Parse(Build()) = %+v, want %+v", got, want)
	}
	if len(got.DHCPHosts) != 0 {
		t.Errorf("Parse(Build()) = %d dhcp hosts, want none", len(got.DHCPHosts))
	}
}

// TestParseAdoptedNetwork feeds the builder the description of a libvirt
// network it did not write — the adopted default network with its legacy dhcp
// range and hosts — and requires Parse to surface what Kumo cares about.
func TestParseAdoptedNetwork(t *testing.T) {
	// The name and bridge of an adopted default network survive unchanged; the
	// mode and gateway come back so the service can record them on the row.
	doc := []byte(`<network>
  <name>kumo-default</name>
  <uuid>6d79e7ba-16d7-45dc-b8ea-9db86ea52ac9</uuid>
  <forward mode='nat'><nat><port start='1024' end='65535'/></nat></forward>
  <bridge name='virbr-default' stp='on' delay='0'/>
  <mac address='52:54:00:98:76:54'/>
  <ip address='192.168.100.1' netmask='255.255.255.0'>
    <dhcp>
      <range start='192.168.100.10' end='192.168.100.250'/>
      <host mac='52:54:00:12:34:56' name='web-1' ip='192.168.100.10'/>
    </dhcp>
  </ip>
</network>
`)

	got, err := networkxml.Parse(doc)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	want := hypervisor.NetworkInfo{
		Name:    "kumo-default",
		Bridge:  "virbr-default",
		CIDR:    "192.168.100.0/24",
		Gateway: "192.168.100.1",
		Mode:    hypervisor.NetworkModeNAT,
		DHCPHosts: []hypervisor.DHCPHost{
			{MAC: "52:54:00:12:34:56", Name: "web-1", IP: "192.168.100.10"},
		},
	}
	if got.Name != want.Name || got.Bridge != want.Bridge || got.CIDR != want.CIDR ||
		got.Gateway != want.Gateway || got.Mode != want.Mode {
		t.Errorf("Parse() = %+v, want %+v", got, want)
	}
	if len(got.DHCPHosts) != len(want.DHCPHosts) {
		t.Fatalf("Parse() = %d dhcp hosts, want %d", len(got.DHCPHosts), len(want.DHCPHosts))
	}
	for i := range want.DHCPHosts {
		if got.DHCPHosts[i] != want.DHCPHosts[i] {
			t.Errorf("Parse().DHCPHosts[%d] = %+v, want %+v", i, got.DHCPHosts[i], want.DHCPHosts[i])
		}
	}
}

func TestHostXML(t *testing.T) {
	t.Parallel()

	host, err := networkxml.HostXML(hypervisor.DHCPHost{
		MAC: "52:54:00:12:34:56", Name: "web-1", IP: "10.0.1.10",
	})
	if err != nil {
		t.Fatalf("HostXML() = %v", err)
	}
	want := `<host mac="52:54:00:12:34:56" ip="10.0.1.10" name="web-1"></host>`
	if string(host) != want {
		t.Errorf("HostXML() = %s, want %s", host, want)
	}

	// A delete fragment identifies the host by MAC alone, the way
	// virNetworkUpdate's delete command matches it.
	gone, err := networkxml.HostXML(hypervisor.DHCPHost{MAC: "52:54:00:12:34:56"})
	if err != nil {
		t.Fatalf("HostXML() = %v", err)
	}
	if string(gone) != `<host mac="52:54:00:12:34:56"></host>` {
		t.Errorf("HostXML() delete fragment = %s", gone)
	}

	if _, err := networkxml.HostXML(hypervisor.DHCPHost{}); err == nil {
		t.Error("HostXML() accepted an empty host")
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mutate  func(*hypervisor.NetworkSpec)
		wantErr bool
	}{
		"a complete spec": {mutate: func(*hypervisor.NetworkSpec) {}},
		"no name":         {mutate: func(s *hypervisor.NetworkSpec) { s.Name = "" }, wantErr: true},
		"no bridge":       {mutate: func(s *hypervisor.NetworkSpec) { s.Bridge = "" }, wantErr: true},
		"no gateway":      {mutate: func(s *hypervisor.NetworkSpec) { s.Gateway = "" }, wantErr: true},
		"bad cidr":        {mutate: func(s *hypervisor.NetworkSpec) { s.CIDR = "10.0.0.0/8" }, wantErr: true},
		"bad mode":        {mutate: func(s *hypervisor.NetworkSpec) { s.Mode = "weird" }, wantErr: true},
		"no domain":       {mutate: func(s *hypervisor.NetworkSpec) { s.Domain = "" }},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			spec := natSpec()
			test.mutate(&spec)

			err := networkxml.Validate(spec)
			if test.wantErr && err == nil {
				t.Error("Validate() = nil, want an error")
			}
			if !test.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}

			// A spec that does not validate must not produce a document either.
			if _, err := networkxml.Build(spec); test.wantErr && err == nil {
				t.Error("Build() accepted a spec that Validate() rejected")
			}
		})
	}
}

// assertGolden compares against testdata/<name>, rewriting it under -update.
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create the golden directory: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write the golden file: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the golden file (run go test -update to create it): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("Build() does not match %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func TestDNSHostXML(t *testing.T) {
	t.Parallel()

	out, err := networkxml.DNSHostXML(hypervisor.DNSHost{IP: "192.168.100.10", Hostnames: []string{"api.orders.default"}})
	if err != nil {
		t.Fatalf("DNSHostXML(): %v", err)
	}
	if want := `<host ip="192.168.100.10"><hostname>api.orders.default</hostname></host>`; string(out) != want {
		t.Errorf("DNSHostXML() = %s, want %s", out, want)
	}

	for name, host := range map[string]hypervisor.DNSHost{
		"no address":        {Hostnames: []string{"api.orders.default"}},
		"an IPv6 address":   {IP: "fd00::1", Hostnames: []string{"api.orders.default"}},
		"no hostname":       {IP: "192.168.100.10"},
		"markup in a name":  {IP: "192.168.100.10", Hostnames: []string{"api.<x>.default"}},
		"an uppercase name": {IP: "192.168.100.10", Hostnames: []string{"API.orders.default"}},
		"an empty label":    {IP: "192.168.100.10", Hostnames: []string{"api..default"}},
		"a leading hyphen":  {IP: "192.168.100.10", Hostnames: []string{"-api.orders.default"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := networkxml.DNSHostXML(host); !errors.Is(err, hypervisor.ErrInvalidSpec) {
				t.Errorf("DNSHostXML() = %v, want ErrInvalidSpec", err)
			}
		})
	}
}

// A network read back lists the DNS records it holds.
func TestParseReadsDNSHosts(t *testing.T) {
	t.Parallel()

	info, err := networkxml.Parse([]byte(`<network><name>vpc-0a1b2c3d4e5f60718</name>
	  <bridge name='kmb0a1b2c3d'/><domain name='default'/>
	  <dns><host ip='192.168.100.10'><hostname>api.orders.default</hostname></host></dns>
	  <ip address='192.168.100.1' netmask='255.255.255.0'><dhcp/></ip></network>`))
	if err != nil {
		t.Fatalf("Parse(): %v", err)
	}
	if len(info.DNSHosts) != 1 || info.DNSHosts[0].IP != "192.168.100.10" ||
		len(info.DNSHosts[0].Hostnames) != 1 || info.DNSHosts[0].Hostnames[0] != "api.orders.default" {
		t.Errorf("DNSHosts = %+v", info.DNSHosts)
	}
}
