# 09 — Spark clusters (EMR-like, standalone)

Managed Apache Spark clusters: a master and a group of workers that Kumo runs
as virtual machines from a golden image, with jobs reading and writing Kumo's
object storage and a history server that outlives the cluster's jobs. The
service a user asks for when they say "give me a Spark cluster": the shape of
Amazon EMR, on one Kumo node.

It follows the pattern Postgres (`07-databases.md`) and Kubernetes
(`08-kubernetes.md`) set: an engine image built out of band, VMs hidden behind
the module, cloud-init seeds rendered by kumod, no in-guest agent, reachable
from inside the VPC only.

## 0. Status

**Approved 2026-11-20; not started.** The owner's answers that shape it,
including S12 and S13, are in §11. Build it one M9 task at a time, in order.

### Scope of the first version

- **Create** a cluster: one master and one node group of 1-10 workers, in a
  VPC subnet, from the Spark 4.3 engine image.
- **Spark standalone mode**: the master on `spark://<cluster>.<vpc>:7077`,
  authenticated with a per-cluster secret and encrypted on the wire.
- **Submit jobs from inside the VPC only**: `spark-submit` (client or cluster
  deploy mode) or `pyspark` from an instance in the same VPC. Kumo has no job
  API in v1.
- **Object storage**: each cluster gets a Kumo access key of its own, scoped to
  the buckets chosen at create, wired into `s3a://` with nothing for the job to
  configure.
- **History server** on the master, reading event logs from a bucket, so a
  job's history outlives the job (and, as data, the cluster).
- **Scale** the node group, with Spark's graceful decommissioning on the way
  down.
- **Health**: the master's own status, every worker registered.
- **Delete**: every VM, volume, DNS record, the access key and the secrets.
- **Console**: the Spark pages, with the connection details and a
  `spark-submit` example.

Not in v1 (§9): a job ("step") API through Kumo, Spark Connect, notebooks,
autoscaling, more than one node group, upgrades in place, YARN or HDFS.

### Decisions this version fixes

| # | Decision | Why |
|---|---|---|
| S1 | **Spark standalone on VMs** (approved, option A) | What someone asking for a Spark cluster expects, with one service to run. Spark on a Kumo Kubernetes cluster stays possible later (§9) |
| S2 | **Spark 4.3** (approved) | Released 2026-10, supported to 2028-04. One minor per image (§7); 4.5 is the next long-term line and would be a new image beside it |
| S3 | **No in-guest agent, no SSH** | As R3 and K3. kumod observes the cluster through the master's own status endpoint (§6) and drives scale-down through the VM's ACPI shutdown (§4) |
| S4 | **Nodes are hidden rows in `instances`, owned by `spk`** | As R7 and K8, through the managed-VM seam. Capacity, addressing, rollback and reconciliation stay single-sourced |
| S5 | **The shared cluster machinery is extracted first** (approved) | M8 wrote the generic half of "run N VMs as one cluster" inside `internal/service/kubernetes`. M9.0 moves it to `internal/service/managedcluster`, unchanged in behavior, and both modules use it (§10) |
| S6 | **VPC-internal only, submission included** (approved) | As R6 and K7: no security groups, so no Spark port could be made safe outside the VPC. The REST submission server stays off |
| S7 | **The endpoint is a DNS name** | `<cluster>.<vpc>`, served by the VPC's dnsmasq through `hypervisor.AddDNSHost`, as K6 |
| S8 | **Authentication and encryption on** | `spark.authenticate` with a per-cluster 256-bit secret and `spark.network.crypto.enabled`, so a VPC host without the secret can neither submit nor join as a worker (§5) |
| S9 | **Object storage through a per-cluster access key** (approved) | A RustFS user of its own (`06-object-storage.md` §5), read-write on the buckets chosen at create, deleted with the cluster |
| S10 | **History server on the master, event logs in a bucket** (approved) | The logs are objects, so they survive the cluster. Event logs are written by each job's **driver**, which runs on the submitting instance or a worker and never on the master, so there is no local fallback: a cluster without an event-log bucket has event logs off and no history server, and the console says why (revised 2026-11-21, M9.2) |
| S11 | **IDs `spk-` and `ng-`; API under `/api/v1/spark`** (approved) | `spk-` + 17 hex for clusters; node groups reuse `ng-`, in their own table |
| S12 | **Kumo keeps the cluster's access key secret, and admins get it in the client configuration** (approved; revised 2026-11-21, option A) | A job's S3 settings travel from its driver to its executors, and the driver runs where `spark-submit` runs, so the settings must be on the submitting instance, not in the workers' seeds. Connect hands an admin a client `spark-defaults.conf` carrying them (§5), so Kumo keeps the secret, 0600, beside the Spark secret, as K4 keeps a cluster CA. Of the nodes, only the master's history server holds it |
| S13 | **Decommission by ACPI shutdown, not by the master's UI** (approved) | The master's decommission endpoint has no authentication; opening it would let any VPC host drain workers. Instead the worker's systemd unit stops Spark with `SIGPWR`, which Spark treats as "decommission", so kumod's ordinary graceful shutdown drains the worker (§4) |

