# 08 — Kubernetes clusters (EKS-like, kubeadm)

Kumo's third service beyond compute. A **cluster** is a Kubernetes control plane
and a group of worker nodes that Kumo creates, scales, watches and deletes for
you, with an API endpoint inside a VPC and a kubeconfig the console hands out.
The virtual machines underneath are Kumo's, not the user's, and do not appear on
the Instances pages. This is the "modules registered under `/api/v1/<service>`"
extension point of `00-architecture.md` §10, and the AWS EKS vocabulary the
console mimics.

It is built the way the databases module is (`07-databases.md`): **a golden
image with everything installed, configured on first boot by a cloud-init seed
kumod renders, with no SSH and no agent.** The image carries CRI-O, kubeadm,
kubelet and the control-plane container images; the seed carries a cluster CA and
a join token kumod generated before any VM booted, so `kubeadm init` on the
control plane and `kubeadm join` on every worker need nothing from anybody at
boot.

## 0. Status

**APPROVED 2026-11-20. Not built.** `05-roadmap.md` lists Kubernetes
clusters in the post-v0.1 backlog ("do not start without approval"), and
`AGENTS.md` puts it outside the current phase. The owner chose kubeadm on a
golden image over Kubespray (K1, §10) and answered every open question on
2026-11-20 (§11): Kumo keeps each cluster's CA key, the first version is
Kubernetes **1.37**, pod networking is **Cilium**, and the client, ID prefixes
and API path stand as proposed. The milestone is M8 in `05-roadmap.md`, which
carries each task's acceptance criteria; §12 below is its summary.

### Scope of the first version

The point is one cluster shape that comes up correctly and unattended, not a
feature matrix.

| In v1 | Not in v1 |
|---|---|
| Kubernetes 1.37, x86_64 | Several versions at once, aarch64 |
| Create, list, detail, delete a cluster | Kubernetes version upgrades |
| One control-plane node | Three control-plane nodes (HA), a VIP |
| One worker node group, 1 to 10 nodes, resized up and down | Several node groups, autoscaling, taints and labels per group |
| API endpoint reachable from inside its VPC | Public endpoint, an API proxy through kumod |
| Admin and read-only kubeconfigs from the console | OIDC, IAM-style mapping, per-user identities |
| Cilium, VXLAN tunnelling, `NetworkPolicy` enforced | A choice of CNI, a network policy UI, Hubble, kube-proxy replacement |
| `local-path` storage on the node's root disk | A CSI driver (needs the volumes milestone) |
| `NodePort` services inside the VPC | `LoadBalancer` services (MetalLB or a Kumo CCM) |
| Cluster and node health in the console | Metrics, logs, add-ons catalog, Fargate-like anything |

Every row on the right is additive: §9 records which of them this design leaves
room for and which would need a redesign.

### Decisions this version fixes

