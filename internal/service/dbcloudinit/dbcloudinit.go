// Package dbcloudinit renders the cloud-init user-data a DB instance boots
// from: the first-boot script that formats the data disk, creates the cluster
// on it, creates the admin role and its database, and only then opens the
// engine to the VPC (docs/design/07-databases.md section 5).
//
// Like internal/service/cloudinit it is free of libvirt, of the database and
// of the services, so every rule is a function with a test.
//
// It never receives the admin password. It takes the SCRAM-SHA-256 verifier
// internal/service/dbcred derived from it, which is what makes "the password
// is not in the seed" a property of the type signature rather than a promise.
package dbcloudinit

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
)

// cloudConfigHeader must be the first line for cloud-init to read the document
// as a cloud-config rather than as a shell script.
const cloudConfigHeader = "#cloud-config"

// scriptPath is where the first-boot script is written.
//
// /run, so a script that holds a credential never lands on a disk that
// outlives the boot. It is invoked as `bash <path>` rather than executed,
// because /run is mounted noexec on Ubuntu and a mode of 0755 does not change
// that -- a lesson from M7.1, where the same mistake stopped the image build
// silently.
const scriptPath = "/run/kumo-db-firstboot"

// DataDiskDevice is where the guest finds its data disk. By id, not by name:
// /dev/vdb is whichever disk libvirt attached second (the seed ISO holds it
// today), and a first boot that formats the wrong disk is not a failure worth
// debugging twice.
var DataDiskDevice = "/dev/disk/by-id/virtio-" + hypervisor.DataDiskSerial

// DataDiskLabel is the filesystem label the data disk is mounted by, so a
// device rename cannot detach a database from its data.
const DataDiskLabel = "pgdata"

// Memory shares of the instance type's RAM, applied at first boot because only
// the renderer knows the size: the image records the knobs, never the values
// (docs/design/07-databases.md section 7).
const (
	sharedBuffersPercent      = 25
	effectiveCacheSizePercent = 60
)

// Bounds and shapes of what may be interpolated, from
// docs/design/07-databases.md section 3.
const (
	MaxIdentifierLength = 60
	MaxRoleNameLength   = 63
	// MinPort keeps a database off the privileged range even though it runs as
	// postgres; MaxPort is the ceiling of the field.
	MinPort = 1024
	MaxPort = 65535
)

var (
	// identifierPattern is a DNS label: it becomes the guest's hostname and
	// the name dnsmasq serves, which is the database's endpoint.
	identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	// roleNamePattern is an unquoted SQL identifier, which is what lets the
	// role and database names go into the statements without quoting rules of
	// their own.
	roleNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	// enginePattern is a major version, which becomes a path segment.
	enginePattern = regexp.MustCompile(`^\d{1,3}$`)
	// verifierPattern is the shape dbcred produces. Checked here as well
	// because this is the value that goes into a SQL string literal.
	verifierPattern = regexp.MustCompile(
		`^SCRAM-SHA-256\$\d+:[A-Za-z0-9+/]+={0,2}\$[A-Za-z0-9+/]+={0,2}:[A-Za-z0-9+/]+={0,2}$`)
)

// reservedRoleNames may not be the admin role or the initial database. The
// pg_ prefix is reserved by Postgres itself; the rest are names a user would
// only pick by accident or to collide with something.
var reservedRoleNames = map[string]struct{}{
	"postgres":  {},
	"template0": {},
	"template1": {},
	"rdsadmin":  {},
	"kumo":      {},
}

// ErrInvalidRequest means a field could not be interpolated safely. It wraps
// nothing: the message names the field, and the service layer turns it into a
// field error of its own.
var ErrInvalidRequest = errors.New("dbcloudinit: the request is not renderable")

// Request is everything the first-boot script needs.
//
// There is no password field, by design. Verifier is what dbcred derived, and
// a caller that still holds the password cannot accidentally pass it here.
type Request struct {
	// Identifier is the DB instance's user-chosen name: the guest's hostname
	// and the label its endpoint resolves under.
	Identifier string
	// AdminUsername and DatabaseName are the role and database first boot
	// creates, the role owning the database.
	AdminUsername string
	DatabaseName  string
	// Verifier is the SCRAM-SHA-256 verifier from internal/service/dbcred.
	Verifier string
	// Port is the port the engine listens on.
	Port int
	// VPCCIDR is the only range pg_hba.conf admits. There is no network policy
	// behind it (R6): that one line is the whole boundary.
	VPCCIDR string
	// MemoryMiB is the instance type's memory, which the engine's buffer
	// settings are derived from.
	MemoryMiB int
	// EngineVersion is the Postgres major version, e.g. "18". It names the
	// cluster and its paths.
	EngineVersion string
}

