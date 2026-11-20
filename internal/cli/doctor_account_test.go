//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountHint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	owner := os.Geteuid()

	tests := []struct {
		name     string
		dir      string
		euid     int
		wantHint bool
	}{
		{name: "the owner of the data directory", dir: dir, euid: owner},
		// root reads every file, but what doctor creates would be root's and
		// the service could not write it (install.md says so too).
		{name: "root", dir: dir, euid: 0, wantHint: true},
		// What happened on a packaged node: an operator ran plain
		// `kumod doctor` and the 0600 signing key refused them.
		{name: "another account", dir: dir, euid: owner + 4242, wantHint: true},
		// No directory yet is checkDataDir's failure to report, not a
		// question of accounts.
		{name: "a data directory that does not exist", dir: filepath.Join(dir, "absent"), euid: owner + 4242},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.euid == 0 && owner == 0 {
				t.Skip("run as root, the scratch directory is root's own")
			}

			hint := accountHint(tt.dir, tt.euid)
			if (hint != "") != tt.wantHint {
				t.Fatalf("accountHint() = %q, want a hint: %v", hint, tt.wantHint)
			}
			if !tt.wantHint {
				return
			}
			// The hint names the directory and the exact command to run as
			// its owner, which is the whole point of printing it.
			if !strings.Contains(hint, tt.dir) || !strings.Contains(hint, "sudo -u ") ||
				!strings.Contains(hint, "kumod doctor") {
				t.Errorf("accountHint() = %q, want the directory and a sudo -u ... kumod doctor command", hint)
			}
		})
	}
}