| # | Decision | Rationale |
|---|---|---|
| K1 | **kubeadm, driven by cloud-init, on a golden image** (decided) | Upstream Kubernetes, configured the way Cluster API's kubeadm bootstrap does it. No Ansible or Python on the host (architecture rule 6), no SSH into nodes, no first boot that needs the network. §10 records why not Kubespray |
| K2 | **kumod generates the cluster's PKI and join token before any VM boots** | The control plane's seed carries the CA; every worker's seed carries the token and the CA's public-key hash. Every boot is self-sufficient and every step is idempotent, which is what the task model needs (§4) |
| K3 | **No in-guest agent** | As R3 of `07-databases.md`. kumod observes and operates the cluster through the Kubernetes API, the one interface every cluster already has (§6) |
| K4 | **kumod holds the cluster CA and its own credential** (approved, §5) | Scaling, health, node removal and v2 upgrades all need the API and the CA. This is the opposite of R8 for databases, and the owner accepted it knowingly |
| K5 | **One control-plane node in v1** | Kumo is single-host (`00-architecture.md` §1): three control-plane VMs on one machine fail together, so v1 would pay HA's cost for none of its benefit. The schema and the endpoint (K6) leave room for three |
| K6 | **The API endpoint is a DNS name, not an address** | `api.<cluster>.<vpc>`, served by the VPC's dnsmasq, as R9 does for databases. It is the certificate's SAN from day one, so a VIP in front of three control planes later changes a DNS record, not every kubeconfig |
| K7 | **VPC-internal only** | As R6: security groups are a non-goal, so nothing could make a public API server safe. `kubectl` runs from an instance in the VPC |
| K8 | **Nodes are hidden rows in `instances`, owned by `eks`** | As R7, through the M7 seam (`internal/service/instance/managed.go`): capacity, addressing, rollback and reconciliation stay single-sourced. The seam needs one extension (§4) |
| K9 | **Pod and service ranges come from 100.64.0.0/10** | Kumo VPCs are RFC 1918, so a shared-address-space range cannot overlap any of them. Defaults: pods `100.64.0.0/16`, services `100.96.0.0/16` |
| K10 | **Kumo roles map to Kubernetes groups, not to `system:masters`** | Admin kubeconfigs carry the group `kumo:admins`, bound to `cluster-admin`; viewer ones `kumo:viewers`, bound to `view`. A binding can be deleted; `system:masters` cannot be revoked by anything short of rotating the CA |
| K11 | **No Kubernetes client library** (approved) | kumod needs about six API calls (§6). Plain HTTPS with a client certificate and `encoding/json` keeps `client-go` and its dependency tree out of the binary |
| K12 | **Cilium for pod networking, in VXLAN tunnel mode, with kube-proxy kept** (approved) | Tunnelling needs nothing from the VPC bridge beyond node-to-node UDP, so it works the same on a NAT and an isolated VPC. kube-proxy stays, as kubeadm installs it, so v1 has one fewer moving part; replacing it with Cilium's eBPF datapath is a later, per-image change (§9). Cilium enforces standard `NetworkPolicy` from day one |
| K13 | **Kubernetes 1.37 first** (approved) | One minor per image (§7). The version is a column and an image, so 1.38 is a new image beside it |

## 1. Shape

```
 console ──/api/v1/eks/*──▶ kumod ── service/kubernetes
                                      │
                                      ├─ store (SQLite: k8s_clusters, k8s_node_groups,
                                      │         hidden rows in instances)
                                      ├─ service/instance (managed seam: reserve, launch, terminate)
                                      ├─ hypervisor.Hypervisor (domains, DHCP hosts)
                                      └─ k8sapi (stdlib HTTPS client, kumod's own credential)
                                                  │
 kubectl in the VPC ──HTTPS 6443──▶ api.<cluster>.<vpc> ── control-plane VM ── worker VMs
                                    bytes never pass through kumod
```

- **Layering is unchanged.** Handlers call `service/kubernetes`; it calls the
  store, `service/instance`, the hypervisor and `k8sapi`. Nothing imports `gin`
  below the handlers, and nothing touches libvirt outside `internal/hypervisor`.
- **Three sources of truth, each for its own facts.** libvirt owns whether the
  VMs run; SQLite owns the cluster's metadata, its version, its node group's
  desired size and which instances back it; the Kubernetes API owns whether the
  cluster is healthy and which nodes are `Ready`. §6 says how kumod reads the
  third.
- **Long operations are tasks:** `k8s.create`, `k8s.scale`, `k8s.delete`.

## 2. Configuration

| Variable | Default | Description |
|---|---|---|
| `KUMO_KUBERNETES` | `on` | `on` or `off`. With `off`, `/api/v1/eks/*` answers `503 unavailable` and the console hides the Kubernetes pages |
| `KUMO_K8S_READY_TIMEOUT` | `20m` | How long `k8s.create` waits for the API server and every node to be `Ready` |
| `KUMO_K8S_PROBE_INTERVAL` | `30s` | How often the health pass of §6 checks an active cluster |

