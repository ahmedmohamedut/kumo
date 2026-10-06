-- +goose Up
CREATE TABLE vpcs (
  id              TEXT PRIMARY KEY,              -- vpc-<17 hex>; also libvirt_network by default
  name            TEXT NOT NULL UNIQUE,          -- immutable; seeds DNS domain
  cidr            TEXT NOT NULL,                 -- RFC 1918, /16 to /28, immutable
  mode            TEXT NOT NULL DEFAULT 'nat' CHECK (mode IN ('nat','isolated')), -- immutable
  gateway_ip      TEXT NOT NULL,                 -- first host IP of VPC CIDR (e.g. 10.0.0.1)
  netmask         TEXT NOT NULL,                 -- e.g. 255.255.0.0
  bridge_name     TEXT NOT NULL UNIQUE,          -- kmb<8 hex> (or virbr-default for adopted)
  libvirt_network TEXT NOT NULL UNIQUE,          -- vpc-<17 hex> (or kumo-default for adopted)
  is_default      INTEGER NOT NULL DEFAULT 0,
  created_by      TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
);

CREATE TABLE subnets (
  id          TEXT PRIMARY KEY,                  -- subnet-<17 hex>
  vpc_id      TEXT NOT NULL REFERENCES vpcs(id), -- parent VPC
  name        TEXT NOT NULL,                     -- unique per VPC, immutable
  cidr        TEXT NOT NULL,                     -- contained within VPC CIDR, /16 to /28, immutable
  is_default  INTEGER NOT NULL DEFAULT 0,
  created_by  TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL,
  UNIQUE (vpc_id, name)
);
CREATE INDEX idx_subnets_vpc ON subnets(vpc_id);

CREATE TABLE network_interfaces (
  id           TEXT PRIMARY KEY,                 -- eni-<17 hex>
  instance_id  TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  subnet_id    TEXT NOT NULL REFERENCES subnets(id),
  device_index INTEGER NOT NULL DEFAULT 0,       -- 0 for eth0, modelled for multi-NIC later
  mac_address  TEXT NOT NULL UNIQUE,             -- 52:54:00:xx:xx:xx
  private_ip   TEXT NOT NULL,                    -- allocated static IP in subnet
  created_at   TEXT NOT NULL,
  UNIQUE (instance_id, device_index)
);
CREATE INDEX idx_network_interfaces_subnet ON network_interfaces(subnet_id);
CREATE UNIQUE INDEX idx_network_interfaces_ip_live ON network_interfaces(subnet_id, private_ip);

-- +goose Down
DROP TABLE network_interfaces;
DROP TABLE subnets;
DROP TABLE vpcs;