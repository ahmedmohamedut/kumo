-- Queries of the Kubernetes module (docs/design/08-kubernetes.md section 3).
--
-- No key is read or written here: a cluster's CA keys live in
-- $KUMO_DATA_DIR/clusters/<id>/, and only the CA certificate, which is public,
-- is a column.

-- The cluster a managed VM is a node of. There is at most one, because
-- k8s_nodes is keyed on the instance.
-- name: GetK8sClusterByInstance :one
SELECT c.id, c.name
FROM k8s_clusters c
JOIN k8s_nodes n ON n.cluster_id = c.id
WHERE n.instance_id = ?1;

-- name: CreateK8sCluster :one
INSERT INTO k8s_clusters (
  id, name, kubernetes_version, image_id, vpc_id, subnet_id,
  pod_cidr, service_cidr, control_plane_count, control_plane_type,
  state, state_reason, endpoint, ca_cert_pem, created_by, created_at, updated_at
) VALUES (
  ?1, ?2, ?3, ?4, ?5, ?6,
  ?7, ?8, ?9, ?10,
  ?11, NULL, NULL, ?12, ?13, ?14, ?14
)
RETURNING *;

-- name: GetK8sCluster :one
SELECT * FROM k8s_clusters WHERE id = ?1;

-- name: CreateK8sNodeGroup :one
INSERT INTO k8s_node_groups (
  id, cluster_id, name, instance_type, root_disk_gb, desired_size, created_at, updated_at
) VALUES (
  ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?7
)
RETURNING *;

-- name: ListK8sNodeGroups :many
SELECT * FROM k8s_node_groups WHERE cluster_id = ?1 ORDER BY name;

-- name: CreateK8sNode :exec
INSERT INTO k8s_nodes (instance_id, cluster_id, node_group_id, role)
VALUES (?1, ?2, ?3, ?4);

-- A cluster's nodes with the instance each one is, control plane first.
-- name: ListK8sNodes :many
SELECT n.instance_id, n.node_group_id, n.role,
       i.name, i.state, i.private_ip, i.instance_type
FROM k8s_nodes n
JOIN instances i ON i.id = n.instance_id
WHERE n.cluster_id = ?1
ORDER BY n.role, i.name;

-- GET /eks/clusters: newest first, keyset-paged on (created_at, id) like
-- every collection. instr(haystack, '') is 1, so an empty query matches every
-- row, and an empty state_filter matches every state.
-- name: ListK8sClusters :many
SELECT * FROM k8s_clusters
WHERE instr(lower(name), lower(sqlc.arg(query))) > 0
  AND (CAST(sqlc.arg(state_filter) AS TEXT) = '' OR state = CAST(sqlc.arg(state_filter) AS TEXT))
  AND (CAST(sqlc.arg(cursor_created) AS TEXT) = ''
       OR created_at < CAST(sqlc.arg(cursor_created) AS TEXT)
       OR (created_at = CAST(sqlc.arg(cursor_created) AS TEXT) AND id < sqlc.arg(cursor_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- The clusters the health pass watches (section 6).
-- name: ListK8sClustersInState :many
SELECT * FROM k8s_clusters WHERE state = ?1 ORDER BY created_at;

-- The state and the reason are written together, so a cluster cannot end up
-- failed or degraded with nothing to say about it.
-- name: SetK8sClusterState :one
UPDATE k8s_clusters
SET state = ?2,
    state_reason = ?3,
    updated_at = ?4
WHERE id = ?1
RETURNING *;

-- The end of a successful create: the endpoint is published with the state
-- that makes it usable.
-- name: MarkK8sClusterActive :one
UPDATE k8s_clusters
SET state = 'active',
    state_reason = NULL,
    endpoint = ?2,
    updated_at = ?3
WHERE id = ?1
RETURNING *;

-- name: DeleteK8sNode :exec
DELETE FROM k8s_nodes WHERE instance_id = ?1;

-- The node groups and the node rows cascade; the instances rows are the
-- instance service's to delete, before this, in the same transaction.
-- name: DeleteK8sCluster :exec
DELETE FROM k8s_clusters WHERE id = ?1;