On by default for the reason the databases module is: it needs nothing outside
the binary except an engine image, which is a state the console reports, not a
configuration error. `/system/info` gains `kubernetes: {enabled, versions}`;
`kumod doctor` warns when the module is on and no image is available.

## 3. Data model

One migration, `0008_kubernetes.sql`:

```sql
-- +goose Up
CREATE TABLE k8s_clusters (
  id                  TEXT PRIMARY KEY,           -- eks- + 17 hex (§11, answer 5)
  name                TEXT NOT NULL UNIQUE,       -- DNS label; the endpoint is api.<name>.<vpc>
  kubernetes_version  TEXT NOT NULL,              -- '1.37', matched against images.engine_version
  image_id            TEXT NOT NULL REFERENCES images(id),
  vpc_id              TEXT NOT NULL REFERENCES vpcs(id),
  subnet_id           TEXT NOT NULL REFERENCES subnets(id),
  pod_cidr            TEXT NOT NULL,
  service_cidr        TEXT NOT NULL,
  control_plane_count INTEGER NOT NULL DEFAULT 1 CHECK (control_plane_count IN (1, 3)),
  control_plane_type  TEXT NOT NULL REFERENCES instance_types(id),
  state               TEXT NOT NULL,              -- creating|active|updating|degraded|deleting|failed
  state_reason        TEXT,
  endpoint            TEXT,                       -- https://api.<name>.<vpc>:6443, set when active
  ca_cert_pem         TEXT NOT NULL,              -- public; shown in the console, put in kubeconfigs
  created_by          TEXT NOT NULL REFERENCES users(id),
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);

CREATE TABLE k8s_node_groups (
  id              TEXT PRIMARY KEY,               -- ng- + 17 hex
  cluster_id      TEXT NOT NULL REFERENCES k8s_clusters(id) ON DELETE CASCADE,
  name            TEXT NOT NULL,                  -- 'default' in v1
  instance_type   TEXT NOT NULL REFERENCES instance_types(id),
  root_disk_gb    INTEGER NOT NULL,
  desired_size    INTEGER NOT NULL CHECK (desired_size BETWEEN 1 AND 10),
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  UNIQUE (cluster_id, name)
);

-- Which instances are this cluster's nodes, and in which role.
CREATE TABLE k8s_nodes (
  instance_id     TEXT PRIMARY KEY REFERENCES instances(id),
  cluster_id      TEXT NOT NULL REFERENCES k8s_clusters(id) ON DELETE CASCADE,
  node_group_id   TEXT REFERENCES k8s_node_groups(id) ON DELETE CASCADE,  -- NULL for control plane
  role            TEXT NOT NULL CHECK (role IN ('control-plane', 'worker'))
);
```

`instances.managed_by` gains the value `eks`. The private keys of §5 are **not
in SQLite**: a copy of `kumo.db` is a thing operators make casually, for backups
and bug reports, and it should not be a copy of every cluster's CA.

## 4. Lifecycle

### Create (task `k8s.create`)

The handler validates, resolves and reserves synchronously, then returns `202`
with the cluster `creating`.

