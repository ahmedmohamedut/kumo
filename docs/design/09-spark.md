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

**Proposed 2026-11-20; awaiting the owner's approval.** Nothing is built.
The owner's answers that shape it are in §11; two points this design adds on
top of them need a yes or a no (§11, S12 and S13).

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
| S10 | **History server on the master, event logs in a bucket** (approved) | The logs are objects, so they survive the cluster; with object storage off they go to the master's disk and go with it |
| S11 | **IDs `spk-` and `ng-`; API under `/api/v1/spark`** (approved) | `spk-` + 17 hex for clusters; node groups reuse `ng-`, in their own table |
| S12 | **Kumo keeps the cluster's access key secret** (to approve) | A worker added by a scale-up needs the S3 credentials in its seed, so the secret cannot be shown once and forgotten as `06-object-storage.md` §6 does for a user's key. It is kept beside the Spark secret, 0600, as K4 keeps a cluster CA (§5) |
| S13 | **Decommission by ACPI shutdown, not by the master's UI** (to approve) | The master's decommission endpoint has no authentication; opening it would let any VPC host drain workers. Instead the worker's systemd unit stops Spark with `SIGPWR`, which Spark treats as "decommission", so kumod's ordinary graceful shutdown drains the worker (§4) |

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
   ├── GET http://<master>:8080/json   health (§6)              ▼
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
| `KUMO_SPARK` | `on` | the module; off makes every `/spark` route answer `503` |
| `KUMO_SPARK_READY_TIMEOUT` | `10m` | how long a create waits for the master and every worker |
| `KUMO_SPARK_PROBE_INTERVAL` | `30s` | the health pass's interval per cluster |
| `KUMO_SPARK_DRAIN_TIMEOUT` | `10m` | how long a scale-down lets one worker decommission (§4) |
| `KUMO_SPARK_S3_ENDPOINT` | `$KUMO_S3_PUBLIC_URL` | where the **guests** reach RustFS's S3 port. It must be an address a VPC can route to: the host's LAN address works on a NAT VPC, nothing does on an isolated one (§4 refuses that combination) |

`/system/info` gains `spark {enabled, versions, object_storage}`; `kumod
doctor` warns with the module on and no image, and when object storage is on
and the guests' S3 endpoint is a loopback address.

## 3. Data model

Migration `0009_spark.sql`, following `0008_kubernetes.sql`:

- `spk_clusters`: `id` (`spk-`), `name` (unique, a DNS label of 1-40),
  `spark_version`, `state`, `state_reason`, `vpc_id`, `subnet_id`,
  `master_type`, `endpoint`, `object_storage` (bool), `access_key_id` (the
  RustFS user, nullable), `event_log_bucket` (nullable), `created_by`,
  timestamps.
- `spk_cluster_buckets`: `cluster_id`, `bucket` — the buckets the access key
  is granted, read-write.
- `spk_node_groups`: `id` (`ng-`), `cluster_id`, `name`, `instance_type`,
  `root_disk_gb`, `desired_size`.
- `spk_nodes`: `cluster_id`, `instance_id`, `name`, `role` (`master` |
  `worker`), `node_group_id`.
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
1. the master: seed (spark-env, spark-defaults with the secret, the S3
   settings and the event-log location, systemd units for master and history
   server), root overlay, domain
2. the DNS record <cluster>.<vpc> -> the master's address
3. wait for the master's /json to answer with status ALIVE
4. the workers: seed (the master URL and the same secret and S3 settings),
   overlay, domain each
5. wait until every expected worker is registered ALIVE
6. in 1 txn: instances -> running, cluster -> active, endpoint written;
   degraded with the missing workers named if step 5 timed out
