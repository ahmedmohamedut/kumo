package dbcloudinit_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ahmedmohamedut/kumo/internal/service/dbcloudinit"
)

// The verifier from packaging/images/postgres-18/verify.cloud-config.yaml, the
// one M7.1 proved a real Postgres 18.6 accepts. Using it here keeps the
// renderer's tests and the image's fixture on the same string.
const testVerifier = "SCRAM-SHA-256$4096:a3Vtb3ZlcmlmeXNhbHQxNg==" +
	"$hiDQWkAbO/n9B7N0E+t3MMu7yW2/iznKlWrHTlhLak4=" +
	":6AV9UPNJrRLab9h/afybuQYged1b/uxraiwbnMww2sY="

func testRequest() dbcloudinit.Request {
	return dbcloudinit.Request{
		Identifier:    "orders",
		AdminUsername: "app",
		DatabaseName:  "orders_db",
		Verifier:      testVerifier,
		Port:          5432,
		VPCCIDR:       "10.0.1.0/24",
		MemoryMiB:     4096,
		EngineVersion: "18",
	}
}

// render returns the parsed document and the first-boot script inside it.
func render(t *testing.T, req dbcloudinit.Request) (map[string]any, string) {
	t.Helper()

	out, err := dbcloudinit.UserData(req)
	if err != nil {
		t.Fatalf("UserData(): %v", err)
	}
	header, body, ok := strings.Cut(out, "\n")
	if !ok || header != "#cloud-config" {
		t.Fatalf("user data does not start with the cloud-config header:\n%s", out)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("the rendered user data is not valid YAML: %v\n%s", err, body)
	}

	files, ok := doc["write_files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("write_files = %#v, want one entry", doc["write_files"])
	}
	file, ok := files[0].(map[string]any)
	if !ok {
		t.Fatalf("write_files[0] = %#v, want a mapping", files[0])
	}
	script, ok := file["content"].(string)
	if !ok {
		t.Fatalf("write_files[0].content = %#v, want a string", file["content"])
	}
	return doc, script
}

func TestUserDataIsAValidCloudConfig(t *testing.T) {
	t.Parallel()

	doc, script := render(t, testRequest())

	if doc["ssh_pwauth"] != false {
		t.Errorf("ssh_pwauth = %#v, want false: a DB instance has no interactive login", doc["ssh_pwauth"])
	}
	if doc["hostname"] != "orders" {
		t.Errorf("hostname = %#v, want the identifier", doc["hostname"])
	}
	// No key pair, and no users block that could add one.
	if _, ok := doc["users"]; ok {
		t.Errorf("the document has a users block: %#v", doc["users"])
	}
	if script == "" {
		t.Fatal("the first-boot script is empty")
	}

	// /run is noexec on Ubuntu, so the script has to be handed to an
	// interpreter rather than executed. M7.1 learned this the hard way.
	runcmd, ok := doc["runcmd"].([]any)
	if !ok || len(runcmd) != 1 {
		t.Fatalf("runcmd = %#v, want one command", doc["runcmd"])
	}
	first, ok := runcmd[0].([]any)
	if !ok || len(first) != 2 || first[0] != "bash" {
		t.Errorf("runcmd[0] = %#v, want [bash, <path>]", runcmd[0])
	}
}

// A rendered script that does not parse is a guest that boots and does
// nothing, which is the hardest failure to see from outside.
func TestRenderedScriptIsValidBash(t *testing.T) {
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available to check the rendered script")
	}
	_, script := render(t, testRequest())

	path := filepath.Join(t.TempDir(), "firstboot.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write the script: %v", err)
	}
	out, err := exec.Command(bash, "-n", path).CombinedOutput()
	if err != nil {
		t.Errorf("bash -n failed: %v\n%s\n%s", err, out, script)
	}
}