## 1. Shape

```
                     ┌────────────── VPC (one libvirt network) ───────────────┐
 instance in the VPC │                                                        │
  spark-submit ──────┼──▶ <cluster>.<vpc>:7077   master VM (spk-…-master)     │
  browser     ───────┼──▶ :8080 master UI         spark master                 │
                     │   :18080 history server    spark history server         │
                     │                                 ▲                      │
                     │        worker VM … worker VM ───┘ (register, auth)     │
                     │          executors ──────────────────────┐             │
                     └──────────────────────────────────────────┼─────────────┘
 kumod (host, the VPC's gateway)                                │ s3a://
   ├── GET http://<master>:8080/json/  health (§6)              ▼
   ├── libvirt: VMs, DNS record, ACPI shutdown         RustFS (S3 port on the host)
   └── RustFS admin: the cluster's access key
```

- One master VM: the Spark master, the history server, nothing else. Jobs run
  on the workers (cluster deploy mode) or on the submitting instance (client
  mode).
- Each worker offers its VM's vCPUs and its memory less 1 GiB for the OS
  (`SPARK_WORKER_CORES`, `SPARK_WORKER_MEMORY`), so an instance type is the
  capacity a user sees.
- The workers' local disk is shuffle and spill space only; data lives in
  object storage.

## 2. Configuration

| Variable | Default | Meaning |
|---|---|---|
| `KUMO_SPARK` | `off` | the module, off by default while it is a preview (until the M9.8 real-node run passes); off makes every `/spark` route answer `503` |
| `KUMO_SPARK_READY_TIMEOUT` | `10m` | how long a create waits for the master and every worker |
| `KUMO_SPARK_PROBE_INTERVAL` | `30s` | the health pass's interval per cluster |
| `KUMO_SPARK_DRAIN_TIMEOUT` | `10m` | how long a scale-down lets one worker decommission (§4) |
| `KUMO_SPARK_S3_ENDPOINT` | `$KUMO_S3_PUBLIC_URL` | where the **guests** reach RustFS's S3 port. It must be an address a VPC can route to: the host's LAN address works on a NAT VPC, nothing does on an isolated one (§4 refuses that combination) |

`/system/info` gains `spark {enabled, versions, object_storage}`; `kumod
doctor` warns with the module on and no image, and when object storage is on
and the guests' S3 endpoint is a loopback address.

## 3. Data model

Migration `0009_spark.sql`, following `0008_kubernetes.sql`:

- `spark_clusters`: `id` (`spk-`), `name` (unique, a DNS label of 1-40),
  `spark_version`, `image_id`, `state`, `state_reason`, `vpc_id`, `subnet_id`,
  `master_type`, `endpoint`, `object_storage` (0/1), `access_key_id` (the
  RustFS user; set exactly with object storage, by CHECK),
  `event_log_bucket` (nullable; only with object storage, by CHECK),
  `created_by`, timestamps.
- `spark_cluster_buckets`: `cluster_id`, `bucket` -- the buckets the access
  key is granted, read-write.
- `spark_node_groups`: `id` (`ng-`), `cluster_id`, `name`, `instance_type`,
  `root_disk_gb`, `desired_size`.
- `spark_nodes`: `instance_id` (the key), `cluster_id`, `node_group_id`,
  `role` (`master` | `worker`); the master belongs to no node group.
