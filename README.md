# Kumo

> Your own mini-AWS, running on your own hardware, in about 15 minutes.

Kumo is an open-source, AWS-style private cloud for teams that find OpenStack too heavy and Proxmox too low-level. One Go binary, one web console, one SQLite file. No message bus cluster, no dozen microservices.

> **Status:** early design phase. Nothing here is production-ready yet. See the [roadmap](docs/design/05-roadmap.md).
> "Kumo" (雲, "cloud") is a working name.

## Why Kumo?

| | OpenStack | Proxmox | **Kumo** |
|---|---|---|---|
| Install time | Days | Hours | **~15 min** |
| Mental model | Many services | Hypervisor admin | **AWS-like (instances, images, key pairs, ...)** |
| Moving parts | 10+ services + MQ + DB | Monolith | **1 binary + SQLite + libvirt** |
| Target | Large operators | Sysadmins | **Small/mid teams** |

## Planned services

| Kumo service | AWS analogue | Backed by | Status |
|---|---|---|---|
| Instances | EC2 | KVM / libvirt | 🚧 MVP (in design) |
| Images | AMI | qcow2 cloud images | 🚧 MVP |
| Key Pairs | EC2 Key Pairs | SSH keys | 🚧 MVP |
| Object Storage | S3 | RustFS | 📝 planned |
| Databases | RDS | PostgreSQL, MySQL | 📝 planned |
| Cache | ElastiCache | Redis | 📝 planned |
| Messaging / Streaming | SQS / SNS / Kinesis | ActiveMQ, NATS | 📝 planned |
| Secrets | Secrets Manager | OpenBao | 📝 planned |
| Kubernetes | EKS | k3s / kubeadm | 📝 planned |

The first milestone covers only the **EC2-like trio: Instances, Images, Key Pairs**.

## Tech stack

- **Frontend:** React, Redux Toolkit (RTK Query), Vite, TypeScript, [Cloudscape Design System](https://cloudscape.design)
- **Backend:** Go, Gin, [digitalocean/go-libvirt](https://github.com/digitalocean/go-libvirt), SQLite, JWT
- **Hypervisor:** KVM via libvirt (QEMU)

## Requirements

- Linux host with hardware virtualization (Intel VT-x / AMD-V; check `egrep -c '(vmx|svm)' /proc/cpuinfo`)
- `libvirtd` + QEMU/KVM installed and running
- Go 1.23+ and Node.js 20+ (development only)

## Quick start (target experience)

```bash
curl -fsSL https://get.kumo.example/install.sh | sudo bash
# open https://<host>:8443  -> create the admin account -> launch your first instance
```

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

## Repository layout

```
cmd/kumod/        Backend entrypoint
internal/         Backend packages (api, auth, service, hypervisor, store, ...)
web/              React console
docs/design/      Architecture and design docs (start here)
AGENTS.md         Instructions for AI coding agents
```

## Documentation

1. [Architecture](docs/design/00-architecture.md)
2. [REST API](docs/design/01-api.md)
3. [Data model](docs/design/02-data-model.md)
4. [Frontend](docs/design/03-frontend.md)
5. [libvirt integration](docs/design/04-libvirt-integration.md)
6. [Roadmap & milestones](docs/design/05-roadmap.md)

## Contributing

Issues and PRs are welcome once the scaffold lands. Read `AGENTS.md` for conventions (they apply to humans too).

## License

Apache-2.0
