-- The network interfaces table grows with launches (M4b.3); the subnet and VPC
-- delete checks read it now.

-- name: CountInterfacesInSubnet :one
SELECT COUNT(*) FROM network_interfaces WHERE subnet_id = ?1;

-- name: CountSubnetAllocations :many
SELECT subnet_id, COUNT(*) AS allocated
FROM network_interfaces
WHERE subnet_id IN (sqlc.slice('subnet_ids'))
GROUP BY subnet_id;

-- name: CountVPCSubnets :one
SELECT COUNT(*) FROM subnets WHERE vpc_id = ?1;

-- name: CountVPCInstances :one
SELECT COUNT(*) FROM network_interfaces ni
JOIN subnets s ON s.id = ni.subnet_id
WHERE s.vpc_id = ?1;