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
sudo -u kumo kumod doctor
```

Run it as the `kumo` user, not as root: it creates the database, the signing
key and the pool directories, and files root creates there are files the service
cannot write. (Symptom: `attempt to write a readonly database` on every start;
fix with `sudo chown -R kumo:kumo /var/lib/kumo`.) The same goes for
`kumod admin` below.

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
sudo -u kumo kumod admin create
sudo -u kumo kumod admin reset-password
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

If the default range collides with a route this host already has, Kumo starts
anyway **without** a default VPC and logs a warning; `kumod doctor` and the
dashboard say so too. Either create a VPC in the console on a free range (the
first one you create becomes the default) or set `KUMO_DEFAULT_VPC_CIDR` before
the first start. The default VPC is created once, on the first boot, and
changing the range afterwards means deleting it.

## Object storage (optional)

Kumo can manage an S3-compatible object store: buckets, an object browser and
access keys for applications, all in the console. The data lives in
[RustFS](https://rustfs.com), which **you install and run**; Kumo is the
control plane in front of it. Object storage is off by default, and nothing
above this section needs it. The design is in
[`06-object-storage.md`](design/06-object-storage.md).

### What RustFS needs to be

| Requirement | Why |
|---|---|
| RustFS **1.0.4** or later (1.0.4 is what Kumo is tested against) | Kumo uses its S3 API and its MinIO-compatible admin API (`/minio/admin/v3`), which ship enabled |
| The S3 port (default `:9000`) reachable **from kumod** | bucket and key management, and the object listings |
| The same port reachable **from the browsers** that open the console | uploads and downloads go from the browser straight to RustFS through short-lived signed links, never through kumod |
| RustFS's **admin (root) access key and secret** | creating buckets, users and policies needs them; Kumo keeps them in a file only it can read |
| Clocks in sync on the RustFS host, the Kumo host and the users' machines (NTP) | every request is signed with SigV4, which RustFS refuses when the clocks are more than about 15 minutes apart |
| A RustFS that is yours to manage | Kumo lists and can delete **every** bucket on it, including ones made with other tools |

The RustFS console (`:9001` by default) is not used by Kumo and can stay off.

### Install RustFS

Follow the [RustFS documentation](https://rustfs.com/docs/) for your
distribution. A single-node install has the shape Kumo was tested on:

- a `rustfs` system user, the binary at `/usr/bin/rustfs`, and a
  `rustfs.service` systemd unit with `EnvironmentFile=/etc/default/rustfs`;
- `/etc/default/rustfs` with at least:

  ```bash
  RUSTFS_ACCESS_KEY="<admin access key>"
  RUSTFS_SECRET_KEY="<a long random secret>"
  RUSTFS_VOLUMES="/data"          # where objects are stored
  RUSTFS_ADDRESS=":9000"          # the S3 API
  RUSTFS_CONSOLE_ENABLE=false     # Kumo does not need the RustFS console
  ```

Pick a real secret (`openssl rand -base64 30`), not a default. Then **make
`/etc/default/rustfs` readable by root only**: it holds the keys that own every
bucket, and systemd reads it as root before starting the service.

```bash
sudo chmod 600 /etc/default/rustfs
sudo systemctl enable --now rustfs
curl -s http://127.0.0.1:9000/health     # {"status":"ok","version":"1.0.4",...}
```

### Point Kumo at it

1. Put the same admin keys in Kumo's credentials file, owned by `kumo` and
   readable by nobody else. kumod refuses to start with a file that group or
   others can read.

   ```bash
   sudo install -o kumo -g kumo -m 600 /dev/null /var/lib/kumo/s3.credentials
   sudoedit /var/lib/kumo/s3.credentials
   ```

   ```bash
   access_key=<admin access key>
   secret_key=<admin secret>
   ```

   The `RUSTFS_ACCESS_KEY=`/`RUSTFS_SECRET_KEY=` names are accepted too, so a
   copy of `/etc/default/rustfs` works as it is (the other lines are ignored).

2. Turn the module on in `/etc/kumo/kumod.env`:

   ```bash
   KUMO_OBJECTSTORE=rustfs
   KUMO_S3_ENDPOINT=http://127.0.0.1:9000
   # Every address you open the console at, scheme and port included:
   KUMO_S3_CORS_ORIGINS=http://kumo.example.lan:8080,http://127.0.0.1:8080
   # Only when browsers reach RustFS at another address than kumod does:
   # KUMO_S3_PUBLIC_URL=http://kumo.example.lan:9000
   ```

3. Check, then restart:

   ```bash
   sudo -u kumo kumod doctor      # expect: ok    object store  http://127.0.0.1:9000 answers ...
   sudo systemctl restart kumod
   ```

**Storage → Buckets** and **Storage → Access keys** then appear in the
console's side navigation.

| Variable | Default | Set it to |
|---|---|---|
| `KUMO_OBJECTSTORE` | `none` | `rustfs` to turn object storage on |
| `KUMO_S3_ENDPOINT` | *(none)* | where kumod reaches RustFS, usually `http://127.0.0.1:9000` |
| `KUMO_S3_PUBLIC_URL` | the endpoint | where browsers reach RustFS, when that differs (a LAN name instead of loopback) |
| `KUMO_S3_CORS_ORIGINS` | *(empty)* | the console's origins; without them browsers refuse uploads |
| `KUMO_S3_CREDENTIALS_FILE` | `/var/lib/kumo/s3.credentials` | another path, if you keep secrets elsewhere |
| `KUMO_S3_REGION` | `us-east-1` | rarely: RustFS accepts any region |
| `KUMO_S3_PRESIGN_TTL` | `15m` | how long an upload or download link stays valid (at most `168h`) |

