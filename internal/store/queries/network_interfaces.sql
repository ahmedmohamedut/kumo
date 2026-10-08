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

-- A launch writes the interface in the same transaction as the instance row: a
-- domain that boots without an interface is a domain with no identity on its
-- network, and a lease nobody can match to a row.
-- name: CreateNetworkInterface :one
INSERT INTO network_interfaces (id, instance_id, subnet_id, device_index, mac_address, private_ip, created_at)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)
RETURNING *;

-- The launch and terminate workflows resolve the network behind an instance's
-- interfaces. The join reads the VPC row so the DHCP host can be placed on the
-- right libvirt network and rolled back off it.
-- name: ListInterfacesByInstances :many
SELECT ni.*, s.cidr AS subnet_cidr, s.is_default AS subnet_is_default,
       v.id AS vpc_id, v.cidr AS vpc_cidr, v.gateway_ip, v.libvirt_network
FROM network_interfaces ni
JOIN subnets s ON s.id = ni.subnet_id
JOIN vpcs v ON v.id = s.vpc_id
WHERE ni.instance_id IN (sqlc.slice('instance_ids'))
ORDER BY ni.instance_id, ni.device_index;

-- The addresses a subnet already hands out, so the allocator can offer the next
-- free one. Freed addresses disappear with the interface row that held them.
-- name: ListAllocatedIPsInSubnet :many
SELECT private_ip FROM network_interfaces WHERE subnet_id = ?1;

-- Launches with no explicit subnet attach to the VPC's default subnet, the same
-- one the bootstrap seeds.
-- name: GetDefaultSubnet :one
SELECT * FROM subnets
WHERE is_default = 1
  AND vpc_id = (SELECT id FROM vpcs WHERE is_default = 1)
LIMIT 1;