// Validate reports whether the request can be rendered, so a caller can refuse
// a create request before anything else happens.
func Validate(req Request) error {
	switch {
	case req.Identifier == "" || len(req.Identifier) > MaxIdentifierLength,
		!identifierPattern.MatchString(req.Identifier),
		strings.HasSuffix(req.Identifier, "-"),
		strings.Contains(req.Identifier, "--"):
		return fmt.Errorf("%w: identifier %q", ErrInvalidRequest, req.Identifier)
	}

	for field, name := range map[string]string{
		"admin_username": req.AdminUsername,
		"database_name":  req.DatabaseName,
	} {
		if name == "" || len(name) > MaxRoleNameLength || !roleNamePattern.MatchString(name) {
			return fmt.Errorf("%w: %s %q", ErrInvalidRequest, field, name)
		}
		if _, reserved := reservedRoleNames[name]; reserved || strings.HasPrefix(name, "pg_") {
			return fmt.Errorf("%w: %s %q is reserved", ErrInvalidRequest, field, name)
		}
	}

	if !verifierPattern.MatchString(req.Verifier) {
		// Deliberately not echoed: a verifier is a password hash.
		return fmt.Errorf("%w: the verifier is not a SCRAM-SHA-256 verifier", ErrInvalidRequest)
	}
	if req.Port < MinPort || req.Port > MaxPort {
		return fmt.Errorf("%w: port %d", ErrInvalidRequest, req.Port)
	}
	if prefix, err := netip.ParsePrefix(req.VPCCIDR); err != nil || !prefix.Addr().Is4() {
		return fmt.Errorf("%w: vpc cidr %q", ErrInvalidRequest, req.VPCCIDR)
	}
	if req.MemoryMiB <= 0 {
		return fmt.Errorf("%w: memory %d MiB", ErrInvalidRequest, req.MemoryMiB)
	}
	if !enginePattern.MatchString(req.EngineVersion) {
		return fmt.Errorf("%w: engine version %q", ErrInvalidRequest, req.EngineVersion)
	}
	return nil
}

// document is the cloud-config this package renders. It is marshaled rather
// than templated, so the script cannot break the YAML whatever it contains.
type document struct {
	// A DB instance is not a machine anyone has a shell on (section 5): no key
	// pair is attached and password authentication stays off.
	SSHPwauth  bool        `yaml:"ssh_pwauth"`
	Hostname   string      `yaml:"hostname"`
	WriteFiles []writeFile `yaml:"write_files"`
	Runcmd     [][]string  `yaml:"runcmd"`
}

type writeFile struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
}

// UserData renders the user-data part of a DB instance's seed.
func UserData(req Request) (string, error) {
	if err := Validate(req); err != nil {
		return "", err
	}

	doc := document{
		SSHPwauth: false,
		Hostname:  req.Identifier,
		WriteFiles: []writeFile{{
			Path:        scriptPath,
			Permissions: "0700",
			Content:     script(req),
		}},
		Runcmd: [][]string{{"bash", scriptPath}},
	}

	body, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("dbcloudinit: marshal the cloud-config: %w", err)
	}
	return cloudConfigHeader + "\n" + string(body), nil
}

