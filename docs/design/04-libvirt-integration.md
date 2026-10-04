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
validate → reserve capacity → INSERT instance(state=pending) + task
  1. CreateRootDisk: qcow2 volume in kumo-instances with
       <backingStore><path>…/img-xxx.qcow2</path><format type='qcow2'/></backingStore>
       capacity = root_disk_gb GiB   (virtual size must be ≥ image virtual size)
  2. CreateSeedISO (NoCloud): volume label "cidata", files:
       meta-data:  instance-id: <i-id>\nlocal-hostname: <name>
       user-data:  user's cloud-config, or generated default (see §4)
       (optional network-config v2: dhcp4 on all NICs)
  3. Build domain XML (see §5) → DomainDefineXML → DomainCreate
  4. Poll up to 120 s for state=running and a DHCP lease → store IP/MAC
  5. UPDATE instance state=running; task succeeded
on failure at step N: Undefine domain (if defined), delete root disk + seed ISO, state=error + reason
```

Seed ISO: build in pure Go (`github.com/kdomanski/iso9660`) → write into the instances pool dir → refresh pool. Fallback: shell out to `xorriso`/`genisoimage` only if the pure-Go path proves unreliable (ask first).

### Terminate
`Destroy` (ignore "not running") → `DomainUndefineFlags` (include NVRAM flag if UEFI) → delete `-root.qcow2` and `-seed.iso` → state `terminated`, `terminated_at` set. Idempotent: every step tolerates "already gone".

### Stop / start / reboot
`stop` → `DomainShutdown` (ACPI); a 90 s watchdog task escalates only if `?force=true` was requested; otherwise UI shows `stopping` until guest is off. `force` → `DomainDestroy`. `reboot` → `DomainReboot`; `force` → `DomainReset`.

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

## 6. Networking (MVP)

- NAT network `kumo-default`, `192.168.100.0/24`, bridge `virbr-kumo`, DHCP range `.10–.250`, defined and autostarted in `EnsureInfra`.
- Instances get private IPs only; reachable from the host. Document how to add routes / DNAT for LAN access. Bridged networking option is a post-MVP item (`KUMO_NETWORK` can point at an existing libvirt bridge network).
- IP discovery: `DomainInterfaceAddresses(dom, DOMAIN_INTERFACE_ADDRESSES_SRC_LEASE)`; fallback to `…_SRC_AGENT` if qemu-guest-agent runs.

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
