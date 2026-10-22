# Installing Kumo

Kumo is one binary, one SQLite file and a systemd unit. It talks to the libvirtd
that is already on your host, so there is nothing else to run and nothing to
cluster.

This is the 15-minute path: install, check the host, start, create the
administrator, launch something.

> **This release serves plain HTTP.** TLS is still an open item, so there is no
> `https://` and no port 8443 yet. Put Kumo on a network you control, or behind
> a reverse proxy that terminates TLS. [What that means](#security-of-this-release)
> is spelled out below — please read it before exposing the port.

## 1. Check the host

| Needs | Why |
|---|---|
| Linux on x86-64 or arm64 | Kumo ships Linux packages only |
| Hardware virtualisation | guests run on KVM, not emulation |
| `libvirtd` and QEMU/KVM | Kumo drives libvirt; it does not replace it |
| ~20 GiB free under `/var/lib` | base images and root disks live there |

Hardware virtualisation, in one command:

```bash
grep -cE '(vmx|svm)' /proc/cpuinfo    # 0 means no VT-x / AMD-V
ls -l /dev/kvm                        # missing means the kvm module is not loaded
```

If both come back empty you are probably inside a VM without nested
virtualisation. Kumo will install and its console will work, but no instance can
boot. There is nothing Kumo can do about that from userspace.

Install the hypervisor if it is not there yet:

```bash
# Debian / Ubuntu
sudo apt install qemu-system-x86 libvirt-daemon-system
# Fedora / RHEL / Rocky / Alma
sudo dnf install @virtualization

sudo systemctl enable --now libvirtd
```

## 2. Install Kumo

### The script

```bash
curl -fsSL https://raw.githubusercontent.com/ahmedmohamedut/kumo/main/scripts/install.sh | sudo bash
```

It picks the right `.deb` or `.rpm` for this host from the latest GitHub
release, verifies its checksum, and installs it with the system package manager.
Read it first if you would rather not pipe a script into a root shell — it is
short, and `--dry-run` prints what it would do without touching anything.

### Or the package, by hand

Download from [releases](https://github.com/ahmedmohamedut/kumo/releases) and:

```bash
sudo apt install ./kumo_<version>_amd64.deb         # Debian / Ubuntu
sudo dnf install ./kumo-<version>-1.x86_64.rpm      # Fedora / RHEL
```

Either way you get:

| Path | What |
|---|---|
| `/usr/bin/kumod` | the binary, with the web console embedded |
| `/etc/kumo/kumod.env` | configuration; your edits survive upgrades |
| `/usr/lib/systemd/system/kumod.service` | the unit |
| `/var/lib/kumo` | database, signing key, images, root disks |
| `/usr/libexec/kumo/security-config.sh` | the AppArmor/SELinux rules |

The package creates a `kumo` system user, puts it in the `libvirt` group so it
can reach the socket, and applies the AppArmor or SELinux rules that let a
confined QEMU read and write the storage pools. It does **not** start the
service — that is the next step, and it is yours.

## 3. Check, then start

```bash
sudo kumod doctor
```

`doctor` is the five minutes you save later. It checks `/dev/kvm`, that the
`kumo` user can reach the libvirt socket, that QEMU is installed, that the pools
and the network exist, and that there is disk space left. Fix what it reports
before starting — see [when doctor complains](#when-doctor-complains).

```bash
sudo systemctl enable --now kumod
systemctl status kumod
```

## 4. Create the administrator

Open `http://<this-host>:8080`. A fresh node has no users, so the console opens
its first-run screen and asks for one. The password must be at least 12
characters.

Prefer the command line, or locked out later:

```bash
sudo kumod admin create
sudo kumod admin reset-password
```

Neither reads a password from an argument — arguments are visible to every
process on the host.

## 5. Launch an instance

In the console: **Images → Import image**, pick one from the catalog (Ubuntu,
Debian, Rocky, Alma, Fedora or Alpine) and wait for it to reach *Available* —
it is a few hundred megabytes, downloaded once and reused by every instance.

Then **Key pairs → Create key pair**, and save the private key when it is shown.
It is shown exactly once and never stored on the node.

Then **Instances → Launch instance**: name it, pick the image, a size and the
key pair. The dashboard shows it arrive.

```bash
ssh -i <saved-key>.pem <default-user>@<private-ip>
```

The default user comes from the image (`ubuntu`, `debian`, `fedora`, `alpine`)
and the console shows it on the image's page.

## Configuration

Everything is a `KUMO_*` environment variable in `/etc/kumo/kumod.env`, and
every variable is documented in
[`00-architecture.md`](design/00-architecture.md) section 5. The ones worth
knowing on day one:

| Variable | Default | Change it when |
|---|---|---|
| `KUMO_LISTEN` | `:8080` | you want it on one interface, or another port |
| `KUMO_DEFAULT_VPC_CIDR` | `192.168.100.0/24` | your LAN already uses that range |
| `KUMO_MEMORY_RESERVE_MIB` | `1024` | the host needs more headroom for itself |
| `KUMO_VCPU_OVERCOMMIT` | `4` | you do not want vCPUs oversubscribed |

`systemctl restart kumod` after editing.

**Set `KUMO_DEFAULT_VPC_CIDR` before the first start** if it collides with a
route this host already has. The default VPC is created once, on the first boot,
and changing the range afterwards means deleting it.

## Security of this release

Worth being plain about, because it decides where you put this node.

- **No TLS.** The console and the API are plain HTTP. Anyone who can see the
  traffic can read an access token out of it.
- **The refresh cookie is not `Secure`.** The shipped `kumod.env` sets
  `KUMO_COOKIE_SECURE=false`, because a browser will not store a `Secure` cookie
  over HTTP — left true, every operator would be signed out on every reload. It
  is a deliberate trade for a working HTTP install, not a default to keep once
  TLS is in front of it.
- **Passwords are argon2id**, access tokens are 15-minute JWTs, and refresh
  tokens are stored hashed and rotated on use. Reusing one revokes the whole
  family.
- **Private keys are never stored.** A generated key pair's private half is
  returned once, in the create response, and is not written to the database.
- **Console access is host-local.** VNC and serial consoles bind to `127.0.0.1`.

Until TLS lands: bind to a trusted interface, or terminate HTTPS in a reverse
proxy and set `KUMO_COOKIE_SECURE=true` behind it.

## When doctor complains

**`/dev/kvm` missing or not accessible.** The module is not loaded (`sudo
modprobe kvm_intel` or `kvm_amd`), or virtualisation is off in firmware, or this
host is itself an un-nested VM. The `kumo` user also needs the `kvm` group,
which the package arranges.

**libvirt socket unreachable.** `systemctl status libvirtd`, then confirm the
group membership the package set up:

```bash
id kumo        # expect libvirt (or libvirtd) and kvm
sudo systemctl restart kumod
```

**A launch fails on permissions, with AppArmor or SELinux denials.** Nothing
under `/var/lib` carries a label a confined QEMU may write to on a fresh
install. Re-run the rules:

```bash
sudo /usr/libexec/kumo/security-config.sh
```

**The default VPC overlaps a host route.** Kumo refuses to create a NAT network
that would shadow a network you already use. Set `KUMO_DEFAULT_VPC_CIDR` to a
free RFC 1918 range and restart.

**An image import fails.** The reason is on the image's page. A `403` or `404`
is usually the vendor having moved the file; paste a current URL into the import
form instead of using the catalog entry.

## Upgrading

```bash
sudo apt install ./kumo_<new-version>_amd64.deb
sudo dnf upgrade ./kumo-<new-version>-1.x86_64.rpm
```

Your `/etc/kumo/kumod.env` is kept and the new default arrives beside it as
`.dpkg-dist` or `.rpmnew`. Database migrations run on start. Running instances
are untouched: they are libvirt domains, and a kumod restart re-reads their
state rather than recreating them.

## Uninstalling

```bash
sudo systemctl disable --now kumod
sudo apt remove kumo     # or: sudo dnf remove kumo
```

`/var/lib/kumo` and the `kumo` user are left behind on purpose — removing the
package is not a request to destroy your guests. The instances are still defined
in libvirt and still running. To take everything with it:

```bash
virsh list --all            # the domains named i-...
virsh destroy  <domain>
virsh undefine <domain> --remove-all-storage
sudo rm -rf /var/lib/kumo
sudo userdel kumo
```

## Running from source

For development, not for a node you care about:

```bash
make deps
make dev     # API on :8080 with reload, Vite on :5173
```

No KVM on your machine? `KUMO_HYPERVISOR=fake make dev` runs the whole control
plane against an in-memory hypervisor. Everything above libvirt behaves
normally; the guests are simulated.