```
validate  name, version, node count, the types (control plane >= 2 vCPU, every
          node >= 2048 MiB, kubeadm's preflight minimums), the node root disk
resolve   the image: kind='engine', engine='kubernetes', engine_version, available
          -> 409 no_engine_image
generate  the PKI of section 5 and write it under $KUMO_DATA_DIR/clusters/<id>/
          (0600, written once); a create the reservation refuses removes it
reserve   in 1 txn, instance.WithManagedReservations: capacity for 1 + node_count
          VMs as one sum, the instance rows and their addresses, k8s_clusters,
          k8s_node_groups, k8s_nodes, the task
  1. control plane: root overlay, seed (kubeadm init config + the PKI, a token
     nobody uses), define and start, through LaunchManaged -- which adopts the
     seed of an earlier attempt rather than rendering a new one
  2. a DNS record on the VPC's network: api.<cluster>.<vpc> -> the control
     plane's address (hypervisor.AddDNSHost; dnsmasq serves it, K6)
  3. wait for /readyz, dialing the control plane's address with the endpoint as
     the TLS name. The add-ons (Cilium, local-path, the RBAC) are the seed's
     post-init step, not kumod's
  4. mint the workers' bootstrap token through the API (1 h), then each worker:
     root overlay, seed (kubeadm join: endpoint, that token, the CA hash),
     domain. The token comes from the running API rather than from step 1's
     seed, so a requeued create never needs a token it no longer has. The
     mint is part of step 3's wait: /readyz answers before the seed has
     applied the RBAC that admits kumo:system, and on a fast node the gap is
     seconds, so a 403 (or an unreachable API) is "not yet" and is retried
     until the ready timeout; any other error fails the create at once
  5. wait until every expected node reports Ready, via the API
  6. in 1 txn: instances -> running, cluster -> active, endpoint written; and
     degraded, with the nodes named, if step 5 timed out
on failure before the API answers: cluster -> failed with the reason, and the
  VMs kept for their consoles; delete is the way out (as for a database whose
  engine never answered)
```

**The seam extension** is `instance.WithManagedReservations` (M8.3): the
capacity of the whole set is checked as one sum before anything is locked or
written, and every row commits with the module's own in one transaction, so a
cluster is reserved whole or not at all.

Two failures are worth naming:

- **API up, some nodes never `Ready`.** The control plane is fine and holds the
  evidence. The cluster goes `degraded`, not `failed`, the VMs stay, and the
  console shows which nodes are not `Ready`. Delete or a scale back down are the
  ways out.
- **A requeued task.** Every step adopts what exists: the PKI directory, the
  volumes, the domains, and nodes already joined. A second `kubeadm init` on a
  control plane whose `/etc/kubernetes/admin.conf` exists is skipped by the seed.

### Scale (task `k8s.scale`)

`PATCH /eks/clusters/{id}/node-groups/{ng}` with a new `desired_size`; the
cluster goes `updating`.

- **Up:** kumod creates a fresh bootstrap token through the API (a Secret in
  `kube-system`, 1 h TTL), reserves the VMs and launches them with join seeds.
  Waits for the new Nodes to be `Ready`.
- **Down:** the newest workers first. For each: cordon, evict its pods through
  the Eviction API (honours PodDisruptionBudgets) with a 5-minute limit, delete
  the Node object, terminate the VM. A drain that hits the limit stops the scale
  with `degraded` and says which pods would not move; it never deletes past a
  PDB.

### Delete (task `k8s.delete`)

Allowed from every state but `deleting`. No drain: the whole cluster is going.
Terminate every node VM through the seam, delete the rows, then the PKI
directory last, so a delete that fails partway can still be retried with a
working credential. `local-path` data dies with the nodes, and the delete modal
says so.

### Upgrades

Not in v1. The design for v2 is replacement, not in-place `kubeadm upgrade`:
import the image for the next minor, roll the control plane (with three
control-plane nodes that is a rolling replacement behind the K6 name), then
replace workers one at a time with drain. Replacement keeps the no-agent rule;
in-place would need a way to run commands on a node.

## 5. The cluster credential

**This is where the design departs from databases.** R8 says Kumo never holds a
database's password, and that works because nothing after create needs it. A
cluster is different: scaling needs a bootstrap token kumod mints through the
API, scale-down needs to evict and delete Nodes, health needs to read them, and
v2 upgrades need the CA to join new control-plane nodes. kumod must hold a
credential for as long as the cluster exists.

What kumod generates at create, all in pure Go (`crypto/x509`, `crypto/ecdsa`,
`crypto/ed25519`), and where each piece lives:

