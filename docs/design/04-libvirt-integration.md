# 04 — libvirt Integration

Library: `github.com/digitalocean/go-libvirt` (pure Go, speaks the libvirt RPC protocol over the daemon socket; no cgo, no `libvirt-dev`).
Connection: `libvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(path)))` then `Connect()`. Verify exact constructor names against the version pinned in `go.mod`.

## 1. Hypervisor interface (`internal/hypervisor`)

```go
type Hypervisor interface {
    Ping(ctx context.Context) error
    EnsureInfra(ctx context.Context, cfg InfraConfig) error      // pools + network

    // Volumes
    ImportImage(ctx context.Context, name string, src ImportSource) error
    ImageInfo(ctx context.Context, name string) (ImageInfo, error)   // virtual size, format
    DeleteVolume(ctx context.Context, pool, name string) error
    CreateRootDisk(ctx context.Context, spec RootDiskSpec) error     // backing file + resize
    CreateSeedISO(ctx context.Context, name string, seed CloudInit) error

    // Domains
    DefineAndStart(ctx context.Context, spec DomainSpec) (DomainInfo, error)
    Start(ctx context.Context, id string) error
    Shutdown(ctx context.Context, id string) error                 // ACPI
    Destroy(ctx context.Context, id string) error                  // hard stop
    Reboot(ctx context.Context, id string) error
    Reset(ctx context.Context, id string) error
    Undefine(ctx context.Context, id string) error                 // keeps nothing
    GetDomain(ctx context.Context, id string) (DomainInfo, error)  // state, IPs, mac
    ListDomains(ctx context.Context) ([]DomainInfo, error)
    ConsoleOutput(ctx context.Context, id string, maxBytes int) ([]byte, error)
    Capacity(ctx context.Context) (Capacity, error)

    Events(ctx context.Context) (<-chan DomainEvent, error)        // lifecycle events
}
```

Implementations: `internal/hypervisor/libvirt` (real) and `internal/hypervisor/fake` (in-memory state machine with configurable latency/failures, used by all service tests and `KUMO_HYPERVISOR=fake`).

The libvirt implementation wraps the connection with **reconnect-on-error** (detect `net.ErrClosed`/EOF, re-dial with backoff, re-register events) and a read timeout per call via `ctx`.

## 2. Storage layout

Two **directory** storage pools, created by `EnsureInfra`:

| Pool | Path | Contents |
|---|---|---|
| `kumo-images` | `$KUMO_DATA_DIR/pools/images` | Base images `img-<id>.qcow2` (read-only after import) |
| `kumo-instances` | `$KUMO_DATA_DIR/pools/instances` | `i-<id>-root.qcow2`, `i-<id>-seed.iso` |

Permissions: files owned by the libvirt QEMU user (`libvirt-qemu`/`qemu`) — don't hardcode; set via pool `<permissions>` and verify in the doctor check.

### Image import flow (task `image.import`)
1. Create DB row `pending`; create task. The HTTP handler validates the URL synchronously and never downloads.
2. Download to a temp file in `$KUMO_IMAGE_TEMP_DIR` (stream, enforce `KUMO_IMAGE_MAX_BYTES`, update `progress` from `Content-Length`).
3. Verify checksum if provided (`sha256`/`sha512`).
4. Inspect format/virtual size in pure Go (`internal/imagefmt`): parse the qcow2 header (magic `QFI\xfb`, virtual size at offset 24, big-endian u64). Reject backing-file images (the chain would dangle once the pool changes); if `raw`, accept and record the rounded-up sector size.
5. Create the volume in `kumo-images`, named `img-<id>.qcow2`, via one of two paths:
   - **Directory pool (preferred):** `os.Rename` the staged file into the pool directory, `chmod` it readable by the pool's QEMU user, then `StoragePoolRefresh` so libvirt sees it. This is a metadata operation, not a copy.
   - **Fallback:** `StorageVolCreateXML` + `StorageVolUpload`, for a pool whose path is not local to kumod.
6. Update row: `available`, `size_bytes`, `virtual_size_bytes`, `volume_name`. On any failure: delete the partial volume/temp file; set `failed` + `state_reason`.

`ImportSource` carries **both** sizes, because they are different numbers:

| Field | Meaning | Used as |
|---|---|---|
| `SizeBytes` | bytes on disk (`ls -l`) | length of the upload / staged file |
| `VirtualSizeBytes` | guest capacity (qcow2 header, raw rounded up) | libvirt `<capacity>` |

Setting `capacity` to the file size truncates a sparse qcow2 to its compressed length and makes the volume unusable; setting the upload length to the virtual size hangs or fails. The same two numbers are recorded as `size_bytes` and `virtual_size_bytes`.

## 3. Instance launch flow (task `instance.launch`)

```
validate (capacity + subnet IP allocation in 1 txn) → INSERT instance(state=pending) + eni + task
  1. Allocate IP: lowest free usable IP in subnet; generate 52:54:00:… MAC
  2. CreateRootDisk: qcow2 volume in kumo-instances with
       <backingStore><path>…/img-xxx.qcow2</path><format type='qcow2'/></backingStore>
       capacity = root_disk_gb GiB   (virtual size must be ≥ image virtual size)
  3. CreateSeedISO (NoCloud): volume label "cidata", files:
       meta-data:  instance-id: <i-id>\nlocal-hostname: <name>
       user-data:  user's cloud-config, or generated default (see §4)
       (optional network-config v2: dhcp4 on all NICs)
  4. Write static DHCP host entry <host mac='…' ip='…' name='…'/> to libvirt network
  5. Build domain XML (see §5) → DomainDefineXML → DomainCreate
  6. Poll up to 120 s for state=running and domain responsiveness
  7. UPDATE instance state=running; task succeeded
on failure at step N: Undefine domain (if defined), remove static DHCP host, delete root disk + seed ISO, state=error + reason
```

Seed ISO: build in pure Go (`github.com/kdomanski/iso9660`) → write into the instances pool dir → refresh pool. Fallback: shell out to `xorriso`/`genisoimage` only if the pure-Go path proves unreliable (ask first).

Volume names are `<instance-id>-root.qcow2` and `<instance-id>-seed.iso`, built by
`hypervisor.RootDiskName`/`hypervisor.SeedISOName` so the service that launches, the libvirt backend
that creates the volumes and the fake that holds them cannot disagree on a name a rollback deletes.
The serial log goes to `$KUMO_DATA_DIR/console/<instance-id>.log`, the file the console-output
endpoint of M4.4 serves.

Behavior the steps above do not make obvious:

- **Every step adopts what is already there.** A requeued task (see below) finds its own volumes and
  its own domain, so `CreateRootDisk`/`CreateSeedISO` treat `ErrAlreadyExists` as success and the
  domain is reused rather than defined twice.
- **A shutdown is not a failure.** If the worker's context is done — the process is stopping — the
  row stays `pending` and nothing is deleted. The worker leaves such a task `running` and the next
  `Start` requeues it (`worker.Pool.run`), so the right thing is to leave the work for the requeued
  attempt. Only a real failure marks the row `error` and rolls back.
- **The final write is not a reason to destroy a running guest.** The state, the uuid, the MAC and
  the address are written in one statement. If it fails while the guest is up, the domain and its
  volumes are left alone and the row stays `pending`: the reconciler brings it in line, whereas a
  rollback here would throw away a booted instance.
- **Adoption and rollback both tolerate a half-finished step**, because a step that succeeded and then
  failed to be recorded is the normal case, not an exceptional one.

### Terminate
`Destroy` (ignore "not running") → `Undefine` → remove static DHCP host from libvirt network → delete `-root.qcow2` and `-seed.iso` → in 1 txn: delete the tags and the instance row, whose `network_interfaces` rows follow through the foreign key cascade → remove `$KUMO_DATA_DIR/console/<instance-id>.log`. Idempotent: every step tolerates "already gone".

`Undefine` is `virsh undefine --remove-all-storage --nvram` (`internal/hypervisor`): libvirt has no
single call for that, so virsh undefines the domain with its NVRAM file and then deletes the volumes
it read out of the domain XML, and so does `Undefine`. The explicit `-root.qcow2`/`-seed.iso` pass
that follows is what still cleans up when there was no domain left to read the disks from: a task
requeued after an earlier attempt, or a domain an operator removed by hand.