- `instances.managed_by` accepts `spk`; the image import accepts engine
  `spark`, versioned by its minor (`4.3`).

The tables are `spark_*` rather than `spk_*`: sqlc names its Go types after
them (`02-data-model.md`).
- `instances.managed_by` accepts `spk`.

States: `creating`, `active`, `updating`, `degraded`, `deleting`, `failed`,
with the meanings of `08-kubernetes.md` §4. Secrets are never columns (§5).

## 4. Lifecycle

### Create (task `spark.create`)

```
refuse    name taken; no image; types under the minimums (master 2 vCPU and
          4 GiB, workers 2 GiB); object storage asked for and off, or the
          guests' S3 endpoint unroutable from this VPC; a bucket that does not
          exist; the node cannot hold 1 + N VMs
prepare   the Spark secret; with object storage, the access key (RustFS user
          and policy) and its secret; both written to
          $KUMO_DATA_DIR/spark/<id>/ (0700 dir, 0600 files)
reserve   in 1 txn, managedcluster: capacity and addresses for 1 + N VMs, the
          cluster, its node group, its nodes and the task
1. the master: seed (spark-defaults with the secret; with an event-log
   bucket, the S3 settings and the event-log location for the history
   server; the units for master, and history server if there is a bucket),
   root overlay, domain
2. the DNS record <cluster>.<vpc> -> the master's address
3. wait for the master's /json/ to answer with status ALIVE
4. the workers: seed (the master URL and the same secret; no S3 settings,
   which reach executors from their driver), overlay, domain each
5. wait until every expected worker is registered ALIVE
6. in 1 txn: instances -> running, cluster -> active, endpoint written;
   degraded with the missing workers named if step 5 timed out
```

On failure before the master answers the cluster is `failed`, its VMs kept for
their console logs, as M8 does. Every step adopts what an earlier attempt left,
so a requeued task completes rather than duplicates.

### Scale (task `spark.scale`)

- **Up** launches workers with the same seed recipe; the secret comes from
  the cluster's directory.
- **Down**, newest first, one worker at a time (S13):
  1. a graceful ACPI shutdown of the worker's VM, through the hypervisor
     (not an instance stop task, whose own timeout would cut a drain short);
  2. systemd stops `spark-worker`, and its `ExecStop`
     (`/usr/lib/kumo/spark-worker-drain`, packaging/images/spark-4.3) sends the
     worker `SIGPWR`: Spark decommissions it, moving its cached and shuffle
     blocks (`spark.storage.decommission.*`) and letting running tasks
     finish. The script returns as soon as the worker's own status shows no
     executor and no driver left, or at `KUMO_SPARK_DRAIN_TIMEOUT` (default
     10 m): a decommissioned worker does not exit by itself, so the signal
     alone would always wait out the timeout;
  3. the VM powers off; kumod tears it down and deletes its rows;
  4. a VM still running at the drain timeout plus a minute is forced off and
     removed all the same -- the size asked for is reached -- and the cluster
     ends `degraded`, naming every worker that was forced, because the work
     on it was lost and re-run elsewhere.

M9.2's boot test showed this on the image: an ACPI shutdown of a worker
holding a job's cached data decommissions it, the driver logs it so, and the
VM is off in seconds with the job's result unchanged.

### Delete (task `spark.delete`)

Every VM and volume (the managed-cluster teardown, which tolerates what is
already gone), the DNS record, the access key (user first, then policy, as
`06-object-storage.md` §5), then `$KUMO_DATA_DIR/spark/<id>/`. **The buckets
and the event logs in them are the user's and are kept.**

## 5. Secrets

| Secret | Where it lives | Who sees it |
|---|---|---|
| Spark auth secret (256-bit, per cluster) | `$KUMO_DATA_DIR/spark/<id>/spark.secret`, 0600; every node's seed | An admin, in the client configuration; never a viewer, never a log line, never a task payload |
| Access key secret (S12) | `$KUMO_DATA_DIR/spark/<id>/s3.secret`, 0600; the master's seed when there is an event-log bucket (the history server reads the logs) | An admin, in the client configuration |