func TestScriptInterpolatesTheRequest(t *testing.T) {
	t.Parallel()

	req := testRequest()
	req.Port = 6543
	req.MemoryMiB = 2048
	_, script := render(t, req)

	for _, want := range []string{
		"ADMIN='app'",
		"DATABASE='orders_db'",
		"PORT=6543",
		"VPC_CIDR='10.0.1.0/24'",
		// 25% and 60% of 2048 MiB.
		"SHARED_BUFFERS=512MB",
		"EFFECTIVE_CACHE_SIZE=1228MB",
		"VERIFIER='" + testVerifier + "'",
		"CONFDIR=/etc/postgresql/18/main",
		"DATADIR=/var/lib/postgresql/18/main",
		// By id, never by device name: the seed ISO holds vdb.
		"DEV=/dev/disk/by-id/virtio-kumodata",
		"LABEL=pgdata",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q", want)
		}
	}
}

// The engine settings the image deliberately does not carry are derived from
// the instance type's memory at first boot.
func TestMemorySettingsScaleWithTheInstanceType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		memoryMiB int
		shared    string
		cache     string
	}{
		{1024, "SHARED_BUFFERS=256MB", "EFFECTIVE_CACHE_SIZE=614MB"},
		{2048, "SHARED_BUFFERS=512MB", "EFFECTIVE_CACHE_SIZE=1228MB"},
		{8192, "SHARED_BUFFERS=2048MB", "EFFECTIVE_CACHE_SIZE=4915MB"},
		{32768, "SHARED_BUFFERS=8192MB", "EFFECTIVE_CACHE_SIZE=19660MB"},
	}
	for _, tc := range tests {
		req := testRequest()
		req.MemoryMiB = tc.memoryMiB
		_, script := render(t, req)
		if !strings.Contains(script, tc.shared) {
			t.Errorf("%d MiB: script is missing %q", tc.memoryMiB, tc.shared)
		}
		if !strings.Contains(script, tc.cache) {
			t.Errorf("%d MiB: script is missing %q", tc.memoryMiB, tc.cache)
		}
	}
}

// pg_hba.conf is the whole network boundary (R6): there is no security group
// behind it, so exactly one host line may exist and it must be the VPC.
func TestOnlyTheVPCMayConnect(t *testing.T) {
	t.Parallel()

	_, script := render(t, testRequest())

	hba := heredocBody(t, script, "HBA")
	var hostLines []string
	for _, line := range strings.Split(hba, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "host") {
			hostLines = append(hostLines, strings.TrimSpace(line))
		}
	}
	if len(hostLines) != 1 {
		t.Fatalf("pg_hba host lines = %v, want exactly one", hostLines)
	}
	if !strings.Contains(hostLines[0], "$VPC_CIDR") {
		t.Errorf("host line = %q, want it scoped to the VPC CIDR", hostLines[0])
	}
	if !strings.Contains(hostLines[0], "scram-sha-256") {
		t.Errorf("host line = %q, want scram-sha-256", hostLines[0])
	}
	for _, forbidden := range []string{"0.0.0.0/0", "trust", "md5", "password"} {
		if strings.Contains(hba, forbidden) {
			t.Errorf("pg_hba contains %q:\n%s", forbidden, hba)
		}
	}
}

// No TLS in this version (R5), and it is off explicitly rather than by
// default, so a changed default cannot quietly turn it on half-configured.
func TestTLSIsOffExplicitly(t *testing.T) {
	t.Parallel()

	_, script := render(t, testRequest())
	conf := heredocBody(t, script, "CONF")

	if !strings.Contains(conf, "ssl = off") {
		t.Errorf("postgresql conf is missing ssl = off:\n%s", conf)
	}
	if !strings.Contains(conf, "password_encryption = 'scram-sha-256'") {
		t.Errorf("postgresql conf is missing password_encryption:\n%s", conf)
	}
}

// Step 7 last is what makes the readiness probe of section 6 truthful: until
// the restart nothing outside the guest can connect, so an open port means
// every step before it finished.
func TestTheEngineIsOpenedToTheVPCLast(t *testing.T) {
	t.Parallel()

	_, script := render(t, testRequest())

	conf := heredocBody(t, script, "CONF")
	if !strings.Contains(conf, "listen_addresses = 'localhost'") {
		t.Errorf("the cluster does not start on localhost:\n%s", conf)
	}

	open := strings.Index(script, `listen_addresses = '*'`)
	if open < 0 {
		t.Fatal("the script never opens listen_addresses to the VPC")
	}
	for _, earlier := range []string{"CREATE ROLE", "CREATE DATABASE", "pg_createcluster"} {
		at := strings.Index(script, earlier)
		if at < 0 {
			t.Fatalf("the script never runs %q", earlier)
		}
		if at > open {
			t.Errorf("%q happens after the engine is opened to the VPC", earlier)
		}
	}
}

