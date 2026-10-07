import Badge from '@cloudscape-design/components/badge'
import Box from '@cloudscape-design/components/box'
import Link from '@cloudscape-design/components/link'
import type { CollectionPreferencesProps, TableProps } from '@cloudscape-design/components'

import type { Subnet } from '@/features/subnets/api'
import type { VPC } from '@/features/vpcs/api'
import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'

export type { Subnet }

/**
 * The default column set of the subnets table: Name, Subnet ID, IPv4 CIDR,
 * Connected VPC, IPs available, Default, Created.
 *
 * The subnets list answers plain `Subnet` rows whose only VPC reference is the
 * `vpc_id`, so the connected-VPC cell resolves the name against the page's VPC
 * lookup — the same list the filter dropdown is built from, no extra requests.
 */
export function defaultColumns(
  vpcNames: ReadonlyMap<string, VPC['name']>,
): ReadonlyArray<TableProps.ColumnDefinition<Subnet>> {
  return [
    {
      id: 'name',
      header: 'Name',
      sortingField: 'name',
      cell: (item) => <Link href={`/subnets/${item.id}`}>{item.name}</Link>,
    },
    {
      id: 'id',
      header: 'Subnet ID',
      sortingField: 'id',
      cell: (item) => <Box variant="code">{item.id}</Box>,
    },
    {
      id: 'cidr',
      header: 'IPv4 CIDR',
      sortingField: 'cidr',
      cell: (item) => <Box variant="code">{item.cidr}</Box>,
    },
    {
      id: 'vpc_id',
      header: 'Connected VPC',
      sortingField: 'vpc_id',
      cell: (item) => (
        <Link href={`/vpcs/${item.vpc_id}`}>{vpcNames.get(item.vpc_id) ?? item.vpc_id}</Link>
      ),
    },
    {
      id: 'ips_available',
      header: 'IPs available',
      cell: (item) => availableIps(item).toLocaleString('en-US'),
    },
    {
      id: 'is_default',
      header: 'Default',
      cell: (item) =>
        item.is_default ? (
          <Badge color="green">Default</Badge>
        ) : (
          <Box variant="span" color="text-status-inactive">
            —
          </Box>
        ),
    },
    {
      id: 'created_at',
      header: 'Created',
      sortingField: 'created_at',
      cell: (item) => (
        <span title={formatAbsoluteTime(item.created_at)}>
          {formatRelativeTime(item.created_at)}
        </span>
      ),
    },
  ]
}

/** availableIps is what the "IPs available" cell counts: usable minus allocated. */
export function availableIps(item: Subnet): number {
  return item.usable_ips - item.allocated_ips
}

/** columnDisplay starts with every column visible, in column definition order. */
export const columnDisplayPreferences: ReadonlyArray<TableProps.ColumnDisplayProperties> = [
  { id: 'name', visible: true },
  { id: 'id', visible: true },
  { id: 'cidr', visible: true },
  { id: 'vpc_id', visible: true },
  { id: 'ips_available', visible: true },
  { id: 'is_default', visible: true },
  { id: 'created_at', visible: true },
]

/**
 * columnOptions is the column list in the preferences panel. The ids must match
 * `columnDisplayPreferences` or toggling a column there does nothing.
 */
export const columnOptions: ReadonlyArray<CollectionPreferencesProps.ContentDisplayOption> = [
  { id: 'name', label: 'Name' },
  { id: 'id', label: 'Subnet ID' },
  { id: 'cidr', label: 'IPv4 CIDR' },
  { id: 'vpc_id', label: 'Connected VPC' },
  { id: 'ips_available', label: 'IPs available' },
  { id: 'is_default', label: 'Default' },
  { id: 'created_at', label: 'Created' },
]