**The client configuration** is what makes `s3a://` work with nothing to
configure. A job's settings, its S3 endpoint and credentials included, travel
from its driver to its executors; the driver runs where `spark-submit` runs (a
VPC instance in client mode, a worker in cluster mode, still with the
submitter's settings), so the settings belong to the submitter. Connect hands
an admin a `spark-defaults.conf`, minted on demand with `Cache-Control:
no-store` like a kubeconfig, carrying:

- the master URL, `spark.authenticate` and its secret, `spark.network.crypto.enabled`;
- the decommissioning settings, which are a job's (`spark.decommission.enabled`,
  `spark.storage.decommission.*`);
- with object storage, `spark.hadoop.fs.s3a.*`: the guests' S3 endpoint, path-style
  access, the cluster's access key and secret;
- with an event-log bucket, `spark.eventLog.enabled` and `spark.eventLog.dir`.

The user runs `spark-submit --properties-file kumo-<cluster>.conf --conf
spark.driver.host=$(hostname -I | cut -d' ' -f1) …`, as the file's header and
the console's example say. The executors connect back to the driver, and by
default Spark advertises the driver's **hostname**, which a worker resolves only
if the VPC's DNS happens to know it; the address always works (M9.2's boot test
found executors dying on an unresolvable driver host). The key
it carries is the cluster's own, scoped to the cluster's buckets and deleted
with it, so a leaked file reaches those buckets until the cluster goes.
- **Seeds are shredded** after first boot, as the Kubernetes seeds are, so the
  secrets do not linger in `/var/lib/cloud`.
- The web UIs (master `:8080`, workers `:8081`, history `:18080`) have no
  authentication in Spark; VPC-only is their boundary, and the kill links are
  off (`spark.ui.killEnabled=false`).
- `spark.master.rest.enabled=false`, explicitly: the REST submission server
  is the historical attack surface of a standalone master.

## 6. Readiness and health