// A requeued task re-runs this script against a disk that already holds a
// cluster. Every step that could destroy data is guarded.
func TestTheScriptIsIdempotent(t *testing.T) {
	t.Parallel()

	_, script := render(t, testRequest())

	for _, guard := range []string{
		// Only format a disk with no filesystem.
		`if ! blkid -p -s TYPE -o value "$DEV" >/dev/null 2>&1; then`,
		// Only create a cluster if there is none.
		`if ! pg_lsclusters -h "$CLUSTER" main 2>/dev/null | grep -q .; then`,
		// Only add the fstab entry once.
		`grep -q "LABEL=$LABEL" /etc/fstab ||`,
		// Only mount if it is not mounted.
		`mountpoint -q /var/lib/postgresql || mount /var/lib/postgresql`,
	} {
		if !strings.Contains(script, guard) {
			t.Errorf("script is missing the guard %q", guard)
		}
	}
	if strings.Contains(script, "mkfs") && !strings.Contains(script, "blkid") {
		t.Error("the script formats without checking for a filesystem first")
	}
}

// The seed is a file in the instances pool, so first boot removes cloud-init's
// copies of it -- including obj.pkl, which holds the user-data too.
func TestTheScriptShredsTheSeedCopies(t *testing.T) {
	t.Parallel()

	_, script := render(t, testRequest())
	for _, want := range []string{"shred", "obj.pkl", "user-data*", "/var/lib/cloud"} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q in the shred step", want)
		}
	}
}

// The renderer never receives the password, so it cannot leak one. This holds
// the property: a password-shaped string handed to no field must not appear.
func TestTheRenderedSeedCannotCarryAPassword(t *testing.T) {
	t.Parallel()

	const password = "correct horse battery staple"
	out, err := dbcloudinit.UserData(testRequest())
	if err != nil {
		t.Fatalf("UserData(): %v", err)
	}
	if strings.Contains(out, password) {
		t.Error("the rendered seed contains the password")
	}
	// The verifier is in the seed on purpose; it is a password hash, not the
	// password, and section 5 accounts for it.
	if !strings.Contains(out, testVerifier) {
		t.Error("the rendered seed does not carry the verifier")
	}
}

