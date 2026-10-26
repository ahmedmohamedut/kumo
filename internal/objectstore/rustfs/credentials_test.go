package rustfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCredentials(t *testing.T) {
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		want    Credentials
		wantErr bool
	}{
		{
			name:    "kumo names",
			content: "# RustFS admin keys\naccess_key=kumoadmin\nsecret_key=s3cr3t/with=equals\n",
			mode:    0o600,
			want:    Credentials{AccessKey: "kumoadmin", SecretKey: "s3cr3t/with=equals"},
		},
		{
			name:    "a copy of /etc/default/rustfs",
			content: "RUSTFS_ACCESS_KEY=\"admin\"\nRUSTFS_SECRET_KEY='pa ss'\nRUSTFS_VOLUMES=\"/data\"\n",
			mode:    0o400,
			want:    Credentials{AccessKey: "admin", SecretKey: "pa ss"},
		},
		{
			name:    "export prefix",
			content: "export access_key=a\nexport secret_key=b\n",
			mode:    0o600,
			want:    Credentials{AccessKey: "a", SecretKey: "b"},
		},
		{name: "readable by others", content: "access_key=a\nsecret_key=topsecret\n", mode: 0o644, wantErr: true},
		{name: "readable by group", content: "access_key=a\nsecret_key=topsecret\n", mode: 0o640, wantErr: true},
		{name: "no secret", content: "access_key=a\n", mode: 0o600, wantErr: true},
		{name: "not key=value", content: "access_key=a\ntopsecret\n", mode: 0o600, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s3.credentials")
			if err := os.WriteFile(path, []byte(tt.content), tt.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}

			got, err := ReadCredentials(path)
			if tt.wantErr {
				if !errors.Is(err, ErrCredentialsFile) {
					t.Fatalf("got %v, want ErrCredentialsFile", err)
				}
				if strings.Contains(err.Error(), "topsecret") {
					t.Errorf("the error quotes the secret: %v", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("got %+v, %v, want %+v", got, err, tt.want)
			}
		})
	}

	if _, err := ReadCredentials(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrCredentialsFile) {
		t.Errorf("missing file: got %v", err)
	}
}
