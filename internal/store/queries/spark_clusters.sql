-- Queries of the Spark module (docs/design/09-spark.md section 3).
--
-- No secret is read or written here: a cluster's Spark secret and its access
-- key's secret live in $KUMO_DATA_DIR/spark/<id>/.

-- The cluster a managed VM is a node of. There is at most one, because
-- spark_nodes is keyed on the instance.
-- name: GetSparkClusterByInstance :one
SELECT c.id, c.name
FROM spark_clusters c
JOIN spark_nodes n ON n.cluster_id = c.id
WHERE n.instance_id = ?1;

-- name: CreateSparkCluster :one
INSERT INTO spark_clusters (
  id, name, spark_version, image_id, vpc_id, subnet_id, master_type,
  state, state_reason, endpoint, object_storage, access_key_id, event_log_bucket,
  created_by, created_at, updated_at
) VALUES (
  ?1, ?2, ?3, ?4, ?5, ?6, ?7,
  ?8, NULL, NULL, ?9, ?10, ?11,
  ?12, ?13, ?13
)
RETURNING *;

-- name: GetSparkCluster :one
SELECT * FROM spark_clusters WHERE id = ?1;

-- name: AddSparkClusterBucket :exec
INSERT INTO spark_cluster_buckets (cluster_id, bucket) VALUES (?1, ?2);

-- name: ListSparkClusterBuckets :many
SELECT bucket FROM spark_cluster_buckets WHERE cluster_id = ?1 ORDER BY bucket;

-- name: CreateSparkNodeGroup :one
INSERT INTO spark_node_groups (
  id, cluster_id, name, instance_type, root_disk_gb, desired_size, created_at, updated_at
) VALUES (
  ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?7
)
RETURNING *;

-- name: ListSparkNodeGroups :many
SELECT * FROM spark_node_groups WHERE cluster_id = ?1 ORDER BY name;

-- name: GetSparkNodeGroup :one
SELECT * FROM spark_node_groups WHERE id = ?1 AND cluster_id = ?2;

-- A scale's new size. The node rows the scale adds or removes change in the
-- same transaction.
-- name: SetSparkNodeGroupSize :one
UPDATE spark_node_groups
SET desired_size = ?2,
    updated_at = ?3
WHERE id = ?1
RETURNING *;

-- name: CreateSparkNode :exec
INSERT INTO spark_nodes (instance_id, cluster_id, node_group_id, role)
VALUES (?1, ?2, ?3, ?4);

-- A cluster's nodes with the instance each one is, the master first.
-- name: ListSparkNodes :many
SELECT n.instance_id, n.node_group_id, n.role,
       i.name, i.state, i.private_ip, i.instance_type
FROM spark_nodes n
JOIN instances i ON i.id = n.instance_id
WHERE n.cluster_id = ?1
ORDER BY n.role, i.name;

-- GET /spark/clusters: newest first, keyset-paged on (created_at, id) like
-- every collection. instr(haystack, '') is 1, so an empty query matches every
-- row, and an empty state_filter matches every state.
-- name: ListSparkClusters :many
SELECT * FROM spark_clusters
WHERE instr(lower(name), lower(sqlc.arg(query))) > 0
  AND (CAST(sqlc.arg(state_filter) AS TEXT) = '' OR state = CAST(sqlc.arg(state_filter) AS TEXT))
  AND (CAST(sqlc.arg(cursor_created) AS TEXT) = ''
       OR created_at < CAST(sqlc.arg(cursor_created) AS TEXT)
       OR (created_at = CAST(sqlc.arg(cursor_created) AS TEXT) AND id < sqlc.arg(cursor_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- The clusters the health pass watches (section 6).
-- name: ListSparkClustersInState :many
SELECT * FROM spark_clusters WHERE state = ?1 ORDER BY created_at;

-- The state and the reason are written together, so a cluster cannot end up
-- failed or degraded with nothing to say about it.
-- name: SetSparkClusterState :one
UPDATE spark_clusters
SET state = ?2,
    state_reason = ?3,
    updated_at = ?4
WHERE id = ?1
RETURNING *;

-- The end of a successful create: the endpoint is published with the state
-- that makes it usable.
-- name: MarkSparkClusterActive :one
UPDATE spark_clusters
SET state = 'active',
    state_reason = NULL,
    endpoint = ?2,
    updated_at = ?3
WHERE id = ?1
RETURNING *;

-- name: DeleteSparkNode :exec
DELETE FROM spark_nodes WHERE instance_id = ?1;

-- The buckets, node groups and node rows cascade; the instances rows are the
-- instance service's to delete, before this, in the same transaction.
-- name: DeleteSparkCluster :exec
DELETE FROM spark_clusters WHERE id = ?1;