// Bash expands a variable once and does not rescan the result, which is what
// lets a verifier full of $ go through an unquoted heredoc into SQL. If that
// ever stopped holding, every DB instance would reject its own password and
// nothing would look broken until a client tried to connect.
func TestTheVerifierSurvivesTheHeredoc(t *testing.T) {
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	_, script := render(t, testRequest())

	assignment := regexp.MustCompile(`(?m)^VERIFIER='[^']*'$`).FindString(script)
	if assignment == "" {
		t.Fatal("no VERIFIER assignment in the script")
	}
	sql := regexp.MustCompile(`(?s)runuser -u postgres -- psql [^\n]*<<SQL\n(.*?)\nSQL`).
		FindStringSubmatch(script)
	if len(sql) != 2 {
		t.Fatal("no SQL heredoc in the script")
	}

	probe := assignment + "\nADMIN=app\nDATABASE=db\ncat <<SQL\n" + sql[1] + "\nSQL\n"
	path := filepath.Join(t.TempDir(), "probe.sh")
	if err := os.WriteFile(path, []byte(probe), 0o600); err != nil {
		t.Fatalf("write the probe: %v", err)
	}
	out, err := exec.Command(bash, path).CombinedOutput()
	if err != nil {
		t.Fatalf("the probe failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), testVerifier) {
		t.Errorf("the verifier did not survive the heredoc:\n%s", out)
	}
}

func TestValidationRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*dbcloudinit.Request)
	}{
		{"no identifier", func(r *dbcloudinit.Request) { r.Identifier = "" }},
		{"identifier with an underscore", func(r *dbcloudinit.Request) { r.Identifier = "my_db" }},
		{"identifier starting with a digit", func(r *dbcloudinit.Request) { r.Identifier = "1db" }},
		{"identifier with a trailing dash", func(r *dbcloudinit.Request) { r.Identifier = "orders-" }},
		{"identifier with a doubled dash", func(r *dbcloudinit.Request) { r.Identifier = "or--ders" }},
		{"identifier too long", func(r *dbcloudinit.Request) { r.Identifier = strings.Repeat("a", 61) }},
		{"identifier with a quote", func(r *dbcloudinit.Request) { r.Identifier = "orders'" }},
		{"no admin username", func(r *dbcloudinit.Request) { r.AdminUsername = "" }},
		{"admin username with a dash", func(r *dbcloudinit.Request) { r.AdminUsername = "my-app" }},
		{"admin username reserved", func(r *dbcloudinit.Request) { r.AdminUsername = "postgres" }},
		{"admin username pg_ prefixed", func(r *dbcloudinit.Request) { r.AdminUsername = "pg_stat" }},
		{"admin username with a quote", func(r *dbcloudinit.Request) { r.AdminUsername = "app';--" }},
		{"database reserved", func(r *dbcloudinit.Request) { r.DatabaseName = "template1" }},
		{"database with a semicolon", func(r *dbcloudinit.Request) { r.DatabaseName = "db;drop" }},
		{"no verifier", func(r *dbcloudinit.Request) { r.Verifier = "" }},
		{"a cleartext password where the verifier goes", func(r *dbcloudinit.Request) {
			r.Verifier = "correct horse battery staple"
		}},
		{"verifier with a quote", func(r *dbcloudinit.Request) { r.Verifier = testVerifier + "'" }},
		{"verifier with the wrong mechanism", func(r *dbcloudinit.Request) {
			r.Verifier = strings.Replace(testVerifier, "SCRAM-SHA-256", "md5", 1)
		}},
		{"port too low", func(r *dbcloudinit.Request) { r.Port = 80 }},
		{"port too high", func(r *dbcloudinit.Request) { r.Port = 70000 }},
		{"no cidr", func(r *dbcloudinit.Request) { r.VPCCIDR = "" }},
		{"cidr without a prefix", func(r *dbcloudinit.Request) { r.VPCCIDR = "10.0.1.5" }},
		{"an IPv6 cidr", func(r *dbcloudinit.Request) { r.VPCCIDR = "fd00::/64" }},
		{"no memory", func(r *dbcloudinit.Request) { r.MemoryMiB = 0 }},
		{"negative memory", func(r *dbcloudinit.Request) { r.MemoryMiB = -1 }},
		{"no engine version", func(r *dbcloudinit.Request) { r.EngineVersion = "" }},
		{"engine version with a path", func(r *dbcloudinit.Request) { r.EngineVersion = "18/../19" }},
		{"engine version not a major", func(r *dbcloudinit.Request) { r.EngineVersion = "18.6" }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := testRequest()
			tc.mutate(&req)

			if err := dbcloudinit.Validate(req); !errors.Is(err, dbcloudinit.ErrInvalidRequest) {
				t.Errorf("Validate() = %v, want ErrInvalidRequest", err)
			}
			if _, err := dbcloudinit.UserData(req); !errors.Is(err, dbcloudinit.ErrInvalidRequest) {
				t.Errorf("UserData() = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

// A refusal must not echo the verifier: it is a password hash, and an error
// message is the one place it could leak into a log.
func TestARefusalDoesNotEchoTheVerifier(t *testing.T) {
	t.Parallel()

	req := testRequest()
	req.Verifier = testVerifier + "'"

	err := dbcloudinit.Validate(req)
	if err == nil {
		t.Fatal("Validate() accepted a verifier with a quote")
	}
	if strings.Contains(err.Error(), "SCRAM-SHA-256$") {
		t.Errorf("the refusal echoes the verifier: %v", err)
	}
}

// heredocBody returns the body of the named heredoc in the script.
func heredocBody(t *testing.T, script, tag string) string {
	t.Helper()

	re := regexp.MustCompile(`(?s)<<` + tag + `\n(.*?)\n` + tag + `\n`)
	m := re.FindStringSubmatch(script)
	if len(m) != 2 {
		t.Fatalf("no <<%s heredoc in the script:\n%s", tag, script)
	}
	return m[1]
}
