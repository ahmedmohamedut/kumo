package seediso_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kdomanski/iso9660"

	"github.com/ahmedmohamedut/kumo/internal/seediso"
)

// readSeed reads an ISO that Build wrote and returns its volume label together
// with the contents of every file in its root directory, keyed by the name
// without the ISO9660 version suffix.
//
// Everything is read while the handle is open: the image reads through the file
// it was opened with, so the caller cannot close it and then look at the files.
func readSeed(t *testing.T, path string) (string, map[string]string) {
	t.Helper()

	handle, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the seed ISO: %v", err)
	}
	defer handle.Close()

	image, err := iso9660.OpenImage(handle)
	if err != nil {
		t.Fatalf("the seed ISO is not readable: %v", err)
	}
	label, err := image.Label()
	if err != nil {
		t.Fatalf("read the volume label: %v", err)
	}
	root, err := image.RootDir()
	if err != nil {
		t.Fatalf("read the seed ISO root: %v", err)
	}
	children, err := root.GetAllChildren()
	if err != nil {
		t.Fatalf("read the seed ISO contents: %v", err)
	}

	files := make(map[string]string, len(children))
	for _, child := range children {
		if child.IsDir() {
			continue
		}
		content, err := io.ReadAll(child.Reader())
		if err != nil {
			t.Fatalf("read %s: %v", child.Name(), err)
		}
		name, _, _ := strings.Cut(child.Name(), ";")
		files[name] = string(content)
	}
	return label, files
}

func TestBuildProducesTheSeedCloudInitLooksFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i-0a1b2c3d4e5f60718-seed.iso")

	docs := seediso.Documents{
		MetaData:      "instance-id: i-0a1b2c3d4e5f60718\nlocal-hostname: web-1\n",
		UserData:      "#!/bin/sh\necho hello\n",
		NetworkConfig: "version: 2\nethernets:\n  all:\n    dhcp4: true\n",
	}
	if err := seediso.Build(path, docs); err != nil {
		t.Fatalf("Build() = %v", err)
	}

	label, files := readSeed(t, path)

	// The label is how cloud-init decides this is a NoCloud seed at all, so a
	// change to the constant is a change to whether instances boot.
	if !strings.EqualFold(label, seediso.VolumeLabel) {
		t.Errorf("the volume label is %q, want %q", label, seediso.VolumeLabel)
	}

	for name, want := range map[string]string{
		"meta-data":      docs.MetaData,
		"user-data":      docs.UserData,
		"network-config": docs.NetworkConfig,
	} {
		got, ok := files[name]
		if !ok {
			t.Errorf("the seed ISO has no %s, it holds %v", name, keys(files))
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestBuildOmitsWhatItWasNotGiven(t *testing.T) {
	tests := map[string]struct {
		docs   seediso.Documents
		absent []string
	}{
		"no user data": {
			docs:   seediso.Documents{MetaData: "instance-id: i-1\n", NetworkConfig: "version: 2\n"},
			absent: []string{"user-data"},
		},
		"no network config": {
			docs:   seediso.Documents{MetaData: "instance-id: i-1\n", UserData: "#cloud-config\n"},
			absent: []string{"network-config"},
		},
		"only user data": {
			docs:   seediso.Documents{UserData: "#cloud-config\n"},
			absent: []string{"meta-data", "network-config"},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "seed.iso")
			if err := seediso.Build(path, test.docs); err != nil {
				t.Fatalf("Build() = %v", err)
			}
			_, files := readSeed(t, path)
			for _, absent := range test.absent {
				if _, ok := files[absent]; ok {
					t.Errorf("the seed ISO has %s, want it left out", absent)
				}
			}
			if len(files) != 3-len(test.absent) {
				t.Errorf("the seed ISO holds %v, want exactly the documents that were given", keys(files))
			}
		})
	}
}

func TestBuildPreservesBytesExactly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.iso")

	// A user script with CRLF line endings and trailing whitespace: the encoder
	// must not normalise anything, because cloud-init runs the bytes it is given.
	userData := "#!/bin/sh\r\n  printf 'a\\r\\n'  \r\n# trailing comment\r\n"
	docs := seediso.Documents{MetaData: "instance-id: i-1\n", UserData: userData}
	if err := seediso.Build(path, docs); err != nil {
		t.Fatalf("Build() = %v", err)
	}

	_, files := readSeed(t, path)
	if got := files["user-data"]; got != userData {
		t.Errorf("user-data = %q, want %q", got, userData)
	}
}

func TestBuildRefusesAnEmptySeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.iso")

	if err := seediso.Build(path, seediso.Documents{}); err == nil {
		t.Fatal("Build() = nil, want an error for a seed with no documents")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("Build() left a file behind after refusing")
	}
}

func TestBuildRefusesAnEmptyPath(t *testing.T) {
	if err := seediso.Build("", seediso.Documents{MetaData: "instance-id: i-1\n"}); err == nil {
		t.Error("Build() = nil, want an error for an empty path")
	}
}

func TestBuildCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pools", "kumo-instances", "i-1-seed.iso")

	if err := seediso.Build(path, seediso.Documents{MetaData: "instance-id: i-1\n"}); err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("stat the seed ISO: %v", err)
	}
}

func TestBuildReplacesAnExistingSeedAndLeavesNoTemporaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "i-1-seed.iso")

	first := seediso.Documents{MetaData: "instance-id: i-1\n", UserData: "first\n"}
	if err := seediso.Build(path, first); err != nil {
		t.Fatalf("Build() = %v", err)
	}

	// A retry after a failed launch must find a usable seed, not the old one.
	second := seediso.Documents{MetaData: "instance-id: i-1\n", UserData: "second\n"}
	if err := seediso.Build(path, second); err != nil {
		t.Fatalf("Build() = %v", err)
	}

	_, files := readSeed(t, path)
	if got := files["user-data"]; got != "second\n" {
		t.Errorf("user-data = %q, want the replacement", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("the directory holds %v, want only the seed ISO", names)
	}
}

func TestEmpty(t *testing.T) {
	tests := map[string]struct {
		docs seediso.Documents
		want bool
	}{
		"nothing":          {docs: seediso.Documents{}, want: true},
		"only empty parts": {docs: seediso.Documents{MetaData: "", UserData: ""}, want: true},
		"meta-data":        {docs: seediso.Documents{MetaData: "instance-id: i-1\n"}},
		"user-data":        {docs: seediso.Documents{UserData: "#cloud-config\n"}},
		"network config":   {docs: seediso.Documents{NetworkConfig: "version: 2\n"}},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := test.docs.Empty(); got != test.want {
				t.Errorf("Empty() = %t, want %t", got, test.want)
			}
		})
	}
}

func keys(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	return names
}