### How applications connect

Create an access key in the console (**Storage → Access keys**), choose its
buckets and whether it may write. The secret is shown **once**: copy the
`AWS_*` lines the dialog offers into the application's environment. Any S3
client works with path-style addressing:

```bash
aws --endpoint-url http://kumo.example.lan:9000 s3 ls s3://my-bucket/
```

A key never creates, deletes or reconfigures a bucket; that stays in Kumo.

### When object storage complains

**`FAIL  object store ... unusable credentials file ... chmod 600`.** The file
is readable by group or others. `sudo chmod 600 /var/lib/kumo/s3.credentials`.

**`FAIL  object store ... must set access_key and secret_key`.** The two lines
are missing or misnamed; see the format above.

**`FAIL  object store ... connection refused` or `InvalidAccessKeyId`.** RustFS
is not listening at `KUMO_S3_ENDPOINT`, or the keys differ from
`/etc/default/rustfs`. `systemctl status rustfs`, then compare the keys.

**The Storage pages say "not configured".** `KUMO_OBJECTSTORE` is unset or
kumod was not restarted after setting it. `curl -s localhost:8080/api/v1/system/info`
shows `"object_storage":{"backend":"rustfs","ok":true}` when it is on.

**Uploads fail in the browser with "could not be reached ... CORS".** The
console's address, with its port, is missing from `KUMO_S3_CORS_ORIGINS`
(`http://127.0.0.1` does not cover `http://127.0.0.1:8080`), or the browser
cannot reach `KUMO_S3_PUBLIC_URL`. Fix it and restart kumod; existing buckets
get the new rule on their next upload.

**Uploads fail with `403`.** The clocks are out of sync, or the signed link
expired before the upload started (`KUMO_S3_PRESIGN_TTL`).

### What Kumo does not do to RustFS

Removing the Kumo package deletes `/var/lib/kumo`, including the credentials
file, but **leaves RustFS and its data alone**: buckets, objects and the
access keys Kumo issued stay on RustFS. Delete the keys in the console first if
they should stop working.

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
- **Object storage over HTTP.** With object storage on, signed upload and
  download links, and the one-time display of an access key's secret, cross the
  network unencrypted too; the console's Objects tab says so. Access key
  secrets are never stored, and the RustFS admin keys stay in a file only the
  `kumo` user can read.

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

**No default VPC.** The default range overlaps a host route, and Kumo will not
create a NAT network that would shadow a network you already use. The service
runs; the dashboard shows a warning. Create a VPC on a free RFC 1918 range
(Console → VPCs → Create): the first one becomes the default. Or set
`KUMO_DEFAULT_VPC_CIDR` and restart on a fresh database.

**An image import fails.** The reason is on the image's page. A `403` or `404`
is usually the vendor having moved the file; paste a current URL into the import
form instead of using the catalog entry.

## Upgrading

```bash
sudo apt install ./kumo_<new-version>_amd64.deb
sudo dnf upgrade ./kumo-<new-version>-1.x86_64.rpm
```

Your `/etc/kumo/kumod.env` is kept and the new default arrives beside it as
`.dpkg-dist` or `.rpmnew`. Database migrations run on start. Object storage
arrived in v0.2.0 and stays off after an upgrade until you set
`KUMO_OBJECTSTORE`; see [Object storage](#object-storage-optional). Running instances
are untouched: they are libvirt domains, and a kumod restart re-reads their
state rather than recreating them.

## Uninstalling

```bash
sudo systemctl disable --now kumod
sudo apt remove kumo     # or: sudo dnf remove kumo
```

Removing the package removes **everything** Kumo made, and it does not ask:

- every instance (libvirt domains named `i-…`), stopped and undefined
- the `kumo-images` and `kumo-instances` pools, every VPC's libvirt network and its
  `kmb…` bridge (and the `kumo-default` network, if you adopted one)
- `/var/lib/kumo` (database, signing key, images, root disks) and `/etc/kumo`
- the SELinux/AppArmor rules Kumo added, and the `kumo` user and group

It does **not** touch RustFS: buckets, objects and issued access keys stay
there (see [Object storage](#object-storage-optional)).

An upgrade does none of this. To keep your data, don't remove the package; back
up `/var/lib/kumo` first.

## Running from source

For development, not for a node you care about:

```bash
make deps
make dev     # API on :8080 with reload, Vite on :5173
```

No KVM on your machine? `KUMO_HYPERVISOR=fake make dev` runs the whole control
plane against an in-memory hypervisor. Everything above libvirt behaves
normally; the guests are simulated.
