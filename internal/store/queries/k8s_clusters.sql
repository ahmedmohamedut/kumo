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
