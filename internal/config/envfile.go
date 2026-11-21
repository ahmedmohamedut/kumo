package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// DefaultEnvFile is the configuration file the packages install and the
// systemd unit loads with EnvironmentFile=.
const DefaultEnvFile = "/etc/kumo/kumod.env"

// EnvFileVar names the file the operator commands read. Set to the empty
// string, no file is read.
const EnvFileVar = "KUMO_ENV_FILE"

// EnvFilePath is the file the operator commands should read: KUMO_ENV_FILE
// when it is set (empty included, which means none), DefaultEnvFile otherwise.
func EnvFilePath() string {
	if path, ok := os.LookupEnv(EnvFileVar); ok {
		return path
	}
	return DefaultEnvFile
}

// ApplyEnvFile copies the KUMO_* assignments of a systemd EnvironmentFile into
// the process environment, so that `kumod doctor` and `kumod admin`, run by
// hand, see the configuration the service runs with. A variable that is
// already set wins over the file, the way an explicit setting should. A
// missing file, or an empty path, is not an error: read reports whether a file
// was read at all.
//
// The syntax is the subset systemd and the shipped kumod.env use: one
// KEY=VALUE per line, blank lines and lines starting with # or ; ignored, and
// a value optionally wrapped in one pair of matching quotes. Lines that are not
// KUMO_* assignments are skipped.
func ApplyEnvFile(path string) (read bool, err error) {
	if path == "" {
		return false, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := parseEnvLine(scanner.Text())
		if !ok {
			continue
		}
		if _, set := os.LookupEnv(key); set {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return true, fmt.Errorf("read %s: set %s: %w", path, key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return true, fmt.Errorf("read %s: %w", path, err)
	}
	return true, nil
}

// parseEnvLine returns the KUMO_* assignment on one line of an env file.
func parseEnvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' || line[0] == ';' {
		return "", "", false
	}
	key, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, "KUMO_") || key == EnvFileVar {
		return "", "", false
	}
	value = strings.TrimSpace(value)
	if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
		value = value[1 : n-1]
	}
	return key, value, true
}
