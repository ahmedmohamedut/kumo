package cloudinit_test

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ahmedmohamedut/kumo/internal/service/cloudinit"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample ada@laptop"

// crlf gives a MIME part the CRLF line endings the format requires, so the
// expectations below stay readable as ordinary text.
func crlf(document string) string {
	return strings.ReplaceAll(document, "\n", "\r\n")
}

func TestUserData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  cloudinit.Request
		// want is the exact expected user-data. Empty means "nothing at all",
		// which is the one case where the seed carries no user-data part.
		want string
	}{
		{
			name: "no user data and no key says nothing",
			req:  cloudinit.Request{},
			want: "",
		},
		{
			name: "a key alone generates the default user",
			req:  cloudinit.Request{PublicKey: testKey, DefaultUser: "ubuntu"},
			want: `#cloud-config
users:
  - name: ubuntu
    ssh_authorized_keys:
      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample ada@laptop
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
ssh_pwauth: false
`,
		},
		{
			name: "an image without a default user falls back to root",
			req:  cloudinit.Request{PublicKey: testKey},
			want: `#cloud-config
users:
  - name: root
    ssh_authorized_keys:
      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample ada@laptop
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
ssh_pwauth: false
`,
		},
		{
			name: "user data without a key is passed through untouched",
			req: cloudinit.Request{
				UserData:    "#cloud-config\npackages:\n  - nginx\n",
				DefaultUser: "ubuntu",
			},
			want: "#cloud-config\npackages:\n  - nginx\n",
		},
		{
			name: "a script without a key keeps its shebang",
			req:  cloudinit.Request{UserData: "#!/bin/sh\necho hello\n"},
			want: "#!/bin/sh\necho hello\n",
		},
		{
			name: "a key is merged into a cloud-config that has no users",
			req: cloudinit.Request{
				UserData:    "#cloud-config\npackages:\n  - nginx\n",
				PublicKey:   testKey,
				DefaultUser: "ubuntu",
			},
			want: `#cloud-config
packages:
    - nginx
users:
    - name: ubuntu
      ssh_authorized_keys:
        - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample ada@laptop
`,
		},
		{
			name: "a key is merged into the caller's own user entry",
			req: cloudinit.Request{
				UserData:    "#cloud-config\nusers:\n  - name: ubuntu\n    shell: /bin/zsh\n",
				PublicKey:   testKey,
				DefaultUser: "ubuntu",
			},
			want: `#cloud-config
users:
    - name: ubuntu
      shell: /bin/zsh
      ssh_authorized_keys:
        - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample ada@laptop
`,
		},
		{
			name: "an existing key list is extended, not replaced",
			req: cloudinit.Request{
				UserData: "#cloud-config\nusers:\n  - name: ubuntu\n" +
					"    ssh_authorized_keys:\n      - ssh-rsa AAAAother\n",
				PublicKey:   testKey,
				DefaultUser: "ubuntu",
			},
			want: `#cloud-config
users:
    - name: ubuntu
      ssh_authorized_keys:
        - ssh-rsa AAAAother
        - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample ada@laptop
`,
		},
		{
			name: "a key is not added twice",
			req: cloudinit.Request{
				UserData:    "#cloud-config\nusers:\n  - name: ubuntu\n    ssh_authorized_keys:\n      - " + testKey + "\n",
				PublicKey:   testKey,
				DefaultUser: "ubuntu",
			},
			want: `#cloud-config
users:
    - name: ubuntu
      ssh_authorized_keys:
        - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample ada@laptop
`,
		},
		{
			name: "a second user entry is left alone",
			req: cloudinit.Request{
				UserData:    "#cloud-config\nusers:\n  - name: deploy\n    lock_passwd: true\n",
				PublicKey:   testKey,
				DefaultUser: "ubuntu",
			},
			want: `#cloud-config
users:
    - name: deploy
      lock_passwd: true
    - name: ubuntu
      ssh_authorized_keys:
        - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample ada@laptop
`,
		},
		{
			name: "a script plus a key becomes two parts",
			req: cloudinit.Request{
				UserData:    "#!/bin/sh\napt-get install -y nginx\n",
				PublicKey:   testKey,
				DefaultUser: "ubuntu",
			},
			want: crlf(`Content-Type: multipart/mixed; boundary="==KUMO-SEED=="

--==KUMO-SEED==
Content-Type: text/cloud-config

#cloud-config
users:
  - name: ubuntu
    ssh_authorized_keys:
      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample ada@laptop
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
ssh_pwauth: false
--==KUMO-SEED==
Content-Type: text/x-shellscript

#!/bin/sh
apt-get install -y nginx
--==KUMO-SEED==--
`),
		},
		{
			name: "a MIME document of the caller's own travels verbatim",
			req: cloudinit.Request{
				UserData:    "Content-Type: multipart/mixed; boundary=\"mine\"\n\n--mine--\n",
				PublicKey:   testKey,
				DefaultUser: "ubuntu",
			},
			want: crlf(`Content-Type: multipart/mixed; boundary="==KUMO-SEED=="

--==KUMO-SEED==
Content-Type: text/cloud-config

#cloud-config
users:
  - name: ubuntu
    ssh_authorized_keys:
      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample ada@laptop
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
ssh_pwauth: false
--==KUMO-SEED==
Content-Type: text/plain

Content-Type: multipart/mixed; boundary="mine"

--mine--
--==KUMO-SEED==--
`),
		},
		{
			name: "cloud-config with the header after leading whitespace still merges",
			req: cloudinit.Request{
				UserData:    "\n\n#cloud-config\nusers:\n  - name: ubuntu\n",
				PublicKey:   testKey,
				DefaultUser: "ubuntu",
			},
			want: `#cloud-config
users:
    - name: ubuntu
      ssh_authorized_keys:
        - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample ada@laptop
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := cloudinit.UserData(tt.req)
			if err != nil {
				t.Fatalf("UserData() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("UserData() =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// The rendered cloud-config has to be something cloud-init can actually parse,
// not just something that looks right. Every case that produces a cloud-config is
// re-read as YAML and checked for the key.
func TestUserDataAlwaysProducesParseableCloudConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  cloudinit.Request
	}{
		{name: "generated", req: cloudinit.Request{PublicKey: testKey, DefaultUser: "ubuntu"}},
		{name: "no default user", req: cloudinit.Request{PublicKey: testKey}},
		{
			name: "merged",
			req: cloudinit.Request{
				UserData:    "#cloud-config\nruncmd:\n  - [ systemctl, restart, ssh ]\n",
				PublicKey:   testKey,
				DefaultUser: "ubuntu",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := cloudinit.UserData(tt.req)
			if err != nil {
				t.Fatalf("UserData() error = %v", err)
			}

			body, _ := strings.CutPrefix(got, "#cloud-config\n")
			var doc struct {
				Users []struct {
					Name       string   `yaml:"name"`
					Authorized []string `yaml:"ssh_authorized_keys"`
				} `yaml:"users"`
			}
			if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatalf("the rendered user-data is not valid YAML: %v\n%s", err, got)
			}
			if len(doc.Users) != 1 {
				t.Fatalf("the rendered user-data has %d users, want 1:\n%s", len(doc.Users), got)
			}
			if len(doc.Users[0].Authorized) != 1 || doc.Users[0].Authorized[0] != testKey {
				t.Errorf("the rendered user-data grants %v, want exactly %q",
					doc.Users[0].Authorized, testKey)
			}
		})
	}
}

// TestUserDataLeavesOtherUsersAlone checks that a document Kumo did not have to
// refuse still comes back usable: the duplicate is a user Kumo is not adding a key
// to, so it is left exactly as written.
func TestUserDataLeavesOtherUsersAlone(t *testing.T) {
	t.Parallel()

	userData := "#cloud-config\nusers:\n  - name: deploy\n    shell: /bin/zsh\n" +
		"  - name: deploy\n    shell: /bin/fish\n" +
		"  - name: ubuntu\n    shell: /bin/sh\n"

	got, err := cloudinit.UserData(cloudinit.Request{
		UserData:    userData,
		PublicKey:   testKey,
		DefaultUser: "ubuntu",
	})
	if err != nil {
		t.Fatalf("UserData() = %v", err)
	}
	if !strings.Contains(got, "shell: /bin/zsh") || !strings.Contains(got, "shell: /bin/fish") {
		t.Errorf("UserData() did not preserve the other users:\n%s", got)
	}
	if !strings.Contains(got, testKey) {
		t.Errorf("UserData() did not add the key to the default user:\n%s", got)
	}
}

func TestUserDataRefusesUserDataItCannotMerge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		userData string
		wantErr  error
		wantMsg  string
	}{
		{
			name:     "broken YAML",
			userData: "#cloud-config\nusers: [\n",
			wantErr:  cloudinit.ErrNotYAML,
		},
		{
			name:     "a list instead of a mapping",
			userData: "#cloud-config\n- one\n- two\n",
			wantErr:  cloudinit.ErrNotCloudConfig,
			wantMsg:  "the document is a list, not a mapping",
		},
		{
			name:     "an empty document",
			userData: "#cloud-config\n# nothing but a comment\n",
			wantErr:  cloudinit.ErrNotCloudConfig,
			wantMsg:  "it is empty",
		},
		{
			name:     "users is a mapping",
			userData: "#cloud-config\nusers:\n  ubuntu:\n    shell: /bin/bash\n",
			wantErr:  cloudinit.ErrNotCloudConfig,
			wantMsg:  "users is a mapping, not a list",
		},
		{
			name:     "a user entry that is a string",
			userData: "#cloud-config\nusers:\n  - ubuntu\n",
			wantErr:  cloudinit.ErrNotCloudConfig,
			wantMsg:  `the user "ubuntu" is a value, not a mapping`,
		},
		{
			name:     "ssh_authorized_keys is a string",
			userData: "#cloud-config\nusers:\n  - name: ubuntu\n    ssh_authorized_keys: ssh-ed25519 AAAA\n",
			wantErr:  cloudinit.ErrNotCloudConfig,
			wantMsg:  "ssh_authorized_keys is a value, not a list",
		},
		{
			// Which entry cloud-init keeps is its business, and it may not be
			// the one Kumo put the key in.
			name: "the user listed twice",
			userData: "#cloud-config\nusers:\n  - name: ubuntu\n    shell: /bin/sh\n" +
				"  - name: ubuntu\n    shell: /bin/false\n",
			wantErr: cloudinit.ErrDuplicateUser,
			wantMsg: `"ubuntu"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := cloudinit.UserData(cloudinit.Request{
				UserData:    tt.userData,
				PublicKey:   testKey,
				DefaultUser: "ubuntu",
			})
			if err == nil {
				t.Fatal("UserData() error = nil, want a refusal")
			}
			// The sentinel is the contract the launch service matches on; the
			// message is only there for whoever reads the task's error.
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("UserData() error = %q, want it to wrap %q", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("UserData() error = %q, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

func TestIsCloudConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		userData string
		want     bool
	}{
		{userData: "#cloud-config\n", want: true},
		{userData: "  \n#cloud-config\nusers: []\n", want: true},
		{userData: "#!/bin/sh\n", want: false},
		// cloud-init matches the prefix, not the whole line, so Kumo does too.
		{userData: "#cloud-config-and-then-some", want: true},
		{userData: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.userData, func(t *testing.T) {
			t.Parallel()

			if got := cloudinit.IsCloudConfig(tt.userData); got != tt.want {
				t.Errorf("IsCloudConfig(%q) = %t, want %t", tt.userData, got, tt.want)
			}
		})
	}
}

func TestMetaData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		instanceID string
		hostname   string
		want       string
	}{
		{
			name:       "the usual case",
			instanceID: "i-0a1b2c3d4e5f6071",
			hostname:   "web-1",
			want:       "instance-id: i-0a1b2c3d4e5f6071\nlocal-hostname: web-1\n",
		},
		{
			name:       "no hostname leaves it out rather than writing an empty one",
			instanceID: "i-0a1b2c3d4e5f6071",
			want:       "instance-id: i-0a1b2c3d4e5f6071\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := cloudinit.MetaData(tt.instanceID, tt.hostname); got != tt.want {
				t.Errorf("MetaData() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNetworkConfigAsksForDHCPOnEveryNIC(t *testing.T) {
	t.Parallel()

	got := cloudinit.NetworkConfig()

	var doc struct {
		Version   int `yaml:"version"`
		Ethernets map[string]struct {
			Match struct {
				Name string `yaml:"name"`
			} `yaml:"match"`
			DHCPv4 bool `yaml:"dhcp4"`
		} `yaml:"ethernets"`
	}
	if err := yaml.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("NetworkConfig() is not valid YAML: %v\n%s", err, got)
	}
	if doc.Version != 2 {
		t.Errorf("NetworkConfig() version = %d, want 2", doc.Version)
	}

	// One entry that matches every NIC, rather than a hard-coded eth0: the NIC a
	// guest gets is named by the distro, not by Kumo.
	if len(doc.Ethernets) != 1 {
		t.Fatalf("NetworkConfig() has %d ethernet entries, want 1:\n%s", len(doc.Ethernets), got)
	}
	for name, entry := range doc.Ethernets {
		if !strings.Contains(entry.Match.Name, "en*") {
			t.Errorf("NetworkConfig() entry %q matches %q, want a name pattern", name, entry.Match.Name)
		}
		if !entry.DHCPv4 {
			t.Errorf("NetworkConfig() entry %q does not ask for DHCP", name)
		}
	}
}
