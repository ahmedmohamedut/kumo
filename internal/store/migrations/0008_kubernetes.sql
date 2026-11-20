-- +goose Up
-- Kubernetes clusters (docs/design/08-kubernetes.md section 3).
--
-- A cluster's nodes are rows in instances with managed_by = 'eks', hidden from
-- the Instances pages and counted against the node's capacity exactly as a DB
-- instance's VM is (K8). instances.managed_by has no CHECK constraint, so the
-- new value needs no change to that table.

CREATE TABLE k8s_clusters (
  id                  TEXT PRIMARY KEY,           -- eks-<17 hex>
  name                TEXT NOT NULL,              -- DNS label; the endpoint is api.<name>.<vpc>
  kubernetes_version  TEXT NOT NULL,              -- '1.37', matched against images.engine_version
  image_id            TEXT NOT NULL REFERENCES images(id),
  vpc_id              TEXT NOT NULL REFERENCES vpcs(id),
  subnet_id           TEXT NOT NULL REFERENCES subnets(id),
  pod_cidr            TEXT NOT NULL,
  service_cidr        TEXT NOT NULL,
  control_plane_count INTEGER NOT NULL DEFAULT 1 CHECK (control_plane_count IN (1, 3)),
  control_plane_type  TEXT NOT NULL REFERENCES instance_types(id),
  state               TEXT NOT NULL CHECK (state IN
                       ('creating','active','updating','degraded','deleting','failed')),
  state_reason        TEXT,
  endpoint            TEXT,                       -- https://api.<name>.<vpc>:6443, set once active
  -- The CA certificate is public, and every kubeconfig embeds it. The keys are
  -- not here: they live in $KUMO_DATA_DIR/clusters/<id>/ (section 5), so a copy
  -- of kumo.db is never a copy of a cluster's CA.
  ca_cert_pem         TEXT NOT NULL,
  created_by          TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_k8s_clusters_name ON k8s_clusters(name);

CREATE TABLE k8s_node_groups (
  id            TEXT PRIMARY KEY,                 -- ng-<17 hex>
  cluster_id    TEXT NOT NULL REFERENCES k8s_clusters(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,                    -- 'default' in v1
  instance_type TEXT NOT NULL REFERENCES instance_types(id),
  root_disk_gb  INTEGER NOT NULL,
  desired_size  INTEGER NOT NULL CHECK (desired_size BETWEEN 1 AND 10),
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_k8s_node_groups_name ON k8s_node_groups(cluster_id, name);

-- Which instances are a cluster's nodes, and in which role. A node row goes
-- with its instance: the delete flow removes the two in one transaction, as a
-- DB instance and its VM are removed.
CREATE TABLE k8s_nodes (
  instance_id   TEXT PRIMARY KEY REFERENCES instances(id),
  cluster_id    TEXT NOT NULL REFERENCES k8s_clusters(id) ON DELETE CASCADE,
  node_group_id TEXT REFERENCES k8s_node_groups(id) ON DELETE CASCADE,  -- NULL for a control plane
  role          TEXT NOT NULL CHECK (role IN ('control-plane', 'worker')),
  -- A control plane belongs to no node group; a worker always does.
  CHECK ((role = 'control-plane') = (node_group_id IS NULL))
);
CREATE INDEX idx_k8s_nodes_cluster ON k8s_nodes(cluster_id);

-- +goose Down
DROP TABLE k8s_nodes;
DROP TABLE k8s_node_groups;
DROP TABLE k8s_clusters;
