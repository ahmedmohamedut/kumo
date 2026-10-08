# Testing Kumo

Three layers, each answering a question the others cannot. Run the cheap one first:
a failing unit test tells you what broke, a failing manual pass tells you what to look at.

| Layer | Question it answers | Command | Needs |
|---|---|---|---|
| Go unit/handler | Does the service logic and the HTTP contract hold? | `make test` | Go toolchain, gcc (for `-race`) |
| Frontend component | Does the console render the API's real shapes? | `npm --prefix web run test` | Node, npm |
| Manual, real KVM | Does a guest actually boot, get an address and take a power command? | see [§3](#3-manual-end-to-end-against-real-kvm) | libvirtd, KVM, two pools, a network |

`make lint` gates all of it: `golangci-lint`, `oxlint`, `prettier --check` and `tsc`.

> The frontend typecheck is `npm run typecheck` (`tsc -b`). A bare `npx tsc --noEmit`
> in `web/` checks **zero files** — the root `tsconfig.json` is `{"files": [],
> "references": [...]}`, a solution file. It exits 0 on a tree that does not compile,
> which is a very comfortable way to believe something you have not checked.

## 1. Go tests

```bash
make test          # go test ./... -race + npm test
```

`go test ./...` on its own is the fast inner loop. `-race` needs cgo, so it needs a C
toolchain (`gcc`); on a host without one, drop `-race` and treat the result as
indicative rather than final. Nothing in the suite needs real hardware: the fake
hypervisor in `internal/hypervisor/fake` stands in for libvirt, and SQLite is
in-memory or a temp file.

Deliberately excluded from the default run: anything that talks to a real libvirtd.
Those are behind the `libvirt` build tag (see §3).

## 2. Frontend tests

```bash
npm --prefix web run test        # vitest run
npm --prefix web run typecheck   # tsc -b
npm --prefix web run lint        # oxlint
```

`make test` runs the Go suite and this one together; the two are separate so a frontend
failure does not hide behind a Go one.

Component tests render a real page against a mocked network: MSW
(`web/src/mocks/handlers.ts`) serves the API's own envelopes, and `setMockX` helpers
(`setMockInstances`, `setMockImages`, …) let a test start from an empty list or a
specific state. The auth flow is mocked rather than real because the interesting cases
are the ones a healthy server refuses to produce — a rotated access token, a revoked
refresh family, a setup that is already closed.

Mock handlers enforce the same rules the Go service does. The instance mocks apply the
same lifecycle guard table (`start` only from `stopped`, `terminate` from everything but
`terminated`) so a dropdown the API would refuse cannot pass. **If you add a state or
an action, change both.**

Querying Cloudscape components in tests:

- **Prefer accessible queries.** `getByRole`/`getByLabelText` survive a refactor;
  `data-testid` does not.
- `data-testid` on `Input`, `Select`, `Textarea` and `ButtonDropdown` lands on the
  component's **wrapper `div`**, not the control. `getByTestId('disk').value` is
  `undefined`; use `getByRole('spinbutton', {name: 'Root disk'})`.
- `Table` with `selectionType="single"` renders a **radio**, not a checkbox.
- A disabled `ButtonDropdown` entry is `<span role="menuitem" aria-disabled="true">`, so
  `toBeDisabled()` reports it as enabled. Assert on `aria-disabled`.
- `Alert` has no `role="alert"` in this version and renders its `header` as a `<div>`.
  Query it by text.
- Cloudscape's `Select` is a closed combobox: click the trigger (the button showing the
  `placeholder`) before `findByRole('option')`.

## 3. Manual end-to-end against real KVM

The component tests prove the console renders what the API says. They cannot prove the
API says anything true, because the hypervisor is the fake. This leg is the only one
that closes that gap, and it is not automated — `docs/design/05-roadmap.md` records, per
milestone, which of these have actually been run.

### 3.1 Prerequisites

- A host with KVM (`/dev/kvm` present) and a running `libvirtd`.
- Two storage pools and a NAT network, or let `kumod` create them. Startup bootstraps
  dir pools under `$KUMO_DATA_DIR/pools/*` and a `192.168.100.0/24` NAT network named
  `kumo-default` if they are absent (`docs/design/00-architecture.md` §7).
