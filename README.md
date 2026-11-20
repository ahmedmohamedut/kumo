# Kumo

> Your own mini-AWS, running on your own hardware, in about 15 minutes.

Kumo is an open-source, AWS-style private cloud for teams that find OpenStack too heavy and Proxmox too low-level. One Go binary, one web console, one SQLite file. No message bus cluster, no dozen microservices.

> **Status:** v0.3.0, early. Compute, networking, object storage and managed Postgres work on a single node; managed Kubernetes clusters are built on `main` and ship in the next release. There is no TLS yet, so keep it on a network you trust. See the [roadmap](docs/design/05-roadmap.md) and the [install guide](docs/install.md).
> "Kumo" (雲, "cloud") is a working name.

## Why Kumo?

| | OpenStack | Proxmox | **Kumo** |
|---|---|---|---|
| Install time | Days | Hours | **~15 min** |
| Mental model | Many services | Hypervisor admin | **AWS-like (instances, images, key pairs, ...)** |
| Moving parts | 10+ services + MQ + DB | Monolith | **1 binary + SQLite + libvirt** |
| Target | Large operators | Sysadmins | **Small/mid teams** |

## Services

| Kumo service | AWS analogue | Backed by | Status |
|---|---|---|---|
| Instances, instance types | EC2 | KVM / libvirt | ✅ v0.1 |
| Images | AMI | qcow2 cloud images | ✅ v0.1 |
| Key Pairs | EC2 Key Pairs | SSH keys | ✅ v0.1 |
| VPCs, subnets | VPC | libvirt networks | ✅ v0.1 |
| Object Storage (buckets, objects, access keys) | S3, IAM access keys | [RustFS](https://rustfs.com), optional | ✅ v0.2 |
| Databases | RDS | PostgreSQL 18, on a golden image | ✅ v0.3 |
| Cache | ElastiCache | Redis | 📝 planned |
| Messaging / Streaming | SQS / SNS / Kinesis | ActiveMQ, NATS | 📝 planned |
| Secrets | Secrets Manager | OpenBao | 📝 planned |
| Kubernetes clusters | EKS | kubeadm 1.37, CRI-O, Cilium, on a golden image | ✅ on `main`, next release |

Object storage is optional: Kumo manages a RustFS server you run, and stays a single binary with no outside services until you turn it on. The bytes of uploads and downloads go between the browser and RustFS directly; Kumo hands out short-lived signed links. See [object storage in the install guide](docs/install.md#object-storage-optional).

Databases and Kubernetes clusters run on virtual machines Kumo manages for you, booted from **engine images**: golden qcow2 images the project builds and publishes as GitHub releases (`postgres-18-<date>`, `kubernetes-1.37-<date>`, gzipped). Import one once as an engine image and the service is ready; until then its pages say what to import, and `kumod doctor` warns. Both are reachable from inside their VPC only. A cluster's nodes need no registry: every image a cluster starts with is already in the engine image. The Kubernetes console downloads a kubeconfig minted for you, as an admin (`cluster-admin`) or a viewer (read-only), valid 30 days.

## Tech stack

- **Frontend:** React, Redux Toolkit (RTK Query), Vite, TypeScript, [Cloudscape Design System](https://cloudscape.design)
- **Backend:** Go, Gin, [digitalocean/go-libvirt](https://github.com/digitalocean/go-libvirt), SQLite, JWT, [aws-sdk-go-v2](https://github.com/aws/aws-sdk-go-v2) (S3 client)
- **Hypervisor:** KVM via libvirt (QEMU)
- **Object storage:** [RustFS](https://rustfs.com) (S3-compatible), optional
- **Databases:** PostgreSQL 18 · **Kubernetes:** kubeadm 1.37, CRI-O, Cilium, local-path storage, all on Ubuntu golden images (`packaging/images/`)

## Requirements

- Linux host (x86-64 or arm64) with hardware virtualization (Intel VT-x / AMD-V; check `grep -cE '(vmx|svm)' /proc/cpuinfo`)
- `libvirtd` + QEMU/KVM installed and running
- About 20 GiB free under `/var/lib` for images and root disks, more for databases and clusters (each cluster node is a VM: a 3-node cluster of `kumo.medium` uses 12 GiB of memory)
- **For object storage (optional):** RustFS 1.0.4 or later, whose S3 port (default `:9000`) kumod and the console's browsers can reach, its admin keys, and clocks in sync (NTP). Setup: [install guide](docs/install.md#object-storage-optional)
- Go 1.26+ and Node.js 22+ (development only)

## Quick start

```bash
curl -fsSL https://raw.githubusercontent.com/ahmedmohamedut/kumo/main/scripts/install.sh | sudo bash
sudo -u kumo kumod doctor          # check the host
sudo systemctl enable --now kumod
# open http://<host>:8080 -> create the admin account -> launch your first instance
```

The script installs the `.deb` or `.rpm` of the latest release after checking its checksum. [`docs/install.md`](docs/install.md) is the full 15-minute path, including turning on object storage.

## Development

```bash
git clone https://github.com/<you>/kumo && cd kumo
make deps        # install Go + npm dependencies
make dev         # backend on :8080 (hot reload) + Vite on :5173 (proxying /api)
make test        # backend + frontend tests
make build       # single binary with embedded web console -> bin/kumod
```

Developing without KVM (macOS, CI)? Run the backend with the in-memory fake hypervisor:

```bash
KUMO_HYPERVISOR=fake make dev
```

No RustFS either? `KUMO_OBJECTSTORE=fake` adds an in-memory object store, so the Storage pages work too. On the fake hypervisor the Databases and Kubernetes pages work as well, with a probe and a cluster API that answer as healthy ones would. With a RustFS at hand, `make test-rustfs` runs the object store tests against it (`KUMO_TEST_S3_ENDPOINT`, `KUMO_TEST_S3_ACCESS_KEY`, `KUMO_TEST_S3_SECRET_KEY`).

## Repository layout

```
cmd/kumod/        Backend entrypoint
internal/         Backend packages (api, auth, service, hypervisor, objectstore, store, ...)
web/              React console
docs/design/      Architecture and design docs (start here)
AGENTS.md         Instructions for AI coding agents
```

## Documentation

- [Installing Kumo](docs/install.md) (including RustFS for object storage)

Design:

1. [Architecture](docs/design/00-architecture.md)
2. [REST API](docs/design/01-api.md)
3. [Data model](docs/design/02-data-model.md)
4. [Frontend](docs/design/03-frontend.md)
5. [libvirt integration](docs/design/04-libvirt-integration.md)
6. [Roadmap & milestones](docs/design/05-roadmap.md)
7. [Object storage](docs/design/06-object-storage.md)
8. [Databases](docs/design/07-databases.md)
9. [Kubernetes clusters](docs/design/08-kubernetes.md)

## Contributing

Issues and PRs are welcome. Read `AGENTS.md` for conventions (they apply to humans too).

## License

Apache-2.0