The row is removed with everything else rather than kept as history: an instance that has been
terminated is a deleted resource (`01-api.md`), its name is free again and the capacity report stops
summing what no longer exists. The tasks are what survive, as the record of what was done to the
instance, and `GET /instances/{id}` answers `404` once the terminate has run. A row an earlier
release left in `terminated` is purged by the next `DELETE` for that id.

Every terminate step treats "no such domain" as done, which is what makes the workflow safe to run
twice: a task interrupted after deleting the seed ISO is requeued on the next start and has to be
able to finish.

### Stop / start / reboot
`stop` → `DomainShutdown` (ACPI); a 90 s watchdog task escalates only if `?force=true` was requested; otherwise UI shows `stopping` until guest is off. `force` → `DomainDestroy`. `reboot` → `DomainReboot`; `force` → `DomainReset`.

What the task does around those calls, and why the row moves where it does:

- **`DomainShutdown` is a request, not a fact.** libvirt answers as soon as QEMU has been told, so
  the task polls the domain until it is no longer running, bounded by `KUMO_STOP_TIMEOUT` (90 s).
  On timeout the task fails, the row stays `stopping`, and the operator force-stops — which is why
  `stopping` is one of the states a stop may start from. A domain that is gone counts as stopped: the
  outcome the caller wanted is true, so the row becomes `stopped` rather than `error`.
- **`?force=true` does not wait.** `DomainDestroy` leaves nothing running, so the row is `stopped` as
  soon as the call returns.
- **`start` has no transitional state.** The row stays `stopped` until the domain reports running,
  because `pending` means a launch that has no domain yet (see `00-architecture.md` §4). A domain
  that libvirt does not know is an `error` with `state_reason` starting `domain_missing: `, the same
  conclusion the reconciler reaches.
- **`reboot` writes nothing on success.** The instance was running and is running again. A guest
  that is not running under a row that says it is becomes `stopped` and the task fails, so the row
  stops claiming something the host is not doing.
- **A domain that libvirt refuses for its state is not an error of the host.** `Start` on a running
  domain and `Shutdown` on a shut-off one come back as libvirt's `ERR_OPERATION_INVALID`; the
  service maps them to `hypervisor.ErrInvalidState` and decides: a start that finds the domain
  already running adopts it, a reboot that finds it stopped records that instead.

### One lock per instance
A launch and every lifecycle action hold `locks.Set` on `instance.LockKey(id)` for as long as they
work on the instance, so two tasks can never work on the same domain at once. The two callers treat
it differently on purpose:

- **A request** (`POST /instances/{id}/stop` and friends) uses `TryAcquire` and answers `409` with
  the name of the holder, because an operator pressing stop twice deserves to be told the instance
  is busy, not to wait behind a task they cannot see.
- **A task** uses `Acquire` and waits, because two queued tasks for one instance are going to run in
  some order and the queue will run them both.

The task row is the second guard: a task left `pending` or `running` by a killed process holds no
lock until the pool requeues it, so an action is also refused when the latest task of the instance
is unfinished.

## 4. cloud-init defaults

If the user supplies no `user_data` and a key pair is chosen, generate:

```yaml
#cloud-config
users:
  - name: <image.default_user>
    ssh_authorized_keys:
      - <public_key>
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
ssh_pwauth: false
```

