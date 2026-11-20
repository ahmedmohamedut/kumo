-- +goose Up
-- Spark clusters (docs/design/09-spark.md section 3).
--
-- A cluster's nodes are rows in instances with managed_by = 'spk', hidden from
-- the Instances pages and counted against the node's capacity as every
-- managed VM is (S4). instances.managed_by has no CHECK constraint, so the new
-- value needs no change to that table.
--
-- No secret is a column: the Spark secret and the access key's secret live in
-- $KUMO_DATA_DIR/spark/<id>/ (section 5), so a copy of kumo.db is never a copy
-- of a cluster's credentials. access_key_id is the key's public id, the RustFS
-- user the delete removes.

CREATE TABLE spark_clusters (
  id               TEXT PRIMARY KEY,              -- spk-<17 hex>
  name             TEXT NOT NULL,                 -- DNS label; the endpoint is <name>.<vpc>
  spark_version    TEXT NOT NULL,                 -- '4.3', matched against images.engine_version
  image_id         TEXT NOT NULL REFERENCES images(id),
  vpc_id           TEXT NOT NULL REFERENCES vpcs(id),
  subnet_id        TEXT NOT NULL REFERENCES subnets(id),
  master_type      TEXT NOT NULL REFERENCES instance_types(id),
  state            TEXT NOT NULL CHECK (state IN
                    ('creating','active','updating','degraded','deleting','failed')),
  state_reason     TEXT,
  endpoint         TEXT,                          -- spark://<name>.<vpc>:7077, set once active
  object_storage   INTEGER NOT NULL DEFAULT 0 CHECK (object_storage IN (0, 1)),
  access_key_id    TEXT,                          -- set exactly when object_storage is on
  event_log_bucket TEXT,                          -- one of the cluster's buckets, or NULL
  created_by       TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL,
  CHECK ((object_storage = 1) = (access_key_id IS NOT NULL)),
  -- An event-log bucket needs object storage: drivers write the logs and
  -- never run on the master, so there is no local fallback (S10).
  CHECK (event_log_bucket IS NULL OR object_storage = 1)
);
CREATE UNIQUE INDEX idx_spark_clusters_name ON spark_clusters(name);

-- The buckets the cluster's access key is granted, read-write (S9). The
-- event-log bucket is one of them.
CREATE TABLE spark_cluster_buckets (
  cluster_id TEXT NOT NULL REFERENCES spark_clusters(id) ON DELETE CASCADE,
  bucket     TEXT NOT NULL,
  PRIMARY KEY (cluster_id, bucket)
);

CREATE TABLE spark_node_groups (
  id            TEXT PRIMARY KEY,                 -- ng-<17 hex>
  cluster_id    TEXT NOT NULL REFERENCES spark_clusters(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,                    -- 'default' in v1
  instance_type TEXT NOT NULL REFERENCES instance_types(id),
  root_disk_gb  INTEGER NOT NULL,
  desired_size  INTEGER NOT NULL CHECK (desired_size BETWEEN 1 AND 10),
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_spark_node_groups_name ON spark_node_groups(cluster_id, name);

-- Which instances are a cluster's nodes, and in which role. A node row goes
-- with its instance, in one transaction, as a Kubernetes node's does.
CREATE TABLE spark_nodes (
  instance_id   TEXT PRIMARY KEY REFERENCES instances(id),
  cluster_id    TEXT NOT NULL REFERENCES spark_clusters(id) ON DELETE CASCADE,
  node_group_id TEXT REFERENCES spark_node_groups(id) ON DELETE CASCADE,  -- NULL for the master
  role          TEXT NOT NULL CHECK (role IN ('master', 'worker')),
  -- The master belongs to no node group; a worker always does.
  CHECK ((role = 'master') = (node_group_id IS NULL))
);
CREATE INDEX idx_spark_nodes_cluster ON spark_nodes(cluster_id);

-- +goose Down
DROP TABLE spark_nodes;
DROP TABLE spark_node_groups;
DROP TABLE spark_cluster_buckets;
DROP TABLE spark_clusters;
