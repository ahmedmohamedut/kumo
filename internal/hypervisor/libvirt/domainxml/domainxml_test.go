package domainxml_test

import (
	"encoding/xml"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/libvirt/domainxml"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// testSpec is the instance M4 launches: everything set, nothing derived. The
// golden files then show the document a real launch produces.
func testSpec() hypervisor.DomainSpec {
	return hypervisor.DomainSpec{
		ID:             "i-0a1b2c3d4e5f60718",
		Name:           "web-1",
		UUID:           "3f2b1c0d-0000-4000-8000-000000000001",
		VCPUs:          2,
		MemoryMiB:      2048,
		Machine:        "q35",
		Arch:           "x86_64",
		Network:        "kumo-default",
		MACAddress:     "52:54:00:0a:0b:0c",
		Firmware:       "",
		RootDiskPath:   "/var/lib/kumo/pools/kumo-instances/i-0a1b2c3d4e5f60718-root.qcow2",
		SeedISOPath:    "/var/lib/kumo/pools/kumo-instances/i-0a1b2c3d4e5f60718-seed.iso",
		ConsoleLogPath: "/var/lib/kumo/console/i-0a1b2c3d4e5f60718.log",
	}
}

func TestBuildMatchesGolden(t *testing.T) {
	tests := map[string]struct {
		golden string
		spec   hypervisor.DomainSpec
	}{
		"a fully specified instance": {golden: "domain.xml", spec: testSpec()},
		"defaults fill in what the service left empty": {golden: "domain-defaults.xml", spec: hypervisor.DomainSpec{
			ID:           "i-11111111111111111",
			Name:         "bare",
			UUID:         "00000000-0000-4000-8000-000000000002",
			VCPUs:        1,
			MemoryMiB:    512,
			RootDiskPath: "/var/lib/kumo/pools/kumo-instances/i-11111111111111111-root.qcow2",
			SeedISOPath:  "/var/lib/kumo/pools/kumo-instances/i-11111111111111111-seed.iso",
		}},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := domainxml.Build(test.spec)
			if err != nil {
				t.Fatalf("Build() = %v", err)
			}
			assertGolden(t, test.golden, got)
		})
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	first, err := domainxml.Build(testSpec())
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	second, err := domainxml.Build(testSpec())
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if string(first) != string(second) {
		t.Error("Build() produced two different documents for one spec")
	}
}

func TestBuildEscapesOperatorInput(t *testing.T) {
	spec := testSpec()
	spec.Name = `web"><script>alert(1)</script>&`
	spec.RootDiskPath = "/var/lib/kumo/pools/a&b/i-1-root.qcow2"

	got, err := domainxml.Build(spec)
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	out := string(got)

	if strings.Contains(out, "<script>") {
		t.Error("Build() let a name become structure")
	}
	// The encoder escapes the quote as a numeric reference in character data,
	// which is equivalent; the point is that it stays data.
	if !strings.Contains(out, "<kumo:name>web&#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;&amp;</kumo:name>") {
		t.Errorf("Build() did not escape the name as data:\n%s", out)
	}
	// & inside an attribute must be a reference, never a bare ampersand.
	if !strings.Contains(out, `file="/var/lib/kumo/pools/a&amp;b/i-1-root.qcow2"`) {
		t.Errorf("Build() did not escape the disk path:\n%s", out)
	}
	// And the whole document must still parse as XML.
	if err := xml.Unmarshal(got, new(struct {
		XMLName xml.Name
	})); err != nil {
		t.Errorf("Build() produced XML that does not parse: %v", err)
	}
}

func TestBuildEmitsNothingWhenAValueIsEmpty(t *testing.T) {
	got, err := domainxml.Build(func() hypervisor.DomainSpec {
		spec := testSpec()
		spec.ConsoleLogPath = ""
		spec.Firmware = ""
		return spec
	}())
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	out := string(got)

	if strings.Contains(out, "<log ") {
		t.Errorf("Build() wrote a log element with no path:\n%s", out)
	}
	if strings.Contains(out, "firmware=") {
		t.Errorf("Build() wrote a firmware attribute it was not given:\n%s", out)
	}
}

func TestValidate(t *testing.T) {
	tests := map[string]struct {
		mutate  func(*hypervisor.DomainSpec)
		wantErr bool
	}{
		"a complete spec":     {mutate: func(*hypervisor.DomainSpec) {}},
		"no id":               {mutate: func(s *hypervisor.DomainSpec) { s.ID = "" }, wantErr: true},
		"no name":             {mutate: func(s *hypervisor.DomainSpec) { s.Name = "" }, wantErr: true},
		"no vcpus":            {mutate: func(s *hypervisor.DomainSpec) { s.VCPUs = 0 }, wantErr: true},
		"negative vcpus":      {mutate: func(s *hypervisor.DomainSpec) { s.VCPUs = -1 }, wantErr: true},
		"no memory":           {mutate: func(s *hypervisor.DomainSpec) { s.MemoryMiB = 0 }, wantErr: true},
		"no root disk":        {mutate: func(s *hypervisor.DomainSpec) { s.RootDiskPath = "" }, wantErr: true},
		"no seed iso":         {mutate: func(s *hypervisor.DomainSpec) { s.SeedISOPath = "" }, wantErr: true},
		"no console log path": {mutate: func(s *hypervisor.DomainSpec) { s.ConsoleLogPath = "" }},
		"no uuid":             {mutate: func(s *hypervisor.DomainSpec) { s.UUID = "" }},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			spec := testSpec()
			test.mutate(&spec)

			err := domainxml.Validate(spec)
			switch {
			case test.wantErr && err == nil:
				t.Error("Validate() = nil, want an error")
			case test.wantErr && !strings.Contains(err.Error(), spec.ID+""):
				t.Errorf("Validate() = %v, want it to name the domain", err)
			case !test.wantErr && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			}

			// A spec that does not validate must not produce a document either:
			// a domain with no disk would boot to nothing.
			if _, err := domainxml.Build(spec); test.wantErr && err == nil {
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