If the user supplies `user_data` **and** a key pair: if it begins with `#cloud-config`, merge `ssh_authorized_keys` into the default user (`ssh_authorized_keys` top-level key is honored by cloud-init for the default user); otherwise (script / MIME), deliver the key through a separate MIME part (`write-mime-multipart` semantic, text/cloud-config + user's part). Keep this logic in `internal/service/cloudinit` with unit tests.

## 5. Domain XML template (x86_64, KVM)

Build via Go structs/`text/template` with escaped values (names/ids are validated IDs).

```xml
<domain type='kvm'>
  <name>{{.ID}}</name>
  <uuid>{{.UUID}}</uuid>
  <metadata>
    <kumo:instance xmlns:kumo='https://kumo.example/xmlns/1'>
      <kumo:name>{{.Name}}</kumo:name>
    </kumo:instance>
  </metadata>
  <memory unit='MiB'>{{.MemoryMiB}}</memory>
  <vcpu placement='static'>{{.VCPUs}}</vcpu>
  <os>
    <type arch='x86_64' machine='q35'>hvm</type>
    <boot dev='hd'/>
  </os>
  <features><acpi/><apic/></features>
  <cpu mode='host-passthrough' check='none'/>
  <clock offset='utc'>
    <timer name='rtc' tickpolicy='catchup'/>
    <timer name='pit' tickpolicy='delay'/>
    <timer name='hpet' present='no'/>
  </clock>
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>destroy</on_crash>
  <devices>
    <emulator>/usr/bin/qemu-system-x86_64</emulator>  <!-- omit; let libvirt choose -->
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' cache='none' discard='unmap'/>
      <source file='{{.RootDiskPath}}'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='{{.SeedISOPath}}'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
    <interface type='network'>
      <source network='{{.Network}}'/>
      <model type='virtio'/>
    </interface>
    <serial type='file'>
      <source path='{{.ConsoleLogPath}}'/>
      <target port='0'/>
    </serial>
    <console type='file'><source path='{{.ConsoleLogPath}}'/><target type='serial' port='0'/></console>
    <channel type='unix'>
      <target type='virtio' name='org.qemu.guest_agent.0'/>
    </channel>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
    <video><model type='virtio'/></video>
    <rng model='virtio'><backend model='random'>/dev/urandom</backend></rng>
    <memballoon model='virtio'/>
  </devices>
</domain>
```

Implementation notes:
- Omit `<emulator>`; libvirt resolves it. Remove the comment above when implementing.
- The `<serial type='file'>` + `<console>` duplication above is illustrative. **Choice taken: `<serial type='pty'>` with `<log file=… append='on'/>`, plus `<console type='pty'>` on the same port.** A pty is what `virsh console` needs to attach, and the log file is what the console-output endpoint reads, so one device serves both without a guest ever blocking on a full file. `<log>` is a child of `<serial>` since libvirt 1.3, which is well below any host Kumo supports. When `ConsoleLogPath` is empty the `<log>` element is left out rather than emitted with an empty path.
- The XML is produced by `internal/hypervisor/libvirt/domainxml` from `encoding/xml` structs, never by formatting a template, so operator input is escaped by the encoder. Golden files live in `internal/hypervisor/libvirt/domainxml/testdata`; run `go test ./internal/hypervisor/libvirt/domainxml -update` after an intentional change.
- `<graphics>` binds to `127.0.0.1`; `<channel type='unix'>` is the qemu-guest-agent socket that IP discovery reads.
- The seed ISO is built by `internal/seediso`, which is independent of libvirt and of the instance service: it takes the three already-rendered documents and writes them into an ISO9660 image labelled `cidata`. Writing goes through a temporary file and a rename, so a failed build never leaves a truncated ISO at a path about to be attached to a domain.
- UEFI/aarch64 support is out of scope for M4; leave `Firmware` field in `DomainSpec` for later (`<os firmware='efi'>`).
- Honor `/dev/kvm` absence: `EnsureInfra`/`Ping` should report `kvm: false` in `/system/info`, and launches fail with `unavailable`.

## 6. Networking: VPCs & Subnets

### 6.1 VPC to libvirt network mapping

- **One VPC = one libvirt network = one Linux bridge.**
- Bridge naming: `kmb` + the first 8 hex characters of the VPC ID (e.g. `kmb0a1b2c3d`). This guarantees bridge interface names fit Linux's 15-character `IFNAMSIZ` limit (`len("kmb") + 8 = 11 <= 15`).
- Libvirt network naming: named after the VPC ID (e.g. `vpc-0a1b2c3d4e5f60718`), exactly as a domain is named after its instance ID.
- Default network adoption: an existing default network (e.g. `kumo-default` with bridge `virbr-default`) is adopted on first run if found; its existing names are kept and persisted on the VPC row (`libvirt_network`, `bridge_name`).
- Isolation: VPCs are isolated from each other by libvirt's forwarding rules and distinct bridges.

### 6.2 Forwarding modes

- `mode: "nat"`: libvirt `<forward mode='nat'><nat><port start='1024' end='65535'/></nat></forward>`. Outbound Internet/LAN access via host NAT.
- `mode: "isolated"`: no `<forward>` element in the libvirt network XML. Guests communicate only with other guests on the same VPC and with the host gateway.
- `cidr`, `mode`, and `name` are immutable once the VPC is created. The VPC `name` seeds the libvirt network DNS domain (`<domain name='...'/>`). Only `tags` are patchable.

### 6.3 Subnets: logical partitions

- **Subnets are logical.** A subnet is a DB-only allocation partition of its VPC's bridge.
- No L2 separation, no inter-subnet routing or filtering.
- Guests receive the parent VPC's netmask, so the gateway (`gateway_ip`, the first host address of the VPC CIDR) belongs to the VPC, not to the subnet.

### 6.4 IP & MAC allocation: Kumo allocates, dnsmasq only serves

- **Kumo is the sole allocator.** Libvirt's `<dhcp>` block contains **no dynamic `<range>` element**.
- When an instance is launched, Kumo chooses:
  1. The lowest free usable IPv4 address in the chosen subnet (excluding subnet network, broadcast, and VPC gateway addresses).
  2. A locally administered unicast MAC address (`52:54:00:xx:xx:xx`).
- Both are recorded in `network_interfaces` inside the launch transaction.
- Before the domain boots, Kumo writes a static DHCP host entry to the libvirt network via `virNetworkUpdate` (`VIR_NETWORK_UPDATE_COMMAND_ADD_LAST`, section `VIR_NETWORK_SECTION_IP_DHCP_HOST`):
  ```xml
  <host mac='52:54:00:12:34:56' name='web-1' ip='10.0.1.10'/>
  ```
- dnsmasq serves exactly what Kumo configured. This guarantees IP and MAC stability across instance stop/start cycles.
- On instance termination, the static DHCP host entry is removed via `VIR_NETWORK_UPDATE_COMMAND_DELETE`, and the `network_interfaces` row is removed.

### 6.5 Network interfaces

- One interface per instance initially (`device_index = 0`), modelled for multi-NIC later.
- An interface exists exactly as long as its instance does.
- Deletion of the `network_interfaces` rows is the cascade of the instance row delete, which a
  terminate makes: the same statement, the same transaction, and no history left in
  `instances.private_ip` and `instances.mac_address` to preserve either — those go with the row.

### 6.6 Validation & collision prevention

- VPC CIDR must be RFC 1918 (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`) and prefix length between `/16` and `/28`.
- Subnet CIDR must sit strictly inside the parent VPC CIDR, between `/16` and `/28`, and not overlap any sibling subnet.
- **Host route collision check:** Before defining a new VPC network in libvirt, Kumo inspects all host interface addresses and routing table entries. If the proposed VPC CIDR overlaps with any host route/interface, creation is rejected with `409 conflict` explaining the collision. Kumo's own bridges (`kmb*` and `virbr-*`) are excluded from this host scan, as existing VPC records already track them.

### 6.7 Concurrency & consistency

- State checks and writes share a single SQLite transaction (single writer in WAL mode). A launch cannot slip into a VPC or subnet that a delete has just checked.
- Async tasks serialize on `vpc.LockKey(id)`.

## 7. Events & reconciliation

Register for domain lifecycle events (`ConnectDomainEventRegisterAny` with lifecycle ID); map `STARTED/STOPPED/SHUTDOWN/CRASHED/DEFINED/UNDEFINED` to a reconcile trigger for that ID. Events are a hint; the periodic full reconcile is the safety net.

## 8. Host prerequisites & doctor check

`kumod doctor` verifies and prints remediation:
- `/dev/kvm` present and accessible
- `libvirtd` socket reachable by the `kumo` service user (group `libvirt`)
- QEMU installed; `qemu-img` optional
- Pools/network exist and are active
- Free disk space in the pools directory (warn < 20 GiB)
- `ip_forward` enabled and firewall (nftables/iptables) not blocking libvirt NAT

## 9. Testing strategy

- Unit: XML builder (golden files), cloud-init merge, qcow2 header parser, ISO builder (mount-less verification by re-reading with the same library).
- Service tests use the **fake** hypervisor.
- Integration (opt-in, tag `//go:build libvirt`, `make test-libvirt`): runs against a real libvirtd using `qemu:///session` or a test system with a tiny CirrOS image; creates/destroys a VM end to end. CI runs this on a nested-virt runner when available.
