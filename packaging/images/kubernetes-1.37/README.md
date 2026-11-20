# Kubernetes 1.37 golden image

The recipe for the image every node of a Kumo Kubernetes cluster boots from,
control plane and worker alike; the seed kumod renders decides which. Built
**once, outside Kumo**, published gzipped with its SHA256, and imported into a
node with `kind=engine`, `engine=kubernetes`, `engine_version=1.37`. See
[`docs/design/08-kubernetes.md`](../../../docs/design/08-kubernetes.md) §7 for
why it works this way, and [`../README.md`](../README.md) for the build every
recipe shares and how images are published.

## What is in it

| Component | Version | From |
|---|---|---|
| Ubuntu | 26.04 LTS, one kernel, no headers, no snapd | `cloud-images.ubuntu.com/resolute` |
| CRI-O | 1.37.4, held | `isv:cri-o` on the openSUSE Build Service, key pinned in `files/` |
| kubeadm, kubelet, kubectl | 1.37.2, held | `pkgs.k8s.io`, key pinned in `files/` |
| Control-plane images, CoreDNS, pause | as kubeadm 1.37.2 names them | pre-pulled |
| Cilium (agent, operator, Envoy) | 1.20.5 | `files/cilium.yaml`, pre-pulled by digest |
| local-path-provisioner, busybox helper | 0.0.37, 1.38.0 | `files/local-path-storage.yaml`, pre-pulled by digest |

The manifests are installed at `/usr/share/kumo/kubernetes/`, where a control
plane's seed applies them after `kubeadm init`. Nothing in the image belongs to
a cluster: no PKI, no kubeconfig, no token. `/etc/kumo-image` records the exact
versions a build contains.

## Build

```sh
./build.sh                      # writes out/kumo-kubernetes-1.37-<UTC date>.qcow2
./verify.sh --image out/kumo-kubernetes-1.37-<date>.qcow2
```

`preflight.sh` runs first and refuses to build if either pinned apt key has
expired (it warns a month ahead). Both projects extend their keys from time to
time; refresh the copy in `files/` from the repository's `Release.key` after
checking the fingerprint in `preflight.sh`'s header.

## Verify

`verify.sh` boots the image twice, as a control plane and a worker, from the
hand-written seeds `verify-controlplane.cloud-config.yaml` and
`verify-worker.cloud-config.yaml`. The two VMs share one private link and
nothing else -- no NAT, no gateway that exists, no DNS server that answers --
so any image pull fails. The control plane then checks and reports:

- `kubeadm init` succeeds and the worker joins; both nodes `Ready`
- Cilium, CoreDNS and local-path roll out from the image's own manifests
- a PersistentVolumeClaim binds on the default StorageClass and is written
- a pod on the control plane pings a pod on the worker, across the VXLAN tunnel
- no image-pull event happened anywhere in the cluster

and `verify.sh` checks that booting did not modify the image. The seeds let
kubeadm make its own PKI and join with a fixed token and no CA pinning: they
prove the image. The real seeds, with the PKI kumod generates, are M8.2's.

## Bumping a version

- **Kubernetes or CRI-O patch:** `K8S_PACKAGE` / `CRIO_PACKAGE` in
  `build.cloud-config.yaml`. A new minor is a new recipe directory.
- **Cilium:** `CILIUM_VERSION` and `CHART_SHA256` in `render-cilium.sh` (the
  digest is in `https://helm.cilium.io/index.yaml`), then run it. It needs Helm
  and rewrites `files/cilium.yaml` and the Cilium lines of `files/images.txt`.
- **local-path-provisioner:** re-derive `files/local-path-storage.yaml` as its
  header describes, and update both digests there and in `files/images.txt`.