// script renders the first-boot script of docs/design/07-databases.md
// section 5.
//
// Every value is interpolated once, into a single-quoted shell assignment at
// the top, and used through the variable from then on. Validate has already
// refused anything that could close that quote, so there is one place to look
// when asking whether a value is safely placed.
//
// No line is indented with a tab. The heredoc bodies sit at column zero and
// use the plain form rather than <<-, because a tab-indented body would make
// the script unrepresentable as a YAML literal block and it would be emitted
// as an escaped one-liner that nobody can read inside a guest.
func script(req Request) string {
	var b strings.Builder

	fmt.Fprintf(&b, `#!/usr/bin/env bash
# First boot of a Kumo DB instance (docs/design/07-databases.md section 5).
# Rendered by internal/service/dbcloudinit; not edited by hand.
set -euo pipefail
export LC_ALL=C

DEV=%s
LABEL=%s
ADMIN='%s'
DATABASE='%s'
PORT=%d
VPC_CIDR='%s'
SHARED_BUFFERS=%dMB
EFFECTIVE_CACHE_SIZE=%dMB
VERIFIER='%s'

CONFDIR=/etc/postgresql/%s/main
DATADIR=/var/lib/postgresql/%s/main
CLUSTER=%s

`,
		DataDiskDevice, DataDiskLabel,
		req.AdminUsername, req.DatabaseName, req.Port, req.VPCCIDR,
		req.MemoryMiB*sharedBuffersPercent/100,
		req.MemoryMiB*effectiveCacheSizePercent/100,
		req.Verifier,
		req.EngineVersion, req.EngineVersion, req.EngineVersion)

	b.WriteString(`# 1. Format the data disk, but only if it carries no filesystem. A requeued
#    task re-runs this script against a disk that already holds a cluster, and
#    that must be a no-op rather than an erasure.
if ! blkid -p -s TYPE -o value "$DEV" >/dev/null 2>&1; then
  mkfs.ext4 -q -m 0 -L "$LABEL" "$DEV"
fi

# 2. Mount it at the postgres home, by label so a device rename cannot detach a
#    database from its data, and own it before anything writes there.
install -d -m 0750 /var/lib/postgresql
grep -q "LABEL=$LABEL" /etc/fstab ||
  printf 'LABEL=%s /var/lib/postgresql ext4 defaults,noatime 0 2\n' "$LABEL" >>/etc/fstab
mountpoint -q /var/lib/postgresql || mount /var/lib/postgresql
chown postgres:postgres /var/lib/postgresql
chmod 0750 /var/lib/postgresql

# 3. Create the cluster on the data disk. The image ships without one, so this
#    is where a DB instance gets a cluster identity of its own.
if ! pg_lsclusters -h "$CLUSTER" main 2>/dev/null | grep -q .; then
  pg_createcluster "$CLUSTER" main --datadir="$DATADIR" -- \
    --encoding=UTF8 --locale=C.UTF-8 \
    --auth-local=peer --auth-host=scram-sha-256
fi

# 4. Configuration as an include, so the packaging's postgresql.conf stays the
#    packaging's. listen_addresses stays on localhost until step 7.
mkdir -p "$CONFDIR/conf.d"
cat >"$CONFDIR/conf.d/10-kumo.conf" <<CONF
listen_addresses = 'localhost'
port = $PORT
password_encryption = 'scram-sha-256'
ssl = off
shared_buffers = $SHARED_BUFFERS
effective_cache_size = $EFFECTIVE_CACHE_SIZE
CONF

# 5. Only the VPC may reach it, and only with SCRAM. There is no network policy
#    behind this (R6): this file is the whole boundary.
cat >"$CONFDIR/pg_hba.conf" <<HBA
local all postgres peer
local all all peer
host all all $VPC_CIDR scram-sha-256
HBA
chown postgres:postgres "$CONFDIR/pg_hba.conf" "$CONFDIR/conf.d/10-kumo.conf"

# 6. Start on localhost and create the role and its database. The verifier goes
#    in where a password would: Postgres stores it verbatim, so the cleartext
#    never has to exist on this machine.
pg_ctlcluster "$CLUSTER" main start
# So postgresql-common's generator sees the new cluster and starts it on every
# later boot, which is what makes stop, start and reboot work.
systemctl daemon-reload
runuser -u postgres -- psql -v ON_ERROR_STOP=1 --no-psqlrc -q <<SQL
CREATE ROLE $ADMIN LOGIN CREATEDB PASSWORD '$VERIFIER';
CREATE DATABASE $DATABASE OWNER $ADMIN;
REVOKE ALL ON DATABASE $DATABASE FROM PUBLIC;
SQL

# 7. Open it to the VPC, last. Until this restart nothing outside the guest can
#    connect, so the port being open is proof that every step above finished --
#    which is what lets kumod's readiness probe be a bare TCP connect, with no
#    Postgres driver anywhere in the binary (section 6).
sed -i "s/^listen_addresses = .*/listen_addresses = '*'/" "$CONFDIR/conf.d/10-kumo.conf"
pg_ctlcluster "$CLUSTER" main restart

# 8. Remove cloud-init's copies of this seed. obj.pkl is in the list because
#    the pickled datasource holds the user-data too. The seed disk itself is
#    still attached, and is the host-side exposure section 5 accounts for.
find /var/lib/cloud -type f \
  \( -name 'user-data*' -o -name 'vendor-data*' -o -name 'obj.pkl' \) \
  -exec shred -u {} + 2>/dev/null || true
`)

	return b.String()
}