| Material | Where | Why it is kept |
|---|---|---|
| Cluster CA cert | SQLite (`ca_cert_pem`) and the control-plane seed | Public; every kubeconfig embeds it |
| Cluster CA key | `$KUMO_DATA_DIR/clusters/<id>/ca.key`, 0600, and the control-plane seed | Signs kubeconfigs on demand (K10) and, in v2, new control-plane nodes |
| front-proxy CA, etcd CA, service-account key | Same directory, and the control-plane seed | kubeadm needs all four to init without generating its own; v2's second and third control planes need the same ones |
| kumod's client cert and key (`kumo:system`, bound to a role with exactly the verbs of §6) | Same directory | kumod's own access; not `cluster-admin` |
| Bootstrap token | The control plane's kubeadm configuration (which creates it) and the worker seeds; 1 h TTL | Expires on its own; scale-up mints a new one |

The seed ISO stays attached to the domain, as for databases, so **the control
plane's seed holds CA keys for as long as the VM exists**. That is no wider than
the control plane's own `/etc/kubernetes/pki`, which holds the same keys by
design. The first-boot script shreds cloud-init's copies under `/var/lib/cloud`,
as `dbcloudinit` does. Worker seeds hold only the expiring token and the CA hash.

**As built (M8.2).** `internal/service/k8spki` generates all of it with the
standard library, every key ECDSA P-256, the CAs with kubeadm's own common names
and its 10-year validity, kumod's certificate for one year (kumod holds the CA,
so it re-signs its own whenever it likes). Private material is a `Secret`, whose
`String`, `GoString`, `Format`, `LogValue` and `MarshalJSON` all refuse to show
it, so a key cannot reach a log line or a task payload by being formatted. The
directory holds ten files, all 0600 in a 0700 directory, written by
temp-file-and-rename with the CA certificate last; a second write over an
existing CA is refused, because it would lock kumod out of a running cluster.
`internal/service/k8scloudinit` renders the seeds: the control plane's writes
the PKI where kubeadm looks for it and kubeadm signs every leaf with kumod's
CAs; a worker's pins the CA by hash and never skips verification. Neither names
a Kubernetes version: the control plane fills in its own kubeadm's version at
boot, so a seed cannot ask an image for a version it does not carry (which
would make kubeadm pull). Both write the endpoint into `/etc/hosts` at the
control plane's address, so a first boot does not wait on the VPC's DNS. The
scripts interpolate nothing: every request value reaches the node as a file.

**Kubeconfigs are minted, not stored.** `POST /eks/clusters/{id}/kubeconfig`
signs a fresh client certificate for the calling user (CN = Kumo username, O =
`kumo:admins` or `kumo:viewers` by role), valid 30 days, and returns the file
once. Nothing is recorded but an audit line. Revoking every admin kubeconfig for
a cluster is deleting the `kumo:admins` ClusterRoleBinding and re-creating it
under a new group name, which the API can do without touching the CA.

The alternative is to return the CA key once and keep nothing, as for a DB
password. It is rejected because the cluster could then never be scaled, healed
or upgraded by Kumo, which is the point of the service. Waiting for a secrets
service (OpenBao is in the backlog) is the other alternative; the directory
above is the same shape a KMS-wrapped key would take later.

## 6. Readiness and health

kumod reaches each cluster's API from the host, which sits on every VPC bridge
as its gateway, exactly as the database readiness probe reaches port 5432. The
calls, all plain HTTPS with kumod's client certificate (K11):

| Call | Used for |
|---|---|
| `GET /readyz` | Create step 2; the health pass |
| `GET /api/v1/nodes` | Create step 5, scale, the health pass: `Ready` condition per node |
| `POST /api/v1/namespaces/kube-system/secrets` | Bootstrap token for scale-up |
| `PATCH /api/v1/nodes/{name}` | Cordon |
| `POST /api/v1/namespaces/{ns}/pods/{pod}/eviction` | Drain (with `GET /api/v1/pods?fieldSelector=spec.nodeName=...`) |
| `DELETE /api/v1/nodes/{name}` | Scale-down |