```

On failure before the master answers the cluster is `failed`, its VMs kept for
their console logs, as M8 does. Every step adopts what an earlier attempt left,
so a requeued task completes rather than duplicates.

### Scale (task `spark.scale`)

- **Up** launches workers with the same seed recipe; the secret and the S3
  credentials come from the cluster's directory (S12).
- **Down**, newest first, one worker at a time (S13):
  1. a graceful ACPI shutdown of the worker's VM;
  2. systemd stops `spark-worker` with `KillSignal=SIGPWR` and a stop timeout
     of `KUMO_SPARK_DRAIN_TIMEOUT` (default 10 m): Spark decommissions the
     worker, moving its cached and shuffle blocks
     (`spark.storage.decommission.*`) and letting running tasks finish;
  3. the VM powers off; kumod tears it down and deletes its rows;
  4. a VM still running at the timeout plus a margin is forced off, the
     cluster is marked `degraded` with the worker named, and the scale stops.

This is the one place where Spark's behavior is load-bearing: M9.2 verifies on
the image that `SIGPWR` decommissions a worker that holds blocks.

### Delete (task `spark.delete`)

Every VM and volume (the managed-cluster teardown, which tolerates what is
already gone), the DNS record, the access key (user first, then policy, as
`06-object-storage.md` §5), then `$KUMO_DATA_DIR/spark/<id>/`. **The buckets
and the event logs in them are the user's and are kept.**

## 5. Secrets

| Secret | Where it lives | Who sees it |
|---|---|---|
| Spark auth secret (256-bit, per cluster) | `$KUMO_DATA_DIR/spark/<id>/spark.secret`, 0600; every node's seed | An admin, from **Connect**, on demand; never a viewer, never a log line, never a task payload |
| Access key secret (S12) | `$KUMO_DATA_DIR/spark/<id>/s3.secret`, 0600; every node's seed | Nobody through the API: jobs get it from `spark-defaults.conf` on the nodes |

- **Why the user needs the Spark secret**: `spark-submit` from a VPC instance
  must present it (`--conf spark.authenticate.secret=…`). The console shows it
  once per request, like a kubeconfig download, with `Cache-Control:
  no-store`.
- **Seeds are shredded** after first boot, as the Kubernetes seeds are, so the
  secrets do not linger in `/var/lib/cloud`.
- The web UIs (master `:8080`, workers `:8081`, history `:18080`) have no
  authentication in Spark; VPC-only is their boundary, and the kill links are
  off (`spark.ui.killEnabled=false`).
- `spark.master.rest.enabled=false`, explicitly: the REST submission server
  is the historical attack surface of a standalone master.

## 6. Readiness and health

kumod reads `GET http://<master>:8080/json`, Spark's own JSON view of the
master: `status` and the `workers` with their state. No client library and no
credential: it is a read of what any VPC host can read. Parsed with
`encoding/json` in `internal/sparkapi` (M9.4).

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
| `POST /spark/clusters/{id}/secret` | admin | the Spark secret, `no-store` |
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

What moves from `internal/service/kubernetes` to
`internal/service/managedcluster`, with Kubernetes unchanged in behavior:

- the multi-VM create skeleton: reserve 1 + N through
  `instance.WithManagedReservations`, launch in order with
  `LaunchManaged`, keep VMs of a failed create for inspection;
- the endpoint DNS record and its removal;
- the bounded wait (`await`) with the deleting check, and `setState`;
- node-group arithmetic: newest-first removal, worker names and indices;
- the teardown of a cluster's VMs, tolerant of what is already gone;
- the health pass's strike counting and probe scheduling.

What stays module-specific: tables and queries, seeds, the API client, the
readiness predicate, the drain. The modules supply those through a small
interface. The acceptance test of M9.0 is that nothing about Kubernetes
changes: its unit tests unchanged, `make test-k8s-journey` passing.

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

To approve:

| # | Question | Proposed |
|---|---|---|
| S12 | May Kumo keep each cluster's access key secret, 0600, beside the Spark secret? | Yes: a scale-up cannot work otherwise |
| S13 | Decommission through the VM's ACPI shutdown (`SIGPWR`) rather than the master's unauthenticated endpoint? | Yes |

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
- **M9.4 Master client** — `internal/sparkapi`, the `/json` reader, against
  `httptest` servers that answer like a master.
- **M9.5 Clusters API and tasks** — create, delete, health, the access key
  lifecycle, the secret endpoint, `/system/info`, `kumod doctor`,
  `openapi.yaml`, `01-api.md`.
- **M9.6 Scale** — up, and down by ACPI shutdown.
- **M9.7 Console** — the Spark pages.
- **M9.8 End to end** — on a real node with RustFS: create a cluster with a
  bucket; from the VPC, a PySpark job reads from and writes to it; the history
  server lists the job; scale 2 → 3 → 1 with a job running; delete, with every
  VM, volume, the DNS record, the access key and the secrets gone and the
  bucket kept. Playwright on the fake for the console journey.
