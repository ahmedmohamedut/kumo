# Spark 4.3 golden image

The recipe for the image every node of a Kumo Spark cluster boots from, master
and worker alike; the seed kumod renders decides which. Built **once, outside
Kumo**, published gzipped with its SHA256, and imported into a node with
`kind=engine`, `engine=spark`, `engine_version=4.3`. See
[`docs/design/09-spark.md`](../../../docs/design/09-spark.md) §7 for why it
works this way, and [`../README.md`](../README.md) for the build every recipe
shares and how images are published.

## What is in it

| Component | Version | From |
|---|---|---|
| Ubuntu | 26.04 LTS, one kernel, no headers, no snapd | `cloud-images.ubuntu.com/resolute` |
| OpenJDK | 21, headless | Ubuntu |
| Python | the release's `python3`, for PySpark | Ubuntu |
| Spark, with PySpark | 4.3.0 (`bin-hadoop3`, Hadoop 3.5.0), SHA-512 pinned | `archive.apache.org` |
| `hadoop-aws` | 3.5.0, SHA-512 pinned | Maven Central |
| AWS SDK v2 bundle | 2.35.4, SHA-512 pinned | Maven Central |

Spark is at `/opt/spark` (a link to `/opt/spark-4.3.0`) with the two S3 jars in
`jars/`, so `s3a://` needs no download at run time. SparkR and the YARN and
Kubernetes extras are left out. Three systemd units are installed **disabled**
-- `spark-master`, `spark-worker` and `spark-history` -- and a node's seed
enables the ones for its role after writing `/etc/spark/spark-defaults.conf`
(the secret, encryption, the S3 settings) and `/etc/spark/kumo-node.env`.
Nothing in the image belongs to a cluster. `/etc/kumo-image` records the exact
versions a build contains.

Stopping `spark-worker` drains it: `/usr/lib/kumo/spark-worker-drain` asks
Spark to decommission the worker and waits until no executor or driver is left
on it, bounded by `SPARK_DRAIN_TIMEOUT` (09-spark.md S13). A graceful shutdown
of the VM is therefore a graceful scale-down.

## Build

```sh
./build.sh                      # writes out/kumo-spark-4.3-<UTC date>.qcow2
./verify.sh --image out/kumo-spark-4.3-<date>.qcow2
```

A version bump is an edit to the variables at the top of the provision script
in `build.cloud-config.yaml`: the version, the URL and the SHA-512 of each
download. `hadoop-aws` must be the Hadoop version Spark bundles
(`hadoop.version` in Spark's `spark-parent` POM), and the AWS SDK bundle the one
that Hadoop names (`aws-java-sdk-v2.version` in `hadoop-project`).

## Verify

`verify.sh` boots the image as a master, a worker and a history server in one
VM, with a seed configuring what a cluster's will (`verify.cloud-config.yaml`),
and checks in the guest:

- the master answers and the worker registers `ALIVE`;
- SparkPi and a PySpark job run with authentication and encryption on;
- a client with the wrong secret is refused;
- nothing listens on the REST submission port (6066);
- the history server lists both jobs from their event logs;
- and, from outside, that booting left the image file unchanged.