The health pass runs every `KUMO_K8S_PROBE_INTERVAL` for `active` and
`degraded` clusters: `active` → `degraded` when `/readyz` fails or a node is not
`Ready` for three passes running; back to `active` when both recover. A stopped
or missing VM is the instance reconciler's to report, as it is for databases.

## 7. The golden image

Built by `.github/workflows/engine-images.yml`, as the Postgres image is
(`07-databases.md` §7): recipe `packaging/images/kubernetes-1.37/`, imported with
`kind=engine`, `engine=kubernetes`, `engine_version=1.37`. Contents:

- **Ubuntu 26.04 LTS** (kernel 7.0, supported to 2031), where the Postgres image
  stays on 24.04: the base is a per-recipe setting (`recipe.env`)
- **CRI-O 1.37** from its own repository (`isv:cri-o` on the openSUSE Build
  Service, which it moved to from `pkgs.k8s.io`), and `kubeadm`, `kubelet`,
  `kubectl` 1.37 from `pkgs.k8s.io`, all held at the versions the image is named
  after. CRI-O's version follows Kubernetes' minor for minor and it bundles its
  own OCI runtime and conmon, so the image installs one runtime package rather
  than pinned containerd and runc tarballs (Ubuntu's own containerd, 1.6 on
  24.04 and 1.7 on 26.04, is too old for 1.37, which needs 2.3 or later). Both
  repositories' signing keys are pinned in the recipe, and `preflight.sh`
  refuses an expired one before the build starts
- **One behaviour users will notice:** CRI-O's default capability set leaves
  out `NET_RAW`, which containerd grants, so `ping` and other raw sockets fail
  inside a pod unless its `securityContext` adds `NET_RAW`. That is CRI-O's
  default and arguably the safer one; it is kept, and the console's Connect
  tab should say so (M8.7). `verify.sh` tests pod-to-pod over TCP for this
  reason
- The control-plane, CoreDNS, Cilium (agent, operator, Envoy) and local-path
  images **pre-pulled**, so no boot reaches a registry (the same rule as §7 of
  07). The add-on images are pinned by digest (`files/images.txt`); CRI-O's
  pause image is set to the one kubeadm names, so it is the pulled one. CRI-O
  keeps each image once, unpacked
- **Cilium as a committed manifest, not a chart.** `render-cilium.sh` runs
  `helm template` against the chart archive checked by its published digest,
  with `cilium-values.yaml` (VXLAN tunnelling, `ipam.mode=kubernetes`, kube-proxy
  kept, Hubble off), and the output is committed as `files/cilium.yaml`. A Cilium
  bump is a reviewable diff, and the build needs no Helm. The manifest has **no
  per-cluster values**: pod ranges come from each node's `podCIDR`, which kubeadm
  carves out of the cluster's pod range, and the agent reaches the API server
  through the `kubernetes` Service, which kube-proxy programs. Neither Helm nor
  the `cilium` CLI is in the image or on the host
- local-path-provisioner as a committed manifest with its images pinned by
  digest: upstream's helper pod uses a bare `busybox`, which is `:latest` and
  would be pulled on every volume, so the copy here pins it, and marks the
  StorageClass as the default
- Kernel modules and sysctls kubeadm's preflight wants (`br_netfilter`,
  `overlay`, `net.ipv4.ip_forward`), no swap, `kubelet` enabled but inert until
  kubeadm writes its config; snapd removed
- No PKI, no kubeconfig, no token, no cluster: all of it arrives in the seed
- One kernel, no headers, a size report in the serial log
  (`packaging/images/lib/guest-finish.sh`, shared with Postgres)