kumod reads `GET http://<master>:8080/json/`, Spark's own JSON view of the
master: `status` and the `workers` with their state. No client library and no
credential: it is a read of what any VPC host can read. Parsed with
`encoding/json` in `internal/sparkapi` (M9.4). The trailing slash matters: Spark 4.3 answers `/json`
with a `301` to `/json/` (found by M9.1's verify), and a client that does not
follow redirects would read nothing.

The health pass, on the shared strike logic (S5): a master that does not
answer, or a worker that is not `ALIVE`, is a strike; three in a row make the
cluster `degraded` with the reason, and one good pass makes it `active` again.

## 7. The golden image

`packaging/images/spark-4.3/`, on the shared build (`packaging/images/lib/`)
and published gzipped by `engine-images.yml` as `spark-4.3-<date>`:

- Ubuntu 26.04, OpenJDK 21, Python 3 with the PySpark of the same release;
- Spark 4.3.0 (`bin-hadoop3`) from the Apache archive, its SHA-512 pinned;
- `hadoop-aws` and the AWS SDK bundle of the matching Hadoop version, pinned,
  so `s3a://` needs no download at run time;
- systemd units for the master, the worker and the history server, enabled by
  the seed for the node's role only;
- `verify.sh`: boots the image as master and worker in one VM, runs SparkPi
  and a PySpark job with authentication and encryption on, and checks the
  history server lists them.

Expected about 1.2 GB gzipped, under GitHub's 2 GiB asset limit.

## 8. API (`/api/v1/spark`)

| Method and path | Who | What |
|---|---|---|
| `GET /spark/clusters` | any | list, paged |
| `POST /spark/clusters` | admin | create; `202` with the cluster `creating` |
| `GET /spark/clusters/{id}` | any | detail with node group, nodes, endpoint, UI addresses |
| `PATCH /spark/clusters/{id}/node-groups/{ng}` | admin | resize; `202`, or `200` unchanged |
| `POST /spark/clusters/{id}/client-config` | admin | the client `spark-defaults.conf` (§5), `no-store` |
| `DELETE /spark/clusters/{id}` | admin | delete; `202` |

The create body: `name`, `spark_version` (optional, newest), `subnet_id`
(optional, default subnet), `master_type`, `worker_type`, `worker_count`,
`object_storage` (bool), `buckets` (names), `event_log_bucket` (one of them).

## 9. What this leaves room for

- **A job API** (EMR "steps") through Kumo, submitting to the master's REST
  server signed with Spark 4's `JWSFilter`, if submission from outside the VPC
  is wanted later.
- **Spark Connect** through an authenticated kumod proxy, for notebooks.
- **The UIs through kumod's authenticated proxy**, as VM consoles are.
- **Spark on a Kumo Kubernetes cluster** (option B), as a second mode.
- **A node-wide history server**, so history stays viewable after its cluster
  is deleted.
- **Autoscaling** from the master's pending-app count.

## 10. The extraction (M9.0)

**Done.** `internal/service/managedcluster` holds what M8 had written inside
`internal/service/kubernetes` and does not depend on what runs in the VMs:

- `Node`, `Seed` (a node's cloud-init around the module's user data),
  `WorkerName`, `WorkerIndex`, `HighestIndex`, `SortedByIndex` and `Surplus`
  (the workers a scale-down removes, newest first);
- `Payload` and `DecodePayload`, the task input of every cluster task;
- `Wait`, the bounded poll that ends with the module's cancellation error when
  a delete takes the cluster over, or with `ErrTimedOut`;
- `PublishEndpoint` and `RemoveEndpoint`, the DNS record, idempotent both ways;
- `Teardown`, every node's VM through the module's tolerant teardown;
- `Health` (probe scheduling, unhealthy passes in a row, each node's health as
  last seen) and `Judge`, the active/degraded decision of a pass.

What stays in each module: its tables and queries (so its `layout` and
`setState`), its seeds and therefore its launch loops (each launch renders the
module's seed), its API client, its readiness predicate and its drain. The
reservation of 1 + N VMs was already shared, in `instance.WithManagedReservations`.

Verified as the AC asks: the Kubernetes unit tests pass unchanged, and
`make test-k8s-journey` passes on a real node with the refactored module.

## 11. Owner decisions

Answered 2026-11-20:

| # | Question | Answer |
|---|---|---|
| 1 | Standalone on VMs (A) or Spark on Kubernetes (B)? | **A** (S1) |
| 2 | Spark version | **4.3** (S2) |
| 3 | Integrate with Kumo object storage? | **Yes** (S9) |
| 4 | Job submission | **From inside the VPC only** (S6) |
| 5 | History server | **Yes** (S10) |
| 6 | Naming | **`spk-`**, API `/api/v1/spark` (S11) |
| 7 | Shared cluster code: extract or copy? | **Extract first** (S5, §10) |

| S12 | May Kumo keep each cluster's access key secret, 0600, beside the Spark secret? | **Yes**; and (2026-11-21) admins get it in the client configuration, option A of the M9.2 review |
| S13 | Decommission through the VM's ACPI shutdown (`SIGPWR`) rather than the master's unauthenticated endpoint? | **Yes**; M9.2 verifies `SIGPWR` decommissions on the 4.3 image before M9.6 relies on it |

## 12. Milestone (M9)

One task per PR, in order (acceptance criteria in `05-roadmap.md`):

- **M9.0 Extract the managed-cluster machinery** (§10), no behavior change.
- **M9.1 Golden image** — `packaging/images/spark-4.3/` and `verify.sh`, in
  the engine-images workflow.
- **M9.2 Seeds and secrets** — `internal/service/sparkcloudinit`; verified by
  booting the image as a master and a worker (`sparkimage` build tag): the
  worker registers, a job runs with auth and encryption on, a submit without
  the secret is refused, and `SIGPWR` decommissions a worker holding blocks.
- **M9.3 Data model** — `0009_spark.sql`, `managed_by='spk'`, `spk-` in
  `internal/id`.
- **M9.4 Master client** — `internal/sparkapi`, the `/json/` reader, against
  `httptest` servers that answer like a master.
- **M9.5 Clusters API and tasks** — create, delete, health, the access key
  lifecycle, the client configuration, `/system/info`, `kumod doctor`,
  `openapi.yaml`, `01-api.md`.
- **M9.6 Scale** — up, and down by ACPI shutdown.
- **M9.7 Console** — the Spark pages.
- **M9.8 End to end** — on a real node with RustFS: create a cluster with a
  bucket; from the VPC, a PySpark job reads from and writes to it; the history
  server lists the job; scale 2 → 3 → 1 with a job running; delete, with every
  VM, volume, the DNS record, the access key and the secrets gone and the
  bucket kept. Playwright on the fake for the console journey.