- A cloud image in qcow2 form, small enough to download by hand — CirrOS (~50 MB) is the
  usual choice: <https://cloud-images.ubuntu.com/cirrros/current/cirrros-0.6.x-amd64-disk.img>.

```bash
systemctl start libvirtd
virsh list --all            # the daemon answers
virsh pool-list             # note or create what Kumo will use
virsh net-list
```

### 3.2 Run against the real daemon

```bash
export KUMO_DATA_DIR=/var/lib/kumo
export KUMO_HYPERVISOR=libvirt
export KUMO_LIBVIRT_URI=unix:///var/run/libvirt/libvirt-sock
export KUMO_POOL_IMAGES=kumo-images
export KUMO_POOL_INSTANCES=kumo-instances
export KUMO_NETWORK=kumo-default
export KUMO_COOKIE_SECURE=false     # plain HTTP while testing
export KUMO_LOG_LEVEL=debug

make build      # console + API in one binary
./bin/kumod
```

First run reports `needs_setup: true`; the console walks you through creating the first
administrator. Open `http://localhost:8080`.

### 3.3 The pass

1. **Import.** Images → Import, a CirrOS qcow2 URL, minimum disk ≥ the image's. It moves
   `pending → downloading → available` and the state cell carries a progress bar and,
   on failure, the reason.
2. **Launch.** Instances → Launch instance. Name, image, type, key pair (create one first
   if you want to log in), root disk ≥ the image minimum. It answers immediately in
   `pending`; the detail page should reach `running` on the next poll — seconds, not
   minutes. If it goes to `error`, the reason names the cause.
3. **Confirm the guest is real.** In the console: a **Private IP** appears, and **Console
   output** shows the kernel booting and cloud-init running. Optionally `ssh -i <key>
   cirros@<private-ip>` from a host that can route to `192.168.100.0/24`.
4. **Power.** Stop (graceful) → `stopping` → `stopped`. Start → `running`. Reboot →
   `running` without going through `stopped`. A graceful stop that the guest ignores
   stays in `stopping` — Force stop is the way out.
5. **Terminate.** Terminate asks for the name typed; a wrong name keeps the button shut.
   After it settles, `old-*` volumes are gone from `kumo-instances`
   (`virsh vol-list --pool kumo-instances`), the domain is undefined
   (`virsh list --all`), the console takes you back to the list, and the instance's
   own URL answers `404` — the row is deleted, not left in `terminated` — so the
   name is free to reuse.
6. **Destructive guards.** Try deleting the image an instance was launched from: the
   console keeps you on the page and shows the `409` naming the instance that blocks it.

### 3.4 What this catches that nothing else does

- A domain XML libvirt refuses to define (wrong emulator, missing `<interface>`).
- A disk copy that is the wrong size or not qcow2, so the guest cannot boot.
- The seed ISO not being accepted, so no cloud-init and no injected key.
- DHCP not handing out an address on `kumo-default`.
- Partial resources left behind by a failed launch — the rollback path in
  architecture rule 5 is the code most likely to be wrong and the least likely to be
  covered by a fake-hypervisor test.

### 3.5 Opt-in integration tests

`internal/hypervisor/libvirt/integration_test.go` does the same thing in code and
downloads nothing unless told to:

```bash
export KUMO_TEST_CIRROS_URL=https://cloud-images.ubuntu.com/cirrros/current/cirrros-0.6.x-amd64-disk.img
make test-libvirt                      # go test -tags libvirt ./... -count=1 -timeout 30m
```

`KUMO_TEST_LIBVIRT_URI` and `KUMO_TEST_LIBVIRT_SOCKET` point it at a non-default daemon;
a non-default URI has to come with its own socket. Without `KUMO_TEST_CIRROS_URL` the
boot test skips.

**Status: never yet run against real hardware.** `docs/design/05-roadmap.md` says so per
milestone. Until someone runs it on a KVM host, treat the libvirt half of every task as
written-but-unexercised.