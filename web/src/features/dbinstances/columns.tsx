import Box from '@cloudscape-design/components/box'
import Link from '@cloudscape-design/components/link'
import StatusIndicator from '@cloudscape-design/components/status-indicator'
import type { CollectionPreferencesProps, TableProps } from '@cloudscape-design/components'

import type { DBInstance } from '@/features/dbinstances/api'
import { DB_INSTANCE_INDICATOR } from '@/features/dbinstances/state'
import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'

/**
 * The columns of the DB instances table.
 *
 * The endpoint is among them because it is what a client connects to and the
 * one value an operator copies most; it is null until the guest has an
 * address, which reads as a dash rather than as an empty cell.
 */
export const columnDefinitions: ReadonlyArray<TableProps.ColumnDefinition<DBInstance>> = [
  {
    id: 'identifier',
    header: 'Identifier',
    sortingField: 'identifier',
    isRowHeader: true,
    cell: (item) => <Link href={`/databases/${item.id}`}>{item.identifier}</Link>,
  },
  {
    id: 'state',
    header: 'Status',
    sortingField: 'state',
    cell: (item) => (
      <StatusIndicator type={DB_INSTANCE_INDICATOR[item.state].type}>
        {DB_INSTANCE_INDICATOR[item.state].label}
      </StatusIndicator>
    ),
  },
  {
    id: 'engine',
    header: 'Engine',
    sortingField: 'engine',
    cell: (item) => `Postgres ${item.engine_version}`,
  },
  {
    id: 'endpoint',
    header: 'Endpoint',
    sortingComparator: (a, b) => (a.endpoint ?? '').localeCompare(b.endpoint ?? ''),
    cell: (item) =>
      item.endpoint ?? (
        <Box variant="span" color="text-status-inactive">
          —
        </Box>
      ),
  },
  {
    id: 'instance_type',
    header: 'Size',
    sortingField: 'instance_type',
    cell: (item) => item.instance_type,
  },
  {
    id: 'storage_gb',
    header: 'Storage',
    sortingField: 'storage_gb',
    cell: (item) => `${item.storage_gb} GiB`,
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

export const columnDisplay: ReadonlyArray<CollectionPreferencesProps.ContentDisplayItem> =
  columnDefinitions.map((column) => ({ id: column.id ?? '', visible: true }))

export const columnOptions: ReadonlyArray<CollectionPreferencesProps.ContentDisplayOption> =
  columnDefinitions.map((column) => ({
    id: column.id ?? '',
    label: String(column.header),
    alwaysVisible: column.id === 'identifier',
  }))
