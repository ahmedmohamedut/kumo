import Badge from '@cloudscape-design/components/badge'
import Box from '@cloudscape-design/components/box'
import Link from '@cloudscape-design/components/link'
import type { CollectionPreferencesProps, TableProps } from '@cloudscape-design/components'

import type { VPC } from '@/features/vpcs/api'
import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'

export type { VPC }

/**
 * The default column set of the VPCs table: Name, VPC ID, IPv4 CIDR, IPv4
 * gateway, Connectivity, Default, Created.
 *
 * The count columns deliberately live on the detail page and not here: the list
 * endpoint returns plain VPCs, and only the get endpoint can tell how many
 * subnets and instances live inside one — a table that promised those numbers
 * would need one extra request per row.
 */
export const defaultColumns: ReadonlyArray<TableProps.ColumnDefinition<VPC>> = [
  {
    id: 'name',
    header: 'Name',
    sortingField: 'name',
    cell: (item) => <Link href={`/vpcs/${item.id}`}>{item.name}</Link>,
  },
  {
    id: 'id',
    header: 'VPC ID',
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
    id: 'gateway_ip',
    header: 'IPv4 gateway',
    cell: (item) => <Box variant="code">{item.gateway_ip}</Box>,
  },
  {
    id: 'mode',
    header: 'Connectivity',
    sortingField: 'mode',
    cell: (item) => (item.mode === 'nat' ? 'Internet (NAT)' : 'Isolated'),
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
      <span title={formatAbsoluteTime(item.created_at)}>{formatRelativeTime(item.created_at)}</span>
    ),
  },
]

/** columnDisplay starts with every column visible, in column definition order. */
export const columnDisplayPreferences: ReadonlyArray<TableProps.ColumnDisplayProperties> = [
  { id: 'name', visible: true },
  { id: 'id', visible: true },
  { id: 'cidr', visible: true },
  { id: 'gateway_ip', visible: true },
  { id: 'mode', visible: true },
  { id: 'is_default', visible: true },
  { id: 'created_at', visible: true },
]

/**
 * columnOptions is the column list in the preferences panel. The ids must match
 * `columnDisplayPreferences` or toggling a column there does nothing.
 */
export const columnOptions: ReadonlyArray<CollectionPreferencesProps.ContentDisplayOption> = [
  { id: 'name', label: 'Name' },
  { id: 'id', label: 'VPC ID' },
  { id: 'cidr', label: 'IPv4 CIDR' },
  { id: 'gateway_ip', label: 'IPv4 gateway' },
  { id: 'mode', label: 'Connectivity' },
  { id: 'is_default', label: 'Default' },
  { id: 'created_at', label: 'Created' },
]