The same image serves both roles; the seed decides which. **Size**, measured
at M8.1 (Kubernetes 1.37.2, CRI-O 1.37.4, Cilium 1.20.5): 3634 MiB as a qcow2,
1337 MiB gzipped. Of the 3.6 GiB, the unpacked container images are most of
`/var` (1.6 GiB) and `/usr` holds the OS, the 7.0 kernel's modules and the
Kubernetes binaries (1.6 GiB). For comparison, the same recipe on containerd
and 24.04 was 3802 MiB, because containerd also keeps every image's
compressed layers.
That is past GitHub's 2 GiB release-asset limit uncompressed, which is why
engine images are published gzipped and decompressed on import (`07-databases.md`
§7); the workflow measures the `.gz` against the limit.

`verify.sh` boots it twice, as a control plane and as a worker joined to it, on
a private link with no route anywhere else, so any image pull would fail. It
checks that both nodes are `Ready`, the add-ons roll out from the image's own
manifests, a PersistentVolumeClaim binds on the default StorageClass, a pod
reaches a pod on the other node through the tunnel, and no pull event happened.

## 8. API (`/api/v1/eks`)

Full schemas go in `api/openapi.yaml` and `01-api.md` in the PR that implements
each endpoint.

| Method | Path | Description |
|---|---|---|
| GET | `/eks/clusters` | List, paged and sorted like every collection |
| POST | `/eks/clusters` | (admin) Create, with its one node group → `202`, `creating` |
| GET | `/eks/clusters/{id}` | Detail: version, endpoint, CA cert, state, node group, nodes with `Ready` |
| DELETE | `/eks/clusters/{id}` | (admin) → `202`, `deleting` |
| GET | `/eks/clusters/{id}/node-groups` | The node groups (one in v1) |
| PATCH | `/eks/clusters/{id}/node-groups/{ng}` | (admin) `{desired_size}` → `202`, `updating`. `409` unless `active` or `degraded` |
| POST | `/eks/clusters/{id}/kubeconfig` | Mint a kubeconfig for the caller, by role (§5). `409` unless `active` or `degraded` |

The cluster's VMs answer `409` with the `/eks/clusters` path on every
`/instances/{id}` write, as database VMs do with `/rds/db-instances`.

Console (`03-frontend.md`): a **Kubernetes** nav group with the Clusters table, a
create form (name, version, VPC and subnet, control-plane type, node type and
count), a detail page with Overview, Nodes and Connect tabs (download
kubeconfig, the endpoint, a `kubectl` example, and the VPC-only note), resize
for the node group, and a delete modal with the name typed to confirm and the
`local-path` warning.

## 9. What this leaves room for

**Cheap later, because of a decision above:**
- **Three control-plane nodes** — K5, K6: `control_plane_count` already allows 3,
  the endpoint is already a name, and the stacked-etcd join needs only the CAs
  kumod already holds. Needs a VIP (kube-vip as a static pod in the image).
- **Upgrades by replacement** — §4: an image per minor, and the PKI to join new
  nodes.
- **More node groups** — the table exists; v1 just creates one.
- **`LoadBalancer` services** — Cilium's own LB IPAM with L2 announcements (K12
  makes MetalLB unnecessary), handing out a VPC address range Kumo reserves from
  the subnet allocator; reachable inside the VPC only, like everything else.
- **kube-proxy replacement** — K12: Cilium's eBPF service handling is a change
  to the rendered manifest and to the kubeadm config (skip the kube-proxy
  phase), so it arrives with a new image, not a redesign.
- **An API proxy through kumod** — so `kubectl` works from outside the VPC, behind
  Kumo's own authentication, the way the VNC console is proxied.

**Needs another milestone first:**
- **Persistent volumes that outlive a node** — a CSI driver needs the volumes
  milestone (backlog).
- **Several hosts, and HA that means anything** — multi-host (backlog).
- **Public endpoints, network policy UI** — security groups (backlog).

