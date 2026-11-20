package k8scloudinit

// The first-boot scripts. They are constants: everything a request supplies
// reaches the node as a file the seed writes (the kubeadm configuration, the
// PKI, /etc/hosts), so there is no value to quote here and nothing a name or an
// address could break out of.
//
// No line is indented with a tab, so the scripts stay representable as YAML
// literal blocks and read as scripts inside a guest (see dbcloudinit).

// controlPlaneScript is a control plane's first boot (section 4, create steps
// 1 to 3): kubeadm init from kumod's PKI, then the add-ons and the RBAC the
// image and the seed carry. kumod waits for /readyz and the nodes from outside.
const controlPlaneScript = `#!/usr/bin/env bash
# First boot of a Kumo Kubernetes control plane (docs/design/08-kubernetes.md
# section 4). Rendered by internal/service/k8scloudinit; not edited by hand.
set -euo pipefail
export LC_ALL=C
export KUBECONFIG=/etc/kubernetes/admin.conf

RUN=` + runDir + `
MANIFESTS=` + manifestDir + `
LOG=/var/log/kumo-firstboot.log
say() { printf '%s\n' "$*" >/dev/console; }
trap 'tail -20 "$LOG" >/dev/console 2>&1 || true; say ""; say "` + markerFailed + `"' ERR

# 1. kubeadm init, once. The PKI kumod generated is already under
#    /etc/kubernetes/pki, so kubeadm signs the API server, etcd and kubelet
#    certificates with kumod's CAs rather than making its own. The version is
#    the image's own kubeadm, so nothing is pulled. A boot that finds
#    admin.conf has been through here already and goes on to step 2.
if [ ! -f /etc/kubernetes/admin.conf ]; then
  version=$(kubeadm version -o short)
  sed -i "s|` + versionPlaceholder + `|$version|" "$RUN/kubeadm.yaml"
  kubeadm init --config "$RUN/kubeadm.yaml" --skip-token-print >>"$LOG" 2>&1
fi

# 2. The add-ons the image carries, and the bindings Kumo's identities rely
#    on. kubectl apply is idempotent, and is retried while the API server
#    finishes starting.
deadline=$((SECONDS + 600))
until kubectl apply -f "$MANIFESTS/cilium.yaml" -f "$MANIFESTS/local-path-storage.yaml" \
  -f "$RUN/rbac.yaml" >>"$LOG" 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || false
  sleep 5
done

say ""
say "` + markerControlPlaneReady + `"

# 3. The bootstrap token lives in the kubeadm configuration: remove it, and
#    cloud-init's copies of this seed. The CA keys stay under
#    /etc/kubernetes/pki, where a control plane keeps them by design.
rm -f "$RUN/kubeadm.yaml"
find /var/lib/cloud -type f \
  \( -name 'user-data*' -o -name 'vendor-data*' -o -name 'obj.pkl' \) \
  -exec shred -u {} + 2>/dev/null || true
`

// workerScript is a worker's first boot: kubeadm join, pinned to the cluster
// CA, retried until the control plane answers. A create launches workers once
// the API answers, and a scale-up into a running cluster joins at once; the
// retry covers the moments in between.
const workerScript = `#!/usr/bin/env bash
# First boot of a Kumo Kubernetes worker (docs/design/08-kubernetes.md
# section 4). Rendered by internal/service/k8scloudinit; not edited by hand.
set -euo pipefail
export LC_ALL=C

RUN=` + runDir + `
LOG=/var/log/kumo-firstboot.log
say() { printf '%s\n' "$*" >/dev/console; }
trap 'tail -20 "$LOG" >/dev/console 2>&1 || true; say ""; say "` + markerFailed + `"' ERR

# Joined once: a boot that finds kubelet.conf is a node of the cluster
# already.
if [ ! -f /etc/kubernetes/kubelet.conf ]; then
  deadline=$((SECONDS + 1200))
  until kubeadm join --config "$RUN/kubeadm.yaml" >>"$LOG" 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || false
    kubeadm reset -f >>"$LOG" 2>&1 || true
    sleep 15
  done
fi

say ""
say "` + markerJoined + `"

rm -f "$RUN/kubeadm.yaml"
find /var/lib/cloud -type f \
  \( -name 'user-data*' -o -name 'vendor-data*' -o -name 'obj.pkl' \) \
  -exec shred -u {} + 2>/dev/null || true
`

// rbacManifest binds Kumo's groups (K10) and gives kumod's own identity
// exactly the calls of section 6: health, nodes, pods to drain, evictions, and
// creating bootstrap tokens. Nothing else -- in particular no secret reads, no
// writes outside those, and never system:masters.
const rbacManifest = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kumo:admins
  labels:
    app.kubernetes.io/managed-by: kumo
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: kumo:admins
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kumo:viewers
  labels:
    app.kubernetes.io/managed-by: kumo
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: view
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: kumo:viewers
---
# view is namespaced reads only, so a viewer's kubectl get nodes would be
# refused. Nodes are what a viewer most wants to see of a cluster, and reading
# them reveals nothing a viewer cannot already see on the console.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kumo:viewers-nodes
  labels:
    app.kubernetes.io/managed-by: kumo
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kumo:viewers-nodes
  labels:
    app.kubernetes.io/managed-by: kumo
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kumo:viewers-nodes
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: kumo:viewers
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kumo:system
  labels:
    app.kubernetes.io/managed-by: kumo
rules:
  - nonResourceURLs: ["/readyz", "/livez", "/healthz", "/version"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch", "patch", "delete"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods/eviction"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kumo:system
  labels:
    app.kubernetes.io/managed-by: kumo
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kumo:system
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: kumo:system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kumo:system-bootstrap-tokens
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: kumo
rules:
  # A scale-up mints a bootstrap token, which is a Secret in kube-system.
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kumo:system-bootstrap-tokens
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: kumo
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kumo:system-bootstrap-tokens
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: kumo:system
`