## 10. Why not Kubespray

Kubespray was the first candidate and is a good tool for an operator building a
cluster by hand. As the engine of a managed service inside Kumo it fails on
four counts:

1. **It is Ansible.** The Kumo host would need Python and Ansible at runtime,
   against architecture rule 6, or a helper VM to run them in.
2. **It pushes over SSH.** Kumo would hold SSH keys to every node; the databases
   module deliberately has no shell on its VMs and no agent in them.
3. **It is slow and online.** 20 to 40 minutes, downloading binaries during the
   run unless an offline mirror is built and maintained.
4. **It is not a task.** A playbook that fails halfway leaves a cluster nobody can
   describe precisely, where Kumo's tasks must be idempotent and roll back.

kubeadm with a pre-generated PKI does what Kubespray's `kubeadm` role does,
without the parts that fail those four. Kubespray's hardening defaults (audit
policy, secret encryption at rest, kubelet flags) are worth copying into the
kubeadm configuration the seed renders.

## 11. Owner decisions (answered 2026-11-20)

| # | Question | Answer |
|---|---|---|
| 1 | May Kumo hold each cluster's CA key under `$KUMO_DATA_DIR/clusters/<id>/` (0600)? | **Yes** (K4, §5) |
| 2 | Which Kubernetes minor for v1? | **1.37** (K13) |
| 3 | CNI: Calico or Cilium? | **Cilium** (K12), VXLAN, kube-proxy kept |
| 4 | No `client-go`; plain HTTPS for six calls | **Yes** (K11); M8 adds no Go dependency |
| 5 | ID prefixes `eks-` (clusters) and `ng-` (node groups) | **Yes**; `internal/id` gains both in M8.3 |
| 6 | API path `/api/v1/eks` | **Yes** (§8) |

Settled since, with the owner: **CRI-O rather than containerd, and Ubuntu 26.04
for this image** (§7), and **gzip transport** for engine images, because the
Kubernetes image does not fit the release-asset limit uncompressed. M8.1 pins
Kubernetes 1.37.2, CRI-O 1.37.4 and Cilium 1.20.5. Still to settle inside the
tasks, without needing the owner: the RBAC verbs of
kumod's own `kumo:system` role (M8.2), which §6 bounds.

## 12. Milestone (M8)

One task per PR, in order (acceptance criteria in `05-roadmap.md`):

- **M8.1 Golden image recipe and verify** — `packaging/images/kubernetes-1.37/`
  on Ubuntu 26.04 with CRI-O, the Cilium manifest rendered and committed, the
  build shared with Postgres (`packaging/images/lib/`), gzip transport in the
  image import, added to the engine-images
  workflow; `verify.sh` boots a control plane and a worker and sees both
  `Ready` with Cilium's agent healthy on each.
- **M8.2 PKI and seeds** — `internal/service/k8spki` (CAs, keys, kubeconfig
  signing, bootstrap tokens) and `internal/service/k8scloudinit` (init and join
  seeds), with known-answer tests; verified by booting the M8.1 image with them.
- **M8.3 Data model and seam** — `0008_kubernetes.sql`, `managed_by='eks'`, the
  multi-VM reservation in `managed.go`.
- **M8.4 `k8sapi`** — the stdlib client of §6, tested against `httptest` servers
  that answer like an API server.
- **M8.5 Clusters API and tasks** — create, delete, the health pass, kubeconfig
  minting; `openapi.yaml` and `01-api.md`.
- **M8.6 Scale** — the node-group PATCH, scale up and drain-based scale down.
- **M8.7 Console** — the Kubernetes pages.
- **M8.8 End to end** — on a real node: create a cluster from the published
  image, deploy a workload from an instance in the VPC, scale 2 → 3 → 1, delete
  and confirm every VM and volume is gone. Playwright on the fake for the
  console journey.